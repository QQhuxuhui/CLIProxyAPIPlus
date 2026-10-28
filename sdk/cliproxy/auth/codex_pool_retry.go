package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type poolRetryRoundKey struct{}
type poolRetryRound struct{ round, limit int }

const poolHintMetadataKey = "cliproxy.codex_pool_slot"

// Retry scans stop at the first immediately usable pool. A complete scan is
// necessary only when every pool is unavailable and the earliest recovery wins.
func (m *Manager) poolRetryAvailability(providers []string, model string, attempt int, eligibility authSelectionEligibility, pinned string, limit, status int, attempted map[string]struct{}) (time.Duration, bool) {
	pools := m.codexPools.Load()
	now := time.Now()
	var best time.Duration
	found := false
	visit := func(auths map[string]*Auth) bool {
		for id, a := range auths {
			if a == nil || a.Disabled || a.Status == StatusDisabled || (pinned != "" && id != pinned) || !eligibility.allows(a) || !containsProvider(providers, executorKeyFromAuth(a)) || attempt >= effectiveRequestRetryLimit(a, limit) {
				continue
			}
			if rateCapRequiresPositiveWeight(m.shared.Load().selector) && authWeight(a) <= 0 {
				continue
			}
			if !m.authSupportsRouteModel(registry.GetGlobalRegistry(), a, model) {
				continue
			}
			eligible, next := retryRoundAvailabilityForAuth(a, m.selectionModelForAuth(a, model), now)
			if !eligible {
				continue
			}
			wait := max(time.Duration(0), next.Sub(now))
			if _, used := attempted[id]; used && status == http.StatusTooManyRequests && !m.cooldownDisabledForAuth(a) {
				wait = max(wait, minQuotaCooldownFloor)
			}
			if !found || wait < best {
				found = true
				best = wait
			}
			if wait == 0 {
				return true
			}
		}
		return false
	}
	if hasCodexProvider(providers) {
		table := pools.table.Load()
		order := append([]*codexAccountPool{table.owners[eligibility.poolSlot], pools.side}, table.pools...)
		seen := make(map[*codexAccountPool]bool)
		for _, p := range order {
			if seen[p] {
				continue
			}
			seen[p] = true
			p.lock()
			ready := visit(p.auths)
			p.mu.Unlock()
			if ready {
				return 0, true
			}
		}
	}
	for _, provider := range providers {
		if provider != "codex" {
			m.mu.RLock()
			ready := visit(m.auths)
			m.mu.RUnlock()
			if ready {
				return 0, true
			}
			break
		}
	}
	return best, found
}
func (m *Manager) pooledCapacityAfterPickFailure(ctx context.Context, providers []string, model string, opts exec.Options, original error) error {
	if original == nil || isPoolCapacityError(original) || rateBypassed(ctx) || !hasCodexProvider(providers) {
		return original
	}
	var authErr *Error
	if !errors.As(original, &authErr) || (authErr.Code != "auth_not_found" && authErr.Code != "auth_unavailable") || nonNilContext(ctx).Err() != nil {
		return original
	}
	pools := m.codexPools.Load()
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	pinned := pinnedAuthIDFromMetadata(opts.Metadata)
	now := time.Now()
	var earliest time.Time
	blockedAny := false
	enabled := false
	for _, p := range append(append([]*codexAccountPool{}, pools.table.Load().pools...), pools.side) {
		p.lock()
		if !p.rate.config.Enabled || !p.rate.config.FailFast {
			p.mu.Unlock()
			continue
		}
		enabled = true
		for id, a := range p.auths {
			if a.Disabled || a.Status == StatusDisabled || !eligibility.allows(a) || (pinned != "" && id != pinned) || (rateCapRequiresPositiveWeight(m.shared.Load().selector) && authWeight(a) <= 0) || !m.authSupportsRouteModel(registry.GetGlobalRegistry(), a, model) {
				continue
			}
			blocked, reason, next := isAuthBlockedForModel(a, m.selectionModelForAuth(a, model), now)
			count, limit, rateNext := p.rate.accountLocked(id, p.rate.nowFunc())
			if !blocked && count < limit {
				p.mu.Unlock()
				return original
			}
			if (reason == blockReasonCooldown || next.After(now)) || count >= limit {
				blockedAny = true
				if rateNext.After(next) {
					next = rateNext
				}
				if earliest.IsZero() || next.Before(earliest) {
					earliest = next
				}
			}
		}
		p.mu.Unlock()
	}
	if enabled && blockedAny {
		return newPoolCapacityError(earliest.Sub(now))
	}
	return original
}
