package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// cooldownProbeLease bounds how long one request may hold the probe for an
// account+model. It only guards against a lease that is never released; the
// normal path releases as soon as the attempt finishes.
const cooldownProbeLease = 20 * time.Second

var (
	cooldownProbeGateEnabled atomic.Bool
	cooldownProbeLeases      sync.Map // authID|modelKey -> lease expiry (unix nano)
	cooldownProbeNow         = time.Now
)

// SetCooldownProbeGate toggles the single-probe gate. When an account+model
// cooldown expires, every waiting request used to hit that account at once, and
// all but the first usually earned another 429 and burned a retry. With the gate
// on, one request probes the account while the others rotate to a different
// credential without penalising it. It is off by default.
func SetCooldownProbeGate(enabled bool) {
	cooldownProbeGateEnabled.Store(enabled)
}

// cooldownProbe tracks the probe lease held by one request while it walks
// through credentials.
type cooldownProbe struct {
	held    string
	skipped map[string]struct{}
	open    bool
}

// release returns the lease taken for the previous attempt, if any.
func (p *cooldownProbe) release() {
	if p == nil || p.held == "" {
		return
	}
	cooldownProbeLeases.Delete(p.held)
	p.held = ""
}

// pickNextMixedProbed wraps pickNextMixed with the single-probe gate. A skipped
// credential is only deferred: when nothing else can be picked, the skipped ones
// become eligible again, so the gate never turns a servable request into a
// failure.
func (m *Manager) pickNextMixedProbed(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}, probe *cooldownProbe) (*Auth, ProviderExecutor, string, error) {
	probe.release()
	if probe == nil || !cooldownProbeGateEnabled.Load() {
		return m.pickNextMixed(ctx, providers, model, opts, tried)
	}
	for {
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, model, opts, tried)
		if errPick != nil {
			if probe.open || len(probe.skipped) == 0 {
				return nil, nil, "", errPick
			}
			probe.open = true
			for authID := range probe.skipped {
				delete(tried, authID)
			}
			continue
		}
		if probe.open || auth == nil {
			return auth, executor, provider, nil
		}
		key, recovering := cooldownProbeKey(auth, model, cooldownProbeNow())
		if !recovering || probe.acquire(key) {
			return auth, executor, provider, nil
		}
		if probe.skipped == nil {
			probe.skipped = make(map[string]struct{})
		}
		probe.skipped[auth.ID] = struct{}{}
		tried[auth.ID] = struct{}{}
	}
}

// acquire takes the probe lease for key unless another request holds a live one.
func (p *cooldownProbe) acquire(key string) bool {
	now := cooldownProbeNow()
	expiry := now.Add(cooldownProbeLease).UnixNano()
	for {
		current, loaded := cooldownProbeLeases.LoadOrStore(key, expiry)
		if !loaded {
			p.held = key
			return true
		}
		if currentExpiry, ok := current.(int64); ok && currentExpiry > now.UnixNano() {
			return false
		}
		if cooldownProbeLeases.CompareAndSwap(key, current, expiry) {
			p.held = key
			return true
		}
	}
}

// cooldownProbeKey reports whether auth is leaving a cooldown for model: the
// failure state is still recorded, but its retry time has passed. Such a
// credential has not been confirmed healthy yet.
func cooldownProbeKey(auth *Auth, model string, now time.Time) (string, bool) {
	if auth == nil || model == "" || len(auth.ModelStates) == 0 {
		return "", false
	}
	modelKey := canonicalModelKey(model)
	for stateModel, state := range auth.ModelStates {
		if state == nil || canonicalModelKey(stateModel) != modelKey {
			continue
		}
		if !state.Unavailable && !state.Quota.Exceeded {
			continue
		}
		if state.NextRetryAfter.IsZero() && state.Quota.NextRecoverAt.IsZero() {
			continue
		}
		if state.NextRetryAfter.After(now) || state.Quota.NextRecoverAt.After(now) {
			continue
		}
		return auth.ID + "|" + modelKey, true
	}
	return "", false
}
