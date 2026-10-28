package auth

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type rateAttempt struct {
	id       uint64
	at       time.Time
	rejected bool
}
type rateAccount struct {
	attempts []rateAttempt
	limit    int
	probeAt  time.Time
	// peak is the highest accepted count seen since probeAt.
	peak int
}
type requestRateCap struct {
	pooled   bool
	mu       sync.Mutex
	nowFunc  func() time.Time
	config   internalconfig.RequestRateCapConfig
	home     bool
	accounts map[string]*rateAccount
	sequence uint64
	metrics  poolMetrics
	// probeInterval caches config.ProbeInterval so selection scans do not re-parse it per account.
	probeInterval time.Duration
}

func newRequestRateCap() *requestRateCap {
	r := &requestRateCap{nowFunc: time.Now, accounts: make(map[string]*rateAccount)}
	r.configure(internalconfig.RequestRateCapConfig{})
	return r
}
func (r *requestRateCap) configure(c internalconfig.RequestRateCapConfig) {
	c = c.WithDefaults()
	if c.Validate() != nil {
		c = (internalconfig.RequestRateCapConfig{}).WithDefaults()
	}
	interval, _ := time.ParseDuration(c.ProbeInterval)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.config = c
	r.probeInterval = interval
	for _, a := range r.accounts {
		a.limit = max(c.MinPerMinute, min(c.MaxPerMinute, a.limit))
	}
}
func (r *requestRateCap) enabled() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.config.Enabled && !r.home
}
func (r *requestRateCap) getLocked(id string, now time.Time) *rateAccount {
	a := r.accounts[id]
	if a == nil {
		a = &rateAccount{limit: r.config.InitialPerMinute, probeAt: now}
		r.accounts[id] = a
	}
	first := 0
	for first < len(a.attempts) && !a.attempts[first].at.After(now.Add(-time.Minute)) {
		first++
	}
	if first > 0 {
		copy(a.attempts, a.attempts[first:])
		a.attempts = a.attempts[:len(a.attempts)-first]
	}
	// Once per probe interval, recover toward the initial limit unconditionally, but probe
	// above it only when the account actually reached its limit; otherwise idle accounts
	// drift to the maximum and the pool capacity is overstated.
	if now.Sub(a.probeAt) >= r.probeInterval {
		effective := max(1, int(math.Floor(float64(a.limit)*r.config.Headroom)))
		if a.limit < r.config.InitialPerMinute || a.peak >= effective {
			a.limit = min(r.config.MaxPerMinute, a.limit+1)
		}
		a.peak = 0
		a.probeAt = now
	}
	return a
}
func (r *requestRateCap) accountLocked(id string, now time.Time) (int, int, time.Time) {
	a := r.getLocked(id, now)
	limit := max(1, int(math.Floor(float64(a.limit)*r.config.Headroom)))
	count := 0
	for _, p := range a.attempts {
		if !p.rejected {
			count++
		}
	}
	var next time.Time
	if count >= limit {
		skip := count - limit
		for _, p := range a.attempts {
			if !p.rejected {
				if skip == 0 {
					next = p.at.Add(time.Minute)
					break
				}
				skip--
			}
		}
	}
	return count, limit, next
}
func (r *requestRateCap) account(id string) (int, int, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accountLocked(id, r.nowFunc())
}
func (r *requestRateCap) start(id string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startLocked(id, r.nowFunc())
}
func (r *requestRateCap) startLocked(id string, now time.Time) uint64 {
	a := r.getLocked(id, now)
	if r.pooled {
		r.sequence = codexPoolRateSequence.Add(1)
	} else {
		r.sequence++
	}
	a.attempts = append(a.attempts, rateAttempt{id: r.sequence, at: now})
	accepted := 0
	for _, p := range a.attempts {
		if !p.rejected {
			accepted++
		}
	}
	a.peak = max(a.peak, accepted)
	r.metrics.bucket(now).Attempts++
	return r.sequence
}
func (r *requestRateCap) reject(id string, token uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rejectLocked(id, token)
}
func (r *requestRateCap) rejectLocked(id string, token uint64) {
	now := r.nowFunc()
	a := r.getLocked(id, now)
	found := false
	for i := range a.attempts {
		if a.attempts[i].id == token && !a.attempts[i].rejected {
			a.attempts[i].rejected = true
			found = true
			break
		}
	}
	if !found {
		return
	}
	count, _, _ := r.accountLocked(id, now)
	a.limit = max(r.config.MinPerMinute, min(r.config.MaxPerMinute, count-1))
	a.peak = 0
	a.probeAt = now
}

type rateAttemptContextKey struct{}
type rateBypassContextKey struct{}
type rateAttemptRef struct {
	epoch  uint64
	authID string
	token  atomic.Uint64
}

func rateBypassed(ctx context.Context) bool {
	return ctx != nil && ctx.Value(rateBypassContextKey{}) == true
}

// beginRateAttempt admits and counts one executor attempt, including internal retries.
func (m *Manager) beginRateAttempt(ctx context.Context, a *Auth) (context.Context, error) {
	if a == nil || a.Provider != "codex" || m.HomeEnabled() || rateBypassed(ctx) {
		return ctx, nil
	}
	if m.codexPools.Load() != nil {
		return m.beginPooledRateAttempt(ctx, a)
	}
	r := m.rateCap
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.nowFunc()
	count, limit, next := r.accountLocked(a.ID, now)
	if r.config.Enabled && r.config.FailFast && count >= limit {
		return ctx, newPoolCapacityError(next.Sub(now))
	}
	token := r.startLocked(a.ID, now)
	ref := &rateAttemptRef{authID: a.ID}
	ref.token.Store(token)
	ctx = context.WithValue(ctx, rateAttemptContextKey{}, ref)
	ctx = cliproxyexecutor.WithUpstreamRequestRetryObserver(ctx, func() { ref.token.Store(r.start(a.ID)) })
	return ctx, nil
}
func (m *Manager) observeRateResult(ctx context.Context, result Result) {
	if m.rateCap == nil || ctx == nil || result.Provider != "codex" || result.QuotaDetail == nil || result.QuotaDetail.ReasonCode != "RATE_LIMIT_EXCEEDED" || statusCodeFromResult(result.Error) != 429 {
		return
	}
	if m.codexPools.Load() != nil {
		m.observePooledRateResult(ctx, result)
		return
	}
	if ref, ok := ctx.Value(rateAttemptContextKey{}).(*rateAttemptRef); ok && ref.authID == result.AuthID {
		m.rateCap.reject(ref.authID, ref.token.Load())
	}
}

// poolCapacityError retains auth.Error compatibility while emitting the public API envelope.
type poolCapacityError struct {
	base  *Error
	delay time.Duration
}

func newPoolCapacityError(delay time.Duration) *poolCapacityError {
	return &poolCapacityError{base: &Error{Code: "pool_at_capacity", Message: "codex pool at capacity, retry later", HTTPStatus: 529}, delay: max(time.Second, delay)}
}
func (e *poolCapacityError) Error() string {
	return `{"error":{"type":"upstream_pool_busy","code":"pool_at_capacity","message":"codex pool at capacity, retry later"}}`
}
func (e *poolCapacityError) Headers() http.Header {
	return http.Header{"Retry-After": {strconv.Itoa(int(math.Ceil(e.delay.Seconds())))}}
}
func (e *poolCapacityError) RetryAfter() *time.Duration { d := e.delay; return &d }
func isPoolCapacityError(err error) bool                { var e *poolCapacityError; return errors.As(err, &e) }

// rateCandidate contains only request-specific eligibility, never account secrets.
type rateCandidate struct {
	auth     *Auth
	blocked  bool
	cooldown bool
	next     time.Time
}

func (r *requestRateCap) filter(candidates []rateCandidate) (map[string]bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.config.Enabled || r.home {
		return nil, nil
	}
	now := r.nowFunc()
	allowed := make(map[string]bool)
	ready := false
	capacityBlocked := false
	var earliest time.Time
	for _, c := range candidates {
		if c.auth.Provider != "codex" {
			allowed[c.auth.ID] = true
			if !c.blocked {
				ready = true
			}
			continue
		}
		count, limit, next := r.accountLocked(c.auth.ID, now)
		if count < limit {
			allowed[c.auth.ID] = true
			if !c.blocked {
				ready = true
			}
		}
		if c.blocked && !c.cooldown {
			continue
		}
		if count >= limit || c.cooldown {
			capacityBlocked = true
			if c.next.After(next) {
				next = c.next
			}
			if earliest.IsZero() || next.Before(earliest) {
				earliest = next
			}
		}
	}
	if ready {
		return allowed, nil
	}
	if r.config.FailFast && capacityBlocked {
		return nil, newPoolCapacityError(earliest.Sub(now))
	}
	return nil, nil
}

// rateCapRequiresPositiveWeight mirrors built-in and affinity fallback eligibility.
func rateCapRequiresPositiveWeight(selector Selector) bool {
	if affinity, ok := selector.(*SessionAffinitySelector); ok && affinity != nil {
		selector = affinity.fallback
	}
	_, weighted := selector.(*WeightedRoundRobinSelector)
	return weighted
}

func (m *Manager) filterRateCandidates(ctx context.Context, selector Selector, auths []*Auth, model string, now time.Time) ([]*Auth, error) {
	if rateBypassed(ctx) || !m.rateCap.enabled() {
		return auths, nil
	}
	candidates := make([]rateCandidate, 0, len(auths))
	requirePositiveWeight := rateCapRequiresPositiveWeight(selector)
	for _, a := range auths {
		if requirePositiveWeight && authWeight(a) <= 0 {
			continue
		}
		blocked, reason, next := isAuthBlockedForModel(a, m.selectionModelForAuth(a, model), now)
		candidates = append(candidates, rateCandidate{a, blocked, (reason == blockReasonCooldown || (reason != blockReasonDisabled && next.After(now))), next})
	}
	allowed, err := m.rateCap.filter(candidates)
	if err != nil || allowed == nil {
		return auths, err
	}
	out := make([]*Auth, 0, len(auths))
	for _, a := range auths {
		if allowed[a.ID] {
			out = append(out, a)
		}
	}
	return out, nil
}

func (e *poolCapacityError) Unwrap() error   { return e.base }
func (e *poolCapacityError) StatusCode() int { return 529 }

// finishRateDispatch refunds local failures before any upstream transport attempt.
func (m *Manager) finishRateDispatch(ctx context.Context) {
	if ctx == nil || cliproxyexecutor.UpstreamAttempted(ctx) {
		return
	}
	ref, ok := ctx.Value(rateAttemptContextKey{}).(*rateAttemptRef)
	if !ok {
		return
	}
	if m.codexPools.Load() != nil {
		m.finishPooledRateDispatch(ctx, ref)
		return
	}
	r := m.rateCap
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refundLocked(ref.authID, ref.token.Load())
}
func (r *requestRateCap) refundLocked(id string, token uint64) {
	a := r.accounts[id]
	if a == nil {
		return
	}
	for i, p := range a.attempts {
		if p.id != token {
			continue
		}
		a.attempts = append(a.attempts[:i], a.attempts[i+1:]...)
		idx := (p.at.Truncate(time.Minute).Unix() / 60) % 60
		if idx < 0 {
			idx += 60
		}
		b := &r.metrics.points[idx]
		if b.Time.Equal(p.at.Truncate(time.Minute)) && b.Attempts > 0 {
			b.Attempts--
		}
		break
	}
}
func (m *Manager) recordPoolFailure(err error) {
	if !isPoolCapacityError(err) {
		return
	}
	if pools := m.codexPools.Load(); pools != nil {
		pools.fastFailures.Add(1)
		return
	}
	r := m.rateCap
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metrics.bucket(r.nowFunc()).FastFailures++
}

// poolCapacityAfterPickFailure also considers accounts already attempted by this request.
// They may now be cooling and explain an otherwise generic auth_not_found result.
func (m *Manager) poolCapacityAfterPickFailure(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, err error) error {
	if m.codexPools.Load() != nil {
		return m.pooledCapacityAfterPickFailure(ctx, providers, model, opts, err)
	}
	if err == nil || isPoolCapacityError(err) || rateBypassed(ctx) || !m.rateCap.enabled() {
		return err
	}
	var authErr *Error
	if !errors.As(err, &authErr) || (authErr.Code != "auth_not_found" && authErr.Code != "auth_unavailable") {
		return err
	}
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	pinned := pinnedAuthIDFromMetadata(opts.Metadata)
	m.mu.RLock()
	defer m.mu.RUnlock()
	requirePositiveWeight := rateCapRequiresPositiveWeight(m.selector)
	candidates := make([]*Auth, 0)
	for _, a := range m.auths {
		if a == nil || a.Disabled || (pinned != "" && pinned != a.ID) || !eligibility.allows(a) || !containsProvider(providers, executorKeyFromAuth(a)) || (requirePositiveWeight && authWeight(a) <= 0) {
			continue
		}
		if _, ok := m.executors[executorKeyFromAuth(a)]; !ok {
			continue
		}
		if model != "" && !m.authSupportsRouteModel(registry.GetGlobalRegistry(), a, model) {
			continue
		}
		candidates = append(candidates, a)
	}
	_, errRate := m.filterRateCandidates(ctx, m.selector, candidates, model, time.Now())
	if errRate != nil {
		return errRate
	}
	return err
}

// poolCapacityAfterExecutionFailure runs before any retry wait, including when
// the credential retry budget ended the inner execution loop before selection.
func (m *Manager) poolCapacityAfterExecutionFailure(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, err error) error {
	if err == nil || !isCredentialRetryRoundStatus(statusCodeFromError(err)) {
		return err
	}
	capacity := m.poolCapacityAfterPickFailure(ctx, providers, model, opts, &Error{Code: "auth_not_found"})
	if isPoolCapacityError(capacity) {
		return capacity
	}
	return err
}
