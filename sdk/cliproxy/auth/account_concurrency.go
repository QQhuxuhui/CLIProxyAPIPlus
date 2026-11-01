package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const (
	concurrencyLimitExceededReason = "concurrency_limit_exceeded"
	accountConcurrencySampleEvery  = 5 * time.Second
	accountConcurrencyPointCount   = 60
	// CodexProbeConcurrencyFull is the AcquireCodexProbeAuth reason for a probe that was not run
	// because every otherwise usable credential is at its concurrency limit.
	CodexProbeConcurrencyFull = "all codex oauth credentials at concurrency limit"
)

// CodexConcurrencyPoint is one minute of the global account concurrency series. in_flight,
// capacity, full accounts and unlimited accounts come from the minute's last 5-second sample;
// the peak is the highest sample, not the true peak.
type CodexConcurrencyPoint struct {
	Time                    time.Time `json:"time"`
	InFlight                int       `json:"in_flight"`
	InFlightPeak            int       `json:"in_flight_peak"`
	ConcurrencyCapacity     int       `json:"concurrency_capacity"`
	ConcurrencyFullAccounts int       `json:"concurrency_full_accounts"`
	ConcurrencyRejections   int       `json:"concurrency_rejections"`
	ConcurrencyUnlimited    int       `json:"concurrency_unlimited"`
}

type accountConcurrencyLease struct {
	owner      *accountConcurrency
	id         string
	applicable bool
	released   atomic.Bool
}

// release is idempotent and safe on a nil lease.
func (l *accountConcurrencyLease) release() {
	if l == nil || !l.released.CompareAndSwap(false, true) {
		return
	}
	l.owner.mu.Lock()
	defer l.owner.mu.Unlock()
	if remaining := l.owner.counts[l.id] - 1; remaining > 0 {
		l.owner.counts[l.id] = remaining
	} else {
		delete(l.owner.counts, l.id)
	}
	if l.applicable {
		l.owner.applicableTotal--
	}
}

// accountConcurrency counts in-flight work per stable account ID, independent of pool shards.
type accountConcurrency struct {
	configured   atomic.Bool
	active       atomic.Bool
	defaultLimit atomic.Int64
	store        *accountConcurrencyStore
	nowFunc      func() time.Time

	mu              sync.RWMutex
	counts          map[string]int
	applicableTotal int

	seriesMu sync.Mutex
	points   [accountConcurrencyPointCount]CodexConcurrencyPoint
}

func newAccountConcurrency() *accountConcurrency {
	c := &accountConcurrency{store: newAccountConcurrencyStore(), nowFunc: time.Now, counts: make(map[string]int)}
	c.defaultLimit.Store(internalconfig.DefaultCodexAccountConcurrencyLimit)
	return c
}

// configure applies the effective switch: enabled && !home.
func (c *accountConcurrency) configure(cfg internalconfig.CodexAccountConcurrencyConfig, home bool) {
	limit := cfg.EffectiveDefaultLimit()
	if cfg.Validate() != nil {
		limit = internalconfig.DefaultCodexAccountConcurrencyLimit
	}
	c.defaultLimit.Store(int64(limit))
	c.configured.Store(cfg.Enabled)
	c.active.Store(cfg.Enabled && !home)
}

// accountConcurrencyApplicable limits the feature to Codex OAuth accounts.
func accountConcurrencyApplicable(a *Auth) bool {
	return a != nil && a.Provider == "codex" && a.AuthKind() == AuthKindOAuth
}

// CodexAccountConcurrencyApplicable reports whether the concurrency limit applies to a.
func CodexAccountConcurrencyApplicable(a *Auth) bool { return accountConcurrencyApplicable(a) }

func (c *accountConcurrency) limitFor(id string) int {
	if limit, ok := c.store.get(id); ok {
		return limit
	}
	return int(c.defaultLimit.Load())
}

// tryAcquire atomically compares and increments. Inactive, inapplicable and unlimited accounts
// only count. It is the guarantee; candidate filtering is an optimization.
func (c *accountConcurrency) tryAcquire(a *Auth) (*accountConcurrencyLease, bool) {
	if c == nil || a == nil {
		return nil, true
	}
	applicable := accountConcurrencyApplicable(a)
	limit := 0
	if applicable && c.active.Load() {
		limit = c.limitFor(a.ID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if limit > 0 && c.counts[a.ID] >= limit {
		return nil, false
	}
	c.counts[a.ID]++
	if applicable {
		c.applicableTotal++
	}
	return &accountConcurrencyLease{owner: c, id: a.ID, applicable: applicable}, true
}

// full reports whether a is currently at an enforced limit.
func (c *accountConcurrency) full(a *Auth) bool {
	if c == nil || !c.active.Load() || !accountConcurrencyApplicable(a) {
		return false
	}
	limit := c.limitFor(a.ID)
	if limit <= 0 {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.counts[a.ID] >= limit
}

type concurrencyExemptKey struct{}

// WithConcurrencyExempt keeps selection from filtering on account concurrency. It serves Codex
// Live media sessions, which hold an account for a call and are not counted.
func WithConcurrencyExempt(ctx context.Context) context.Context {
	return context.WithValue(nonNilContext(ctx), concurrencyExemptKey{}, true)
}

func concurrencyFilterActive(ctx context.Context, c *accountConcurrency) bool {
	if c == nil || !c.active.Load() {
		return false
	}
	return ctx == nil || ctx.Value(concurrencyExemptKey{}) != true
}

func newConcurrencyCapacityError() *poolCapacityError {
	err := newPoolCapacityError(time.Second)
	err.reason = concurrencyLimitExceededReason
	return err
}

func isConcurrencyCapacityError(err error) bool {
	var capacity *poolCapacityError
	return errors.As(err, &capacity) && capacity.reason == concurrencyLimitExceededReason
}

// concurrencyPickError returns the capacity error that a failed pick must surface: its own, or
// the one from an account lost to a race earlier in this request. Cancellation stays as it is.
func concurrencyPickError(errPick, busy error) error {
	if isConcurrencyCapacityError(errPick) {
		return errPick
	}
	if busy == nil || errors.Is(errPick, context.Canceled) || errors.Is(errPick, context.DeadlineExceeded) {
		return nil
	}
	return busy
}

// accountConcurrencyFull reports whether selection must skip a for this request.
func (m *Manager) accountConcurrencyFull(ctx context.Context, a *Auth) bool {
	return concurrencyFilterActive(ctx, m.accountConcurrency) && m.accountConcurrency.full(a)
}

// filterConcurrencyCandidates drops full accounts. Blocked candidates stay for the later checks
// to classify, but only a ready candidate can spare the request from the capacity error.
func (m *Manager) filterConcurrencyCandidates(ctx context.Context, selector Selector, auths []*Auth, model string, now time.Time) ([]*Auth, error) {
	if !concurrencyFilterActive(ctx, m.accountConcurrency) {
		return auths, nil
	}
	requirePositiveWeight := rateCapRequiresPositiveWeight(selector)
	out := make([]*Auth, 0, len(auths))
	ready, sawFull := false, false
	for _, a := range auths {
		if requirePositiveWeight && authWeight(a) <= 0 {
			out = append(out, a)
			continue
		}
		if blocked, _, _ := isAuthBlockedForModel(a, m.selectionModelForAuth(a, model), now); blocked {
			out = append(out, a)
			continue
		}
		if m.accountConcurrency.full(a) {
			sawFull = true
			continue
		}
		ready = true
		out = append(out, a)
	}
	if !sawFull {
		return auths, nil
	}
	if !ready {
		return nil, newConcurrencyCapacityError()
	}
	return out, nil
}

// concurrencyPredicateLocked is the scheduler counterpart of filterConcurrencyCandidates. It is
// independent of the rate cap, so neither a disabled cap nor a rate bypass skips it.
func (s *authScheduler) concurrencyPredicateLocked(ctx context.Context, providers []string, model string, base func(*scheduledAuth) bool) (func(*scheduledAuth) bool, error) {
	if !concurrencyFilterActive(ctx, s.accountConcurrency) {
		return base, nil
	}
	full := make(map[string]struct{})
	ready := false
	now := time.Now()
	for _, provider := range providers {
		state := s.providers[provider]
		if state == nil {
			continue
		}
		shard := state.ensureModelLocked(model, now)
		if shard == nil {
			continue
		}
		shard.promoteExpiredLocked(now)
		for _, entry := range shard.entries {
			if entry == nil || entry.auth == nil || entry.state != scheduledStateReady || (base != nil && !base(entry)) {
				continue
			}
			if s.accountConcurrency.full(entry.auth) {
				full[entry.auth.ID] = struct{}{}
			} else {
				ready = true
			}
		}
	}
	if len(full) == 0 {
		return base, nil
	}
	if !ready {
		return nil, newConcurrencyCapacityError()
	}
	return func(entry *scheduledAuth) bool {
		if entry == nil || entry.auth == nil {
			return false
		}
		if _, skip := full[entry.auth.ID]; skip {
			return false
		}
		return base == nil || base(entry)
	}, nil
}

// recordConcurrencyRejection counts one request whose final error is the concurrency 529.
func (m *Manager) recordConcurrencyRejection(err error) {
	if m != nil && m.accountConcurrency != nil && isConcurrencyCapacityError(err) {
		m.accountConcurrency.recordRejection()
	}
}

func (c *accountConcurrency) bucketLocked(now time.Time) *CodexConcurrencyPoint {
	minute := now.Truncate(time.Minute)
	index := (minute.Unix() / 60) % accountConcurrencyPointCount
	if index < 0 {
		index += accountConcurrencyPointCount
	}
	point := &c.points[index]
	if !point.Time.Equal(minute) {
		*point = CodexConcurrencyPoint{Time: minute}
	}
	return point
}

func (c *accountConcurrency) recordRejection() {
	c.seriesMu.Lock()
	defer c.seriesMu.Unlock()
	c.bucketLocked(c.nowFunc()).ConcurrencyRejections++
}

func (c *accountConcurrency) recordSample(totals accountConcurrencyTotals) {
	c.seriesMu.Lock()
	defer c.seriesMu.Unlock()
	point := c.bucketLocked(c.nowFunc())
	point.InFlight = totals.inFlight
	point.InFlightPeak = max(point.InFlightPeak, totals.inFlight)
	point.ConcurrencyCapacity = totals.capacity
	point.ConcurrencyFullAccounts = totals.full
	point.ConcurrencyUnlimited = totals.unlimited
}

// series returns the last 60 minutes, oldest first, with empty minutes filled in.
func (c *accountConcurrency) series() []CodexConcurrencyPoint {
	c.seriesMu.Lock()
	defer c.seriesMu.Unlock()
	now := c.nowFunc().Truncate(time.Minute)
	out := make([]CodexConcurrencyPoint, 0, accountConcurrencyPointCount)
	for i := accountConcurrencyPointCount - 1; i >= 0; i-- {
		minute := now.Add(-time.Duration(i) * time.Minute)
		index := (minute.Unix() / 60) % accountConcurrencyPointCount
		if index < 0 {
			index += accountConcurrencyPointCount
		}
		point := c.points[index]
		if !point.Time.Equal(minute) {
			point = CodexConcurrencyPoint{Time: minute}
		}
		out = append(out, point)
	}
	return out
}

// accountConcurrencyTotals aggregates schedulable accounts; capacity excludes unlimited accounts.
type accountConcurrencyTotals struct {
	inFlight, capacity, full, remaining, unlimited int
}

// CodexAccountConcurrencyView is one consistent read of counts, limits and the default.
type CodexAccountConcurrencyView struct {
	enabled      bool
	defaultLimit int
	overrides    map[string]int
	counts       map[string]int
	total        int
}

// CodexAccountConcurrencyEntry is the view of one applicable account.
type CodexAccountConcurrencyEntry struct {
	InFlight int
	Limit    int
	// Override is nil when the account inherits the default.
	Override *int
	Source   string
}

// CodexAccountConcurrencyView reads a snapshot; lookups on it are O(1).
func (m *Manager) CodexAccountConcurrencyView() CodexAccountConcurrencyView {
	if m == nil || m.accountConcurrency == nil {
		return CodexAccountConcurrencyView{}
	}
	return m.accountConcurrency.view()
}

func (c *accountConcurrency) view() CodexAccountConcurrencyView {
	v := CodexAccountConcurrencyView{enabled: c.active.Load(), defaultLimit: int(c.defaultLimit.Load()), overrides: c.store.snapshot()}
	c.mu.RLock()
	defer c.mu.RUnlock()
	v.counts = make(map[string]int, len(c.counts))
	for id, count := range c.counts {
		v.counts[id] = count
	}
	v.total = c.applicableTotal
	return v
}

// Enabled reports the effective switch.
func (v CodexAccountConcurrencyView) Enabled() bool { return v.enabled }

// Entry returns the state of an applicable account.
func (v CodexAccountConcurrencyView) Entry(a *Auth) (CodexAccountConcurrencyEntry, bool) {
	if !accountConcurrencyApplicable(a) {
		return CodexAccountConcurrencyEntry{}, false
	}
	entry := CodexAccountConcurrencyEntry{InFlight: v.counts[a.ID], Limit: v.defaultLimit, Source: "default"}
	if override, ok := v.overrides[a.ID]; ok {
		entry.Limit, entry.Source = override, "override"
		entry.Override = &override
	}
	return entry, true
}

func (v CodexAccountConcurrencyView) add(totals *accountConcurrencyTotals, a *Auth, now time.Time) {
	entry, ok := v.Entry(a)
	if !ok || a.Disabled || a.Status == StatusDisabled {
		return
	}
	if expiry, hasExpiry := a.AccessTokenExpirationTime(); hasExpiry && !expiry.After(now) {
		return
	}
	if blocked, _, _ := isAuthBlockedForModel(a, "", now); blocked {
		return
	}
	if entry.Limit <= 0 {
		totals.unlimited++
		return
	}
	totals.capacity += entry.Limit
	if entry.InFlight >= entry.Limit {
		totals.full++
	} else {
		totals.remaining += entry.Limit - entry.InFlight
	}
}

// codexConcurrencyTotals scans every Codex account exactly once, including an account seen in
// two pools during a migration. Home owns the pool, so a local estimate would mislead.
func (m *Manager) codexConcurrencyTotals() accountConcurrencyTotals {
	view := m.accountConcurrency.view()
	if m.HomeEnabled() {
		return accountConcurrencyTotals{inFlight: view.total}
	}
	totals := accountConcurrencyTotals{inFlight: view.total}
	now := time.Now()
	seen := make(map[string]struct{})
	visit := func(auths map[string]*Auth) {
		for id, a := range auths {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			view.add(&totals, a, now)
		}
	}
	if pools := m.codexPools.Load(); pools != nil {
		pools.all.Range(func(_, value any) bool {
			p := value.(*codexAccountPool)
			p.lock()
			visit(p.auths)
			p.mu.Unlock()
			return true
		})
	}
	m.mu.RLock()
	visit(m.auths)
	m.mu.RUnlock()
	return totals
}

func (m *Manager) fillCodexConcurrencySnapshot(out *CodexPoolSnapshot) {
	totals := m.codexConcurrencyTotals()
	out.ConcurrencyEnabled = m.accountConcurrency.active.Load()
	out.InFlight = totals.inFlight
	out.ConcurrencyCapacity = totals.capacity
	out.ConcurrencyFullAccounts = totals.full
	out.ConcurrencyRemaining = totals.remaining
	out.UnlimitedAccounts = totals.unlimited
	out.ConcurrencyPoints = m.accountConcurrency.series()
}

// sampleCodexConcurrency records one global sample for the concurrency curve.
func (m *Manager) sampleCodexConcurrency() {
	m.accountConcurrency.recordSample(m.codexConcurrencyTotals())
}

// startCodexConcurrencySampler samples on one global clock rather than per pool, which keeps the
// instantaneous values out of the per-pool minute sums.
func (m *Manager) startCodexConcurrencySampler(ctx context.Context) {
	m.sampleCodexConcurrency()
	go func() {
		ticker := time.NewTicker(accountConcurrencySampleEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.sampleCodexConcurrency()
			}
		}
	}()
}

// CodexAccountConcurrencyLimitResult lists which requested IDs changed.
type CodexAccountConcurrencyLimitResult struct {
	Changed   []string
	Unchanged []string
}

// SetCodexAccountConcurrencyLimits writes the limit for all ids in one file write; a nil limit
// restores inheritance. On error neither memory nor the file changes. It never touches account
// files, Update, persistence or Generation.
func (m *Manager) SetCodexAccountConcurrencyLimits(ids []string, limit *int) (CodexAccountConcurrencyLimitResult, error) {
	var result CodexAccountConcurrencyLimitResult
	if m == nil || m.accountConcurrency == nil {
		return result, errors.New("auth manager unavailable")
	}
	err := m.accountConcurrency.store.update(func(limits map[string]int) bool {
		result = CodexAccountConcurrencyLimitResult{}
		for _, id := range ids {
			current, ok := limits[id]
			if (limit == nil && !ok) || (limit != nil && ok && current == *limit) {
				result.Unchanged = append(result.Unchanged, id)
				continue
			}
			if limit == nil {
				delete(limits, id)
			} else {
				limits[id] = *limit
			}
			result.Changed = append(result.Changed, id)
		}
		return len(result.Changed) > 0
	})
	if err != nil {
		return CodexAccountConcurrencyLimitResult{}, err
	}
	return result, nil
}

// ForgetCodexAccountConcurrencyLimits removes the records of deleted accounts. Failure only logs.
func (m *Manager) ForgetCodexAccountConcurrencyLimits(ids []string) {
	if m == nil || m.accountConcurrency == nil || len(ids) == 0 {
		return
	}
	if _, err := m.SetCodexAccountConcurrencyLimits(ids, nil); err != nil {
		log.WithError(err).Warn("codex account concurrency: failed to clean up deleted accounts")
	}
}

// configureAccountConcurrency applies the runtime config; the limits file loads once per auth dir.
func (m *Manager) configureAccountConcurrency(cfg *internalconfig.Config) {
	m.accountConcurrency.configure(cfg.Codex.AccountConcurrency, cfg.Home.Enabled)
	if strings.TrimSpace(cfg.AuthDir) == "" {
		return
	}
	authDir, err := util.ResolveAuthDir(cfg.AuthDir)
	if err != nil {
		log.WithError(err).Error("codex account concurrency: cannot resolve auth directory")
		return
	}
	m.accountConcurrency.store.open(authDir)
}

// AcquireAuthWithCredentialPolicy selects like SelectAuthWithCredentialPolicy and reserves the
// selected account for one direct upstream request. The caller must call release exactly when
// that request ends; release is idempotent.
func (m *Manager) AcquireAuthWithCredentialPolicy(ctx context.Context, provider, model, policy string, opts cliproxyexecutor.Options) (*Auth, func(), error) {
	tried := make(map[string]struct{})
	var busy error
	for {
		selected, err := m.selectAuthWithCredentialPolicy(ctx, provider, model, policy, opts, tried)
		if err != nil {
			if capacity := concurrencyPickError(err, busy); capacity != nil {
				err = capacity
			}
			m.recordConcurrencyRejection(err)
			return nil, func() {}, err
		}
		lease, ok := m.accountConcurrency.tryAcquire(selected)
		if !ok {
			tried[selected.ID] = struct{}{}
			busy = newConcurrencyCapacityError()
			continue
		}
		return selected, lease.release, nil
	}
}
