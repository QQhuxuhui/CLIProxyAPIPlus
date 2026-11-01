package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// AcquireCodexProbeAuth picks a schedulable codex OAuth credential that supports model (same
// registry check as normal scheduling) with spare capacity in its
// per-account rate window (the v70 codex pool window) and counts one attempt against that
// window, so probing never pushes an account past its learned limit. The attempt is not added
// to the codex pool dashboard metrics. It returns a clone of the credential, or nil and the
// reason none is available. Probing is unavailable in Home mode.
func (m *Manager) AcquireCodexProbeAuth(model string) (*Auth, string) {
	if m == nil || m.rateCap == nil {
		return nil, "auth manager unavailable"
	}
	if m.HomeEnabled() {
		return nil, "home mode"
	}
	if m.codexPools.Load() != nil {
		return m.acquirePooledProbeAuth(model)
	}
	registryRef := registry.GetGlobalRegistry()
	m.mu.RLock()
	defer m.mu.RUnlock()
	r := m.rateCap
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.nowFunc()
	raced := make(map[string]struct{})
	for {
		probe, reason, retry := m.pickCodexProbeLocked(model, r, registryRef, now, raced)
		if !retry {
			return probe, reason
		}
	}
}

// pickCodexProbeLocked reserves the best candidate, reporting retry when it lost a concurrency
// race. A slot is held before the probe's attempt is counted against the rate window.
func (m *Manager) pickCodexProbeLocked(model string, r *requestRateCap, registryRef *registry.ModelRegistry, now time.Time, raced map[string]struct{}) (*Auth, string, bool) {
	oauth, schedulable := 0, 0
	var best *Auth
	bestRemaining := 0
	sawFull := len(raced) > 0
	for _, a := range m.auths {
		if a == nil || a.Provider != "codex" || a.AuthKind() == AuthKindAPIKey || strings.TrimSpace(authAttribute(a, AttributeAPIKey)) != "" {
			continue
		}
		oauth++
		if a.Disabled || a.Status == StatusDisabled {
			continue
		}
		if token, _ := a.Metadata["access_token"].(string); strings.TrimSpace(token) == "" {
			continue
		}
		if expiry, ok := a.AccessTokenExpirationTime(); ok && !expiry.After(now) {
			continue
		}
		if model != "" && !m.authSupportsRouteModel(registryRef, a, model) {
			continue
		}
		if blocked, _, _ := isAuthBlockedForModel(a, model, now); blocked {
			continue
		}
		schedulable++
		if _, lost := raced[a.ID]; lost {
			continue
		}
		if m.accountConcurrency.full(a) {
			sawFull = true
			continue
		}
		count, limit, _ := r.accountLocked(a.ID, now)
		remaining := limit - count
		if remaining <= 0 {
			continue
		}
		if best == nil || remaining > bestRemaining || (remaining == bestRemaining && a.ID < best.ID) {
			best = a
			bestRemaining = remaining
		}
	}
	switch {
	case oauth == 0:
		return nil, "no codex oauth credential", false
	case schedulable == 0:
		return nil, "no schedulable codex oauth credential", false
	case best == nil && sawFull:
		return nil, CodexProbeConcurrencyFull, false
	case best == nil:
		return nil, "all codex oauth credentials at rate window limit", false
	}
	lease, ok := m.accountConcurrency.tryAcquire(best)
	if !ok {
		raced[best.ID] = struct{}{}
		return nil, "", true
	}
	account := r.getLocked(best.ID, now)
	r.sequence++
	account.attempts = append(account.attempts, rateAttempt{id: r.sequence, at: now})
	probe := best.Clone()
	probe.codexProbeAttempt = &rateAttemptRef{authID: best.ID}
	probe.codexProbeAttempt.token.Store(r.sequence)
	probe.codexProbeLease = lease
	return probe, "", false
}

// ReleaseCodexProbeAuth returns the concurrency slot of a probe clone. It is idempotent, so the
// probe runner can defer it to cover cancellation and panics.
func (m *Manager) ReleaseCodexProbeAuth(auth *Auth) {
	if auth != nil {
		auth.codexProbeLease.release()
	}
}

// MarkCodexProbeResult feeds probe failures into normal credential cooling and rate
// learning without publishing business results or recording business request metrics.
// The reservation is consumed once, including successful or canceled probes.
func (m *Manager) MarkCodexProbeResult(ctx context.Context, auth *Auth, model string, err error) {
	m.ReleaseCodexProbeAuth(auth)
	if m == nil || auth == nil || auth.codexProbeAttempt == nil {
		return
	}
	attempt := auth.codexProbeAttempt
	token := attempt.token.Swap(0)
	if token == 0 || err == nil || errors.Is(err, context.Canceled) || (ctx != nil && ctx.Err() != nil) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ref := &rateAttemptRef{authID: attempt.authID, epoch: attempt.epoch}
	ref.token.Store(token)
	ctx = context.WithValue(ctx, rateAttemptContextKey{}, ref)
	result := Result{
		AuthID: attempt.authID, Provider: "codex", Model: model,
		Error: resultErrorFromError(err), CredentialScope: isCredentialScopedError(err),
		SkipQuotaObservation: true,
	}
	fillQuotaHints(&result, err)
	if m.codexPools.Load() != nil {
		ctx = context.WithValue(ctx, poolResultEpochKey{}, auth.RegistrationEpoch)
	}
	m.markResult(ctx, result, true)
}
