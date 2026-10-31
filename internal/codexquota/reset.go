package codexquota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	// resetRecheck bounds how old a reset count may get while scanning is on.
	// Passive headers never carry it, so busy and exhausted accounts need this too.
	resetRecheck = 6 * time.Hour
	// resetDataTTL hides counts once scanning stopped refreshing them.
	resetDataTTL = 2 * resetRecheck
)

var (
	// ErrAccountNotFound means no cached Codex credential has the requested name.
	ErrAccountNotFound = errors.New("codex account not found")
	// ErrNoResetCredit means upstream reports nothing left to redeem.
	ErrNoResetCredit = errors.New("no reset credit available")
	// ErrResetBusy means another reset of the same account is still running.
	ErrResetBusy = errors.New("reset already running for this account")
)

// UpstreamError carries a non-success upstream response.
type UpstreamError struct {
	Status int
}

func (e *UpstreamError) Error() string { return fmt.Sprintf("upstream returned HTTP %d", e.Status) }

// ResetHooks connect the service to credential-bound upstream calls.
type ResetHooks struct {
	Credits       ProbeFunc
	Consume       func(ctx context.Context, authID string) error
	ClearCooldown func(ctx context.Context, authID string) error
}

// ResetStatus is one account's latest reset state.
type ResetStatus struct {
	Name         string    `json:"name"`
	Count        int       `json:"count"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	Remaining    *float64  `json:"remaining"`
	CheckedAt    time.Time `json:"checked_at,omitempty"`
	CooldownKept bool      `json:"cooldown_kept,omitempty"`
}

type usageReading struct {
	windows []Window
	plan    string
	count   *int
	expires time.Time
}

// ParseResetCount reads the redeemable count, or nil when upstream omits it.
func ParseResetCount(body []byte) *int {
	item := gjson.GetBytes(body, "rate_limit_reset_credits.available_count")
	if !item.Exists() || item.Type != gjson.Number {
		return nil
	}
	count := int(item.Int())
	return &count
}

// ParseResetExpiry returns the soonest expiry among redeemable credits.
func ParseResetExpiry(body []byte, now time.Time) time.Time {
	var soonest time.Time
	gjson.GetBytes(body, "credits").ForEach(func(_, credit gjson.Result) bool {
		supported := credit.Get("is_supported_by_plan")
		if credit.Get("status").String() != "available" || (supported.Exists() && !supported.Bool()) {
			return true
		}
		expires, errParse := time.Parse(time.RFC3339Nano, credit.Get("expires_at").String())
		if errParse == nil && expires.After(now) && (soonest.IsZero() || expires.Before(soonest)) {
			soonest = expires.UTC()
		}
		return true
	})
	return soonest
}

func (a *Account) resetKnown(now time.Time) bool {
	if a.ResetCount == nil || now.Sub(a.ResetCheckedAt) > resetDataTTL {
		return false
	}
	return a.ResetExpiresAt.IsZero() || a.ResetExpiresAt.After(now)
}

// resetSources picks the freshest reset reading per account, since files of one
// account share their credits but only one of them gets scanned.
func resetSources(accounts map[string]*Account, now time.Time) map[string]*Account {
	sources := map[string]*Account{}
	for _, account := range accounts {
		if account.Disabled || account.isInvalid() || !account.resetKnown(now) {
			continue
		}
		if current, ok := sources[account.key()]; !ok || account.ResetCheckedAt.After(current.ResetCheckedAt) {
			sources[account.key()] = account
		}
	}
	return sources
}

// SetResetHooks must be called before Start.
func (s *Service) SetResetHooks(hooks ResetHooks) { s.hooks = hooks }

func (s *Service) readUsage(ctx context.Context, id string) (usageReading, int, error) {
	status, body, errProbe := s.probe(ctx, id)
	if errProbe != nil {
		return usageReading{}, status, errProbe
	}
	if status != http.StatusOK {
		return usageReading{}, status, &UpstreamError{Status: status}
	}
	now := s.now()
	windows, plan, errParse := ParseUsage(body, now)
	if errParse != nil {
		return usageReading{}, status, errParse
	}
	reading := usageReading{windows: windows, plan: plan, count: ParseResetCount(body)}
	if reading.count != nil && *reading.count > 0 && s.hooks.Credits != nil {
		creditStatus, credits, errCredits := s.hooks.Credits(ctx, id)
		if errCredits == nil && creditStatus == http.StatusOK {
			reading.expires = ParseResetExpiry(credits, now)
		} else {
			log.WithField("status", creditStatus).Debug("codex quota: reset expiry unavailable")
		}
	}
	return reading, status, nil
}

func (s *Service) applyReadingLocked(account *Account, reading usageReading, now time.Time) {
	// A response that raced ahead of this probe keeps the newer passive data.
	if now.After(account.ObservedAt) {
		s.applyLocked(account, reading.windows, reading.plan, now, sourceScan)
	}
	account.ResetCount = reading.count
	account.ResetExpiresAt = reading.expires
	account.ResetCheckedAt = now
}

func (s *Service) lookup(name string) (id, key string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, account := range s.accounts {
		if account.Name == name {
			return account.ID, account.key(), true
		}
	}
	return "", "", false
}

func (s *Service) statusLocked(account *Account, now time.Time) ResetStatus {
	status := ResetStatus{Name: account.Name, ExpiresAt: account.ResetExpiresAt, CheckedAt: account.ResetCheckedAt}
	if account.ResetCount != nil {
		status.Count = *account.ResetCount
	}
	if len(account.Windows) > 0 {
		remaining, _ := remainingOf(account.Windows, now)
		status.Remaining = &remaining
	}
	return status
}

func (s *Service) refreshAccount(ctx context.Context, id string) (ResetStatus, error) {
	reading, status, errRead := s.readUsage(ctx, id)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	account := s.accounts[id]
	if account == nil {
		return ResetStatus{}, ErrAccountNotFound
	}
	account.ScanAt = now
	if errRead != nil {
		account.ScanDenied = account.ScanDenied || status == http.StatusUnauthorized
		return ResetStatus{}, errRead
	}
	s.applyReadingLocked(account, reading, now)
	s.saveAccountsLocked()
	s.notify()
	return s.statusLocked(account, now), nil
}

// CheckReset reads the live reset state of one account.
func (s *Service) CheckReset(ctx context.Context, name string) (ResetStatus, error) {
	id, _, ok := s.lookup(name)
	if !ok {
		return ResetStatus{}, ErrAccountNotFound
	}
	return s.refreshAccount(ctx, id)
}

// Reset redeems one credit after re-reading the live count, then clears local
// cooldown on every file of the account so it can serve straight away.
func (s *Service) Reset(ctx context.Context, name string) (ResetStatus, error) {
	id, key, ok := s.lookup(name)
	if !ok {
		return ResetStatus{}, ErrAccountNotFound
	}
	if s.hooks.Consume == nil {
		return ResetStatus{}, errors.New("codex reset is not available")
	}
	s.mu.Lock()
	if _, busy := s.resetting[key]; busy {
		s.mu.Unlock()
		return ResetStatus{}, ErrResetBusy
	}
	s.resetting[key] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.resetting, key)
		s.mu.Unlock()
	}()

	before, errCheck := s.refreshAccount(ctx, id)
	if errCheck != nil {
		return ResetStatus{}, errCheck
	}
	if before.Count <= 0 {
		return before, ErrNoResetCredit
	}
	if errConsume := s.hooks.Consume(ctx, id); errConsume != nil {
		// The credit may have been spent if the failure came after sending.
		if _, errRefresh := s.refreshAccount(ctx, id); errRefresh != nil {
			log.WithError(errRefresh).Warn("codex quota: refresh after failed reset failed")
		}
		return before, errConsume
	}
	cooldownKept := s.clearCooldowns(ctx, key)
	s.mu.Lock()
	if account := s.accounts[id]; account != nil && account.ResetCount != nil {
		*account.ResetCount = max(0, *account.ResetCount-1)
	}
	s.mu.Unlock()
	after, errRefresh := s.refreshAccount(ctx, id)
	if errRefresh != nil {
		log.WithError(errRefresh).Warn("codex quota: refresh after reset failed")
		s.mu.Lock()
		if account := s.accounts[id]; account != nil {
			after = s.statusLocked(account, s.now())
		}
		s.mu.Unlock()
	}
	after.CooldownKept = cooldownKept
	return after, nil
}

func (s *Service) clearCooldowns(ctx context.Context, key string) (kept bool) {
	if s.hooks.ClearCooldown == nil {
		return false
	}
	s.mu.Lock()
	ids := []string{}
	for _, account := range s.accounts {
		if account.key() == key {
			ids = append(ids, account.ID)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		if errClear := s.hooks.ClearCooldown(ctx, id); errClear != nil {
			log.WithError(errClear).Warn("codex quota: clear cooldown after reset failed")
			kept = true
		}
	}
	return kept
}

func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
