package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const codexRetryAccounts = 6
const codexRetryWindow = 20 * time.Second

type codexRetryKey struct{}
type codexRetryBudget struct {
	upstreamCalls      atomic.Uint64
	admittedDispatches int
	mu                 sync.Mutex
	now                func() time.Time
	deadline           time.Time
	attempted          map[string]struct{}
	failures           int
	overloads          int
	otherFailures      int
	quota              bool
	last               error
	quotaError         error
	lastProvider       string
}

// WithCodexRetryBudget establishes one soft budget for a logical request. It is
// idempotent so handler retries and websocket replays share the original budget.
// It never installs a context deadline or interrupts an established stream.
func WithCodexRetryBudget(ctx context.Context, parents ...context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if codexBudget(ctx) != nil {
		return ctx
	}
	for _, parent := range parents {
		if b := codexBudget(parent); b != nil {
			return context.WithValue(ctx, codexRetryKey{}, b)
		}
	}
	return context.WithValue(ctx, codexRetryKey{}, &codexRetryBudget{now: time.Now, deadline: time.Now().Add(codexRetryWindow), attempted: make(map[string]struct{})})
}
func codexBudget(ctx context.Context) *codexRetryBudget {
	if ctx == nil {
		return nil
	}
	b, _ := ctx.Value(codexRetryKey{}).(*codexRetryBudget)
	return b
}
func (b *codexRetryBudget) remaining() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deadline.Sub(b.now())
}
func (b *codexRetryBudget) count() int { b.mu.Lock(); defer b.mu.Unlock(); return len(b.attempted) }
func (b *codexRetryBudget) exclusions() map[string]struct{} {
	out := make(map[string]struct{})
	if b == nil {
		return out
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for id := range b.attempted {
		out[id] = struct{}{}
	}
	return out
}
func (b *codexRetryBudget) claim(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if (len(b.attempted) > 0 && !b.now().Before(b.deadline)) || len(b.attempted) >= codexRetryAccounts {
		return false
	}
	if _, seen := b.attempted[id]; seen {
		return false
	}
	b.attempted[id] = struct{}{}
	return true
}

var errCodexRetryBudget = errors.New("codex request retry budget exhausted")

func claimCodexRetry(ctx context.Context, provider, id string) error {
	if provider != "codex" {
		return nil
	}
	if b := codexBudget(ctx); b != nil && !b.claim(id) {
		return errCodexRetryBudget
	}
	return nil
}
func codexOnly(providers []string) bool { return len(providers) == 1 && providers[0] == "codex" }

// codexFailureClass checks structured quota information before broad status
// classification: legacy bootstrap failures may wrap rate_limit_error as 503.
func codexFailureClass(err error) string {
	if err == nil || isPoolCapacityError(err) || errors.Is(err, errCodexRetryBudget) {
		return ""
	}
	var resultErr *Error
	if errors.As(err, &resultErr) && resultErr.Code == "codex_upstream_overload" {
		return "overload"
	}
	status := statusCodeFromError(err)
	body := err.Error()
	var original interface{ CodexFailureInfo() (int, string, string) }
	if errors.As(err, &original) {
		if code, raw, source := original.CodexFailureInfo(); source != "" {
			status, body = code, raw
		}
	}
	if i := strings.IndexByte(body, '{'); i >= 0 {
		body = body[i:]
	}
	typ := strings.ToLower(gjson.Get(body, "error.type").String())
	code := strings.ToLower(gjson.Get(body, "error.code").String())
	if typ == "" {
		typ = strings.ToLower(gjson.Get(body, "type").String())
	}
	if code == "" {
		code = strings.ToLower(gjson.Get(body, "code").String())
	}
	if code == "" && resultErr != nil {
		code = strings.ToLower(resultErr.Code)
	}
	lower := strings.ToLower(body)
	if status == 429 || typ == "rate_limit_error" || code == "rate_limit_exceeded" || typ == "usage_limit_reached" || code == "usage_limit_reached" {
		return "quota"
	}
	if code == "model_at_capacity" || code == "model_is_at_capacity" || typ == "model_at_capacity" || typ == "model_is_at_capacity" {
		return "capacity"
	}

	if isRequestInvalidError(err) || isRequestStopError(err) || isRequestTerminatedError(err) {
		return "other"
	}
	if status == 503 || status == 529 || typ == "service_unavailable_error" || code == "server_is_overloaded" || ((typ == "server_error" || code == "server_error") && strings.Contains(lower, "you can retry your request")) {
		return "overload"
	}
	if isTransientTransportError(err) || isTransientTransportResultError(resultErrorWithoutClassification(err)) {
		return "transport"
	}
	return "other"
}

// Keep transport classification independent of resultErrorFromError.
func resultErrorWithoutClassification(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Message: err.Error(), HTTPStatus: statusCodeFromError(err)}
}
func (b *codexRetryBudget) observe(err error) {
	if b == nil || err == nil || errors.Is(err, errCodexRetryBudget) || isPoolCapacityError(err) {
		return
	}
	class := codexFailureClass(err)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	b.last = err
	switch class {
	case "overload":
		b.overloads++
	case "quota":
		b.quota = true
		b.quotaError = err
	case "transport":
	default:
		b.otherFailures++
	}
}

type codexRetryFinalError struct {
	status int
	kind   string
	cause  error
	delay  time.Duration
}

func (e *codexRetryFinalError) Error() string {
	if e.kind == "upstream_pool_busy" {
		return `{"error":{"type":"upstream_pool_busy","code":"upstream_pool_busy","message":"Codex upstream capacity is temporarily exhausted"}}`
	}
	return e.cause.Error()
}
func (e *codexRetryFinalError) StatusCode() int            { return e.status }
func (e *codexRetryFinalError) Unwrap() error              { return e.cause }
func (e *codexRetryFinalError) RetryAfter() *time.Duration { return &e.delay }
func (e *codexRetryFinalError) Headers() http.Header {
	seconds := max(1, int((e.delay+time.Second-1)/time.Second))
	return http.Header{"Retry-After": {strconv.Itoa(seconds)}}
}
func (m *Manager) codexFinalError(b *codexRetryBudget, model string, fallback error) error {
	if isRequestInvalidError(fallback) || isRequestStopError(fallback) || isRequestTerminatedError(fallback) {
		return fallback
	}
	b.mu.Lock()
	quota, overloads, other, last, quotaErr := b.quota, b.overloads, b.otherFailures, b.last, b.quotaError
	b.mu.Unlock()
	if overloads > 0 && !quota && other == 0 {
		return &codexRetryFinalError{status: 529, kind: "upstream_pool_busy", cause: last, delay: time.Second}
	}
	if quota {
		now := time.Now()
		var earliest time.Time
		attempted := b.exclusions()
		m.mu.RLock()
		for id := range attempted {
			if a := m.auths[id]; a != nil {
				_, _, next := isAuthBlockedForModel(a, m.selectionModelForAuth(a, model), now)
				if next.After(now) && (earliest.IsZero() || next.Before(earliest)) {
					earliest = next
				}
			}
		}
		m.mu.RUnlock()
		return &codexRetryFinalError{status: 429, cause: quotaErr, delay: max(time.Second, earliest.Sub(now))}
	}
	if isPoolCapacityError(fallback) {
		return fallback
	}
	if last != nil {
		return last
	}
	return fallback
}

// codexRetryWait considers only credentials not yet attempted by this request.
// A non-positive maxWait retains the existing no-wait configuration semantics.
func (m *Manager) codexRetryWait(ctx context.Context, model string, opts exec.Options, maxWait time.Duration) (time.Duration, bool) {
	b := codexBudget(ctx)
	if b == nil {
		return 0, false
	}
	excluded := b.exclusions()
	now := time.Now()
	var earliest time.Time
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	pinned := pinnedAuthIDFromMetadata(opts.Metadata)
	m.mu.RLock()
	defer m.mu.RUnlock()
	requireWeight := rateCapRequiresPositiveWeight(m.selector)
	for id, a := range m.auths {
		if a == nil || a.Provider != "codex" || a.Disabled || a.Status == StatusDisabled || !eligibility.allows(a) || (pinned != "" && id != pinned) {
			continue
		}
		if requireWeight && authWeight(a) <= 0 {
			continue
		}
		if _, seen := excluded[id]; seen {
			continue
		}
		if !m.authSupportsRouteModel(registry.GetGlobalRegistry(), a, model) {
			continue
		}
		blocked, _, next := isAuthBlockedForModel(a, m.selectionModelForAuth(a, model), now)
		if !blocked {
			return 0, true
		}
		if next.After(now) && (earliest.IsZero() || next.Before(earliest)) {
			earliest = next
		}
	}
	if earliest.IsZero() {
		return 0, false
	}
	wait := earliest.Sub(now)
	if maxWait <= 0 || wait > maxWait || wait >= b.remaining() {
		return 0, false
	}
	return wait, true
}
func runCodexRetry[T any](m *Manager, ctx context.Context, model string, opts exec.Options, run func(context.Context) (T, error)) (T, error) {
	ctx = WithCodexRetryBudget(ctx)
	b := codexBudget(ctx)
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	_, _, maxWait := m.retrySettings()
	var last error
	for b.count() < codexRetryAccounts && (b.count() == 0 || b.remaining() > 0) {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		before := b.count()
		value, err := run(ctx)
		if err == nil {
			return value, nil
		}
		err = unwrapExecutionBoundaryError(err)
		if b.count() > before && !errors.Is(err, errCodexRetryBudget) && !isPoolCapacityError(err) {
			b.mu.Lock()
			if b.last != nil {
				b.last = err
				if codexFailureClass(err) == "quota" {
					b.quotaError = err
				}
			}
			b.mu.Unlock()
		}
		err = m.poolCapacityAfterExecutionFailure(ctx, []string{"codex"}, model, opts, err)
		if last == nil || b.count() > before || isPoolCapacityError(err) {
			last = err
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if isRequestInvalidError(err) || isRequestStopError(err) || isRequestTerminatedError(err) {
			return zero, err
		}
		if errors.Is(err, errCodexRetryBudget) || isPoolCapacityError(err) {
			break
		}
		if b.count() > before {
			continue
		}
		if wait, ok := m.codexRetryWait(ctx, model, opts, maxWait); ok {
			// No jitter: the soft admission budget must not be extended by waiting.
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return zero, ctx.Err()
			case <-timer.C:
			}
			continue
		}
		break
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if last == nil {
		last = &Error{Code: "auth_not_found", Message: "no untried Codex credential available"}
	}
	return zero, m.codexFinalError(b, model, last)
}

// CheckCodexRetryAdmission gates a new send, never the lifetime of an in-flight send.
func CheckCodexRetryAdmission(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if b := codexBudget(ctx); b != nil && b.remaining() <= 0 {
		return errCodexRetryBudget
	}
	return nil
}

func (m *Manager) claimLocalCodexRetry(ctx context.Context, provider, id string) error {
	if m.HomeEnabled() {
		return nil
	}
	return claimCodexRetry(ctx, provider, id)
}

// executionResultError observes the original error before the public auth.Error
// representation drops transport-specific methods. Error retains its SDK layout.
func (m *Manager) executionResultError(ctx context.Context, provider string, err error) *Error {
	result := resultErrorFromError(err)
	if b := codexBudget(ctx); b != nil {
		b.mu.Lock()
		b.lastProvider = provider
		b.mu.Unlock()
	}
	if provider != "codex" || err == nil || !m.codexRetryEnabled() {
		return result
	}
	codexBudget(ctx).observe(err)
	switch codexFailureClass(err) {
	case "overload":
		result.Code = "codex_upstream_overload"
	case "quota":
		result.HTTPStatus = http.StatusTooManyRequests
	}
	return result
}

func (m *Manager) codexRequestContext(ctx context.Context, providers []string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if !m.codexRetryEnabled() {
		return exec.WithCodexGenerationObserver(context.WithValue(ctx, codexRetryKey{}, (*codexRetryBudget)(nil)), nil)
	}
	for _, p := range providers {
		if strings.EqualFold(strings.TrimSpace(p), "codex") {
			ctx = WithCodexRetryBudget(ctx)
			b := codexBudget(ctx)
			return exec.WithCodexGenerationObserver(ctx, func() { exec.ObserveCodexLogicalCall(b.upstreamCalls.Add(1)) })
		}
	}
	return ctx
}
func (m *Manager) finalizeMixedCodexRetry(ctx context.Context, providers []string, model string, err error) error {
	if err == nil || codexOnly(providers) {
		return err
	}
	b := codexBudget(ctx)
	if b == nil {
		return err
	}
	b.mu.Lock()
	provider, last := b.lastProvider, b.last
	b.mu.Unlock()
	if provider != "codex" || last == nil {
		return err
	}
	if errors.Is(err, last) || errors.Is(err, errCodexRetryBudget) || isPoolCapacityError(err) {
		return m.codexFinalError(b, model, err)
	}
	return err
}
func (m *Manager) codexRetryOverride(ctx context.Context, opts exec.Options, model string, maxWait time.Duration) (time.Duration, bool, bool) {
	b := codexBudget(ctx)
	if b == nil {
		return 0, false, false
	}
	b.mu.Lock()
	lastProvider := b.lastProvider
	b.mu.Unlock()
	if lastProvider != "codex" {
		return 0, false, false
	}
	if b.count() >= codexRetryAccounts || b.remaining() <= 0 {
		return 0, false, true
	}
	wait, ok := m.codexRetryWait(ctx, model, opts, maxWait)
	return wait, ok, true
}
func (m *Manager) codexUsesRequestBudget(ctx context.Context, id string) bool {
	if codexBudget(ctx) == nil || m.HomeEnabled() {
		return false
	}
	// Pooled and side Codex auths live outside m.auths; resolve them through the
	// owner index so the pooled hot path never takes the manager lock.
	if m.codexPools.Load() != nil && m.poolOwnsAuth(id) {
		return true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := m.auths[id]
	return a != nil && a.Provider == "codex"
}

// codexRetryEnabled is read on each logical request so rollback needs no restart.
func (m *Manager) codexRetryEnabled() bool {
	cfg := m.runtimeConfigSnapshot()
	return cfg.Codex.RetryBudget.Enable && !m.HomeEnabled()
}

func codexFinalHeaders(err error, fallback http.Header) http.Header {
	var final *codexRetryFinalError
	if !errors.As(err, &final) {
		return fallback
	}
	headers := fallback.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	for k, v := range final.Headers() {
		headers[k] = v
	}
	return headers
}

func codexEligibleProviders(ctx context.Context, providers []string) []string {
	b := codexBudget(ctx)
	if b == nil || (b.count() < codexRetryAccounts && (b.count() == 0 || b.remaining() > 0)) {
		return providers
	}
	others := make([]string, 0, len(providers))
	for _, p := range providers {
		if p != "codex" {
			others = append(others, p)
		}
	}
	return others
}

// admitCodexDispatch counts sends separately from accounts: aliases on the first
// account are retries too. Only the first dispatch may start after the soft deadline.
func admitCodexDispatch(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if b := codexBudget(ctx); b != nil {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.admittedDispatches > 0 && !b.now().Before(b.deadline) {
			return errCodexRetryBudget
		}
		b.admittedDispatches++
	}
	return nil
}
