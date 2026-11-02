package auth

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// codexSpeedTokenMinValidity is how long an access token must stay valid for a speed probe.
const codexSpeedTokenMinValidity = time.Minute

func codexSpeedTokenUsable(a *Auth, now time.Time) bool {
	if token, _ := a.Metadata["access_token"].(string); strings.TrimSpace(token) == "" {
		return false
	}
	if raw, _ := a.Metadata["expired"].(string); strings.TrimSpace(raw) != "" {
		if exp, err := time.Parse(time.RFC3339, strings.TrimSpace(raw)); err == nil && exp.Before(now.Add(codexSpeedTokenMinValidity)) {
			return false
		}
	}
	return true
}

func codexSpeedOAuth(a *Auth) bool {
	return a != nil && a.Provider == "codex" && a.AuthKind() != AuthKindAPIKey && strings.TrimSpace(authAttribute(a, AttributeAPIKey)) == ""
}

// AcquireCodexProbeAuthByID reserves one speed probe on a specific pooled account: it locates
// the account's current pool, checks eligibility under the pool lock and reserves an attempt in
// that pool's rate window plus an account concurrency slot. Release with ReleaseCodexProbeAuth.
func (m *Manager) AcquireCodexProbeAuthByID(authID, model string) (*Auth, string) {
	if m == nil || m.codexPools.Load() == nil {
		return nil, "codex pools not running"
	}
	guard, err := m.lockAuth(context.Background(), authID)
	if err != nil {
		return nil, "credential missing"
	}
	defer guard.unlock()
	a := guard.auths[authID]
	if guard.pool == nil || a == nil || !codexSpeedOAuth(a) {
		return nil, "credential missing"
	}
	now := time.Now()
	if a.Disabled || a.Status == StatusDisabled || !codexSpeedTokenUsable(a, now) {
		return nil, "credential unusable"
	}
	if blocked, _, _ := isAuthBlockedForModel(a, model, now); blocked {
		return nil, "credential cooling"
	}
	if !m.authSupportsRouteModel(registry.GetGlobalRegistry(), a, model) {
		return nil, "model unsupported"
	}
	r := guard.pool.rate
	count, limit, _ := r.accountLocked(authID, r.nowFunc())
	if count >= limit {
		return nil, "rate window full"
	}
	lease, ok := m.accountConcurrency.tryAcquire(a)
	if !ok {
		return nil, CodexProbeConcurrencyFull
	}
	token := codexPoolRateSequence.Add(1)
	account := r.getLocked(authID, r.nowFunc())
	account.attempts = append(account.attempts, rateAttempt{id: token, at: r.nowFunc()})
	probe := a.Clone()
	probe.codexProbeAttempt = &rateAttemptRef{authID: authID, epoch: probe.RegistrationEpoch}
	probe.codexProbeAttempt.token.Store(token)
	probe.codexProbeLease = lease
	return probe, ""
}

// CodexSpeedAuthInfos lists pooled Codex OAuth accounts for the speed recompute and probing.
// QuotaUsed is left at -1 for the caller to fill.
func (m *Manager) CodexSpeedAuthInfos(model string) []CodexSpeedAuthInfo {
	if m == nil {
		return nil
	}
	pools := m.codexPools.Load()
	if pools == nil {
		return nil
	}
	now := time.Now()
	var out []CodexSpeedAuthInfo
	for _, p := range pools.table.Load().pools {
		p.lock()
		for _, a := range p.auths {
			if !codexSpeedOAuth(a) {
				continue
			}
			schedulable := !a.Disabled && a.Status != StatusDisabled && codexSpeedTokenUsable(a, now)
			if schedulable {
				if blocked, _, _ := isAuthBlockedForModel(a, model, now); blocked {
					schedulable = false
				}
			}
			out = append(out, CodexSpeedAuthInfo{ID: a.ID, AccountKey: codexSpeedAccountKey(a), Schedulable: schedulable, QuotaUsed: -1})
		}
		p.mu.Unlock()
	}
	return out
}

// CodexPoolsRunning reports whether the manager runs Codex account pools.
func (m *Manager) CodexPoolsRunning() bool { return m != nil && m.codexPools.Load() != nil }
