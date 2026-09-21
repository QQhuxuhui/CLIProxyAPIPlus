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
	cooldownProbeLeases      sync.Map // authID|modelKey -> *cooldownProbeLeaseEntry
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

// cooldownProbeLeaseEntry is one lease. Its pointer identifies the owner, so a
// request that outlived its lease cannot release the one that replaced it.
type cooldownProbeLeaseEntry struct {
	expiry int64
}

// cooldownProbe tracks the probe lease held by one request while it walks
// through credentials.
type cooldownProbe struct {
	held    string
	lease   *cooldownProbeLeaseEntry
	skipped map[string]struct{}
	open    bool
}

// release returns the lease taken for the previous attempt, if any.
func (p *cooldownProbe) release() {
	if p == nil || p.held == "" {
		return
	}
	cooldownProbeLeases.CompareAndDelete(p.held, p.lease)
	p.held = ""
	p.lease = nil
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
		// OAuth aliases and unaliased prefixed models use the selection key.
		// API-key aliases can retain the full route key in stateModelForExecution.
		stateModel := m.selectionModelKeyForAuth(auth, model)
		now := cooldownProbeNow()
		key, recovering := cooldownProbeKey(auth, stateModel, now)
		if !recovering && canonicalModelKey(model) != stateModel {
			key, recovering = cooldownProbeKey(auth, model, now)
		}
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
	lease := &cooldownProbeLeaseEntry{expiry: now.Add(cooldownProbeLease).UnixNano()}
	for {
		current, loaded := cooldownProbeLeases.LoadOrStore(key, lease)
		if !loaded {
			p.held, p.lease = key, lease
			return true
		}
		if currentLease, ok := current.(*cooldownProbeLeaseEntry); ok && currentLease != nil && currentLease.expiry > now.UnixNano() {
			return false
		}
		if cooldownProbeLeases.CompareAndSwap(key, current, lease) {
			p.held, p.lease = key, lease
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
