package auth

import (
	"strings"
)

// AcquireCodexProbeAuth picks a schedulable codex OAuth credential with spare capacity in its
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
	m.mu.RLock()
	defer m.mu.RUnlock()
	r := m.rateCap
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.nowFunc()
	oauth, schedulable := 0, 0
	var best *Auth
	bestRemaining := 0
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
		if blocked, _, _ := isAuthBlockedForModel(a, model, now); blocked {
			continue
		}
		schedulable++
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
		return nil, "no codex oauth credential"
	case schedulable == 0:
		return nil, "no schedulable codex oauth credential"
	case best == nil:
		return nil, "all codex oauth credentials at rate window limit"
	}
	account := r.getLocked(best.ID, now)
	r.sequence++
	account.attempts = append(account.attempts, rateAttempt{id: r.sequence, at: now})
	return best.Clone(), ""
}
