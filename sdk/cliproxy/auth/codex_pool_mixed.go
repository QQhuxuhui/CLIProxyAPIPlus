package auth

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type poolPreviewKey struct{}

const poolStickyMetadataKey = "cliproxy.codex_pool_sticky"

func (m *Manager) legacyPoolPriority(ctx context.Context, providers []string, model string, opts exec.Options, tried map[string]struct{}) int {
	best := math.MinInt
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	pinned := pinnedAuthIDFromMetadata(opts.Metadata)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for id, a := range m.auths {
		if a == nil || !containsProvider(providers, executorKeyFromAuth(a)) || !eligibility.allows(a) || (pinned != "" && id != pinned) {
			continue
		}
		if _, used := tried[id]; used {
			continue
		}
		if rateCapRequiresPositiveWeight(m.selector) && authWeight(a) <= 0 {
			continue
		}
		if !m.authSupportsRouteModel(registry.GetGlobalRegistry(), a, model) {
			continue
		}
		if blocked, _, _ := isAuthBlockedForModel(a, m.selectionModelForAuth(a, model), time.Now()); blocked {
			continue
		}
		best = max(best, authPriority(a))
	}
	return best
}
func copyPoolMetadata(dst, src map[string]any) {
	clear(dst)
	for key, value := range src {
		dst[key] = value
	}
}
func (m *Manager) pickPoolMixed(ctx context.Context, providers []string, model string, opts exec.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	var others []string
	for _, p := range providers {
		if !strings.EqualFold(strings.TrimSpace(p), "codex") {
			others = append(others, p)
		}
	}
	if len(others) == 0 {
		a, e, err := m.pickCodexPool(ctx, model, opts, tried)
		return a, e, "codex", err
	}
	opts.EnsureMetadata()
	// Existing legacy affinity remains authoritative while its credential is usable.
	if key := CanonicalSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata); key != "" {
		if hint, status := m.lookupLegacySessionAffinity("mixed", model, key); status == "bound" && hint != nil && containsProvider(others, executorKeyFromAuth(hint)) {
			if _, used := tried[hint.ID]; !used {
				if _, _, err := m.resolveLegacySelectionSnapshot(ctx, hint, model, opts); err == nil {
					return m.pickNextMixed(ctx, others, model, opts, tried)
				}
			}
		}
	}
	priority := m.legacyPoolPriority(ctx, others, model, opts, tried)
	pooledOpts := opts
	pooledOpts.Metadata = cloneRequestMetadata(opts.Metadata)
	a, e, pooledErr := m.pickCodexPool(context.WithValue(nonNilContext(ctx), poolPreviewKey{}, true), model, pooledOpts, tried)
	finish := func() (*Auth, ProviderExecutor, string, error) {
		if saturated, _ := pooledOpts.Metadata[poolSaturatedMetadataKey].(bool); saturated {
			ctx = context.WithValue(nonNilContext(ctx), poolAllowSaturatedKey{}, true)
		}
		if affinity, ok := m.shared.Load().selector.(*SessionAffinitySelector); ok {
			key, _ := m.poolSessionKey(model, pooledOpts)
			selectedID := a.ID
			var err error
			a, e, err = m.bindPoolSelection(ctx, key, model, pooledOpts, affinity, a, tried)
			if nextCtx, nextTried, again := staleSelectionRetry(nonNilContext(ctx), err, tried, selectedID); again {
				return m.pickPoolMixed(nextCtx, providers, model, opts, nextTried)
			}
			if err != nil {
				return nil, nil, "codex", err
			}
		}
		copyPoolMetadata(opts.Metadata, pooledOpts.Metadata)
		return a, e, "codex", nil
	}
	sticky, _ := pooledOpts.Metadata[poolStickyMetadataKey].(bool)
	if pooledErr == nil && (sticky || authPriority(a) >= priority) {
		return finish()
	}
	legacyOpts := opts
	legacyOpts.Metadata = cloneRequestMetadata(opts.Metadata)
	other, otherExec, provider, err := m.pickNextMixed(ctx, others, model, legacyOpts, tried)
	if err == nil {
		copyPoolMetadata(opts.Metadata, legacyOpts.Metadata)
		return other, otherExec, provider, nil
	}
	if pooledErr == nil {
		return finish()
	}
	return nil, nil, "", preferPoolError(pooledErr, err)
}
func (m *Manager) lookupPoolSessionAffinity(model, key string) (*Auth, string) {
	if m.shared.Load().pluginScheduler != nil {
		return nil, "unsupported"
	}
	if _, ok := m.shared.Load().selector.(*SessionAffinitySelector); !ok {
		return nil, "unsupported"
	}
	pools := m.codexPools.Load()
	p, err := pools.lockSession(context.Background(), key)
	if err != nil {
		return nil, "unbound"
	}
	var matches []codexPoolBinding
	for candidate, binding := range p.bindings {
		if candidate.session == key && (model == "" || candidate.model == canonicalModelKey(model)) && time.Now().Before(binding.expires) {
			matches = append(matches, binding)
		}
	}
	p.mu.Unlock()
	var found *Auth
	for _, binding := range matches {
		a, ok := m.GetByID(binding.authID)
		if !ok || a.RegistrationEpoch != binding.epoch {
			continue
		}
		if found != nil && found.ID != a.ID {
			return nil, "ambiguous"
		}
		found = a
	}
	if found == nil {
		return nil, "unbound"
	}
	return found, "bound"
}
