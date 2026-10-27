package auth

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	session "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func clonePoolSelector(selector Selector) Selector {
	if affinity, ok := selector.(*SessionAffinitySelector); ok {
		selector = affinity.fallback
	}
	switch selector.(type) {
	case *RoundRobinSelector:
		return &RoundRobinSelector{}
	case *WeightedRoundRobinSelector:
		return &WeightedRoundRobinSelector{}
	case *FillFirstSelector:
		return &FillFirstSelector{}
	default:
		return selector
	}
}
func (p *codexAccountPool) refreshCatalogLocked() {
	priorities := make(map[string]int)
	for _, a := range p.auths {
		if a.Disabled || a.Status == StatusDisabled {
			continue
		}
		priority := authPriority(a)
		for _, id := range registry.GetGlobalRegistry().ClientModelIDs(a.ID) {
			key := strings.ToLower(canonicalModelKey(id))
			if old, ok := priorities[key]; !ok || priority > old {
				priorities[key] = priority
			}
		}
	}
	p.priorities.Store(&priorities)
	p.catalogDirty.Store(false)
}
func (p *codexAccountPool) priorityBound(model string) int {
	if p.catalogDirty.Load() {
		return math.MaxInt
	}
	snapshot := p.priorities.Load()
	if snapshot == nil {
		return math.MaxInt
	}
	value, ok := (*snapshot)[strings.ToLower(canonicalModelKey(model))]
	if !ok {
		return math.MinInt
	}
	return value
}
func (pools *codexPoolSet) pooledPriorityBound(model string) int {
	best := math.MinInt
	for _, p := range pools.table.Load().pools {
		best = max(best, p.priorityBound(model))
	}
	return best
}

// poolPickOne snapshots eligible candidates under just this pool's lock. Plugin
// callbacks and selector code run after releasing it; final identity validation
// goes through the owner index and may locate a newly migrated slot.
func (m *Manager) poolPickOne(ctx context.Context, p *codexAccountPool, model string, opts exec.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	if err := nonNilContext(ctx).Err(); err != nil {
		return nil, nil, err
	}
	shared := m.shared.Load()
	if hint := p.availability.Load(); hint != nil && hint.potential == 0 && !rateBypassed(ctx) && time.Now().Before(hint.until) {
		delay := time.Until(hint.until)
		if hint.capacity {
			return nil, nil, newPoolCapacityError(delay)
		}
		return nil, nil, newModelCooldownErrorWithCause(model, "codex", delay, nil)
	}
	across := false
	if plugin, ok := shared.pluginScheduler.(PluginSchedulerAcrossPriorities); ok {
		across = plugin.SchedulerWantsAcrossPriorities()
	}
	p.lock()
	if p.retired {
		p.mu.Unlock()
		return nil, nil, &Error{Code: "auth_not_found", Message: "pool retired"}
	}
	if !isSameSelector(p.selectorSource, shared.selector) || p.selector == nil {
		p.selector = clonePoolSelector(shared.selector)
		p.selectorSource = shared.selector
	}
	selector := p.selector
	p.refreshAvailabilityHintLocked()
	if p.catalogDirty.Load() || p.priorities.Load() == nil {
		p.refreshCatalogLocked()
	}
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	pinned := pinnedAuthIDFromMetadata(opts.Metadata)
	var candidates []*Auth
	var rateReady []*Auth
	var earliest time.Time
	rateBlocked := false
	for id, a := range p.auths {
		if a == nil || a.Provider != "codex" || a.Disabled || a.Status == StatusDisabled || !eligibility.allows(a) || (pinned != "" && id != pinned) {
			continue
		}
		if _, used := tried[id]; used {
			continue
		}
		if m.codexPools.Load().owners.entry(id).moving {
			continue
		}
		if rateCapRequiresPositiveWeight(selector) && authWeight(a) <= 0 {
			continue
		}
		if !m.authSupportsRouteModel(registry.GetGlobalRegistry(), a, model) {
			continue
		}
		candidates = append(candidates, a)
		ready, next := m.pooledRateAvailableLocked(p, a, ctx)
		if ready {
			rateReady = append(rateReady, a)
		} else {
			rateBlocked = true
			if earliest.IsZero() || next.Before(earliest) {
				earliest = next
			}
		}
	}
	var available []*Auth
	var err error
	if len(candidates) == 0 {
		err = &Error{Code: "auth_not_found", Message: "no auth available"}
	} else if len(rateReady) == 0 && rateBlocked && p.rate.config.FailFast {
		err = newPoolCapacityError(earliest.Sub(p.rate.nowFunc()))
	} else {
		available, err = m.availableAuthsForRouteModelAcrossPriorities(rateReady, "codex", model, time.Now())
	}
	if err != nil {
		if !p.rate.config.FailFast && rateBlocked {
			if allow, _ := ctx.Value(poolAllowSaturatedKey{}).(bool); !allow {
				err = &poolSaturatedError{cause: err}
			}
		}
		p.mu.Unlock()
		return nil, nil, err
	}
	if !across {
		available = highestPriorityAuths(available)
	}
	candidates = snapshotAuthsForSelection(selector, available)
	p.mu.Unlock()
	selected, handled, err := m.poolPluginPick(ctx, shared.pluginScheduler, model, opts, candidates)
	if err != nil {
		return nil, nil, err
	}
	if !handled {
		selected, err = selector.Pick(selectorContextForAvailableAuths(ctx, selector, model), "codex", selectionArgForSelector(selector, model), opts, highestPriorityAuths(candidates))
	}
	if err != nil {
		return nil, nil, restoreModelCooldownErrorModel(err, model)
	}
	if selected == nil {
		return nil, nil, &Error{Code: "auth_not_found", Message: "pool selector returned no credential"}
	}
	a, e, err := m.resolveSelectionSnapshot(ctx, selected, model, opts)
	if nextCtx, nextTried, again := staleSelectionRetry(ctx, err, tried, selected.ID); again {
		return m.poolPickOne(nextCtx, p, model, opts, nextTried)
	}
	return a, e, err
}
func (m *Manager) poolPluginPick(ctx context.Context, scheduler PluginScheduler, model string, opts exec.Options, auths []*Auth) (*Auth, bool, error) {
	if scheduler == nil {
		return nil, false, nil
	}
	if state, ok := scheduler.(pluginSchedulerState); ok && !state.HasScheduler() {
		return nil, false, nil
	}
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(auths))
	for _, a := range auths {
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{ID: a.ID, Provider: a.Provider, Priority: authPriority(a), Status: string(a.Status), Attributes: schedulerSafeAttributes(a.Attributes)})
	}
	response, handled, err := scheduler.PickAuth(ctx, pluginapi.SchedulerPickRequest{Provider: "codex", Providers: []string{"codex"}, Model: model, Stream: opts.Stream, Options: schedulerOptions(opts), Candidates: candidates})
	if err != nil {
		return nil, true, err
	}
	if !handled || !response.Handled {
		return nil, false, nil
	}
	if a := pickSchedulerAuthByID(auths, response.AuthID); a != nil {
		return a, true, nil
	}
	// Delegation remains local to this candidate pool, never the global scheduler.
	if strategy, ok := builtinSchedulerStrategy(response.DelegateBuiltin); ok {
		var selector Selector = &RoundRobinSelector{}
		if strategy == schedulerStrategyFillFirst {
			selector = &FillFirstSelector{}
		} else if strategy == schedulerStrategyWeightedRoundRobin {
			selector = &WeightedRoundRobinSelector{}
		}
		selected, err := selector.Pick(selectorContextForAvailableAuths(ctx, selector, model), "codex", model, opts, highestPriorityAuths(auths))
		return selected, true, err
	}
	return nil, false, nil
}
func (pools *codexPoolSet) lockSession(ctx context.Context, key string) (*codexAccountPool, error) {
	slot := codexSlot(key)
	for {
		if err := nonNilContext(ctx).Err(); err != nil {
			return nil, err
		}
		p := pools.table.Load().owners[slot]
		p.lock()
		if p.retired || !p.owns(slot) {
			p.mu.Unlock()
			continue
		}
		return p, nil
	}
}
func (p *codexAccountPool) putBindingLocked(key codexBindingKey, value codexPoolBinding) {
	if _, ok := p.bindings[key]; !ok {
		const limit = 4096
		if len(p.bindingOrder) < limit {
			p.bindingOrder = append(p.bindingOrder, key)
		} else {
			old := p.bindingOrder[p.bindingCursor]
			delete(p.bindings, old)
			p.bindingOrder[p.bindingCursor] = key
			p.bindingCursor = (p.bindingCursor + 1) % limit
		}
	}
	p.bindings[key] = value
}
func (m *Manager) poolBoundAuth(ctx context.Context, binding codexPoolBinding, model string, opts exec.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	stale := &Error{Code: "auth_unavailable", Message: staleSelectionMessage, Retryable: true, HTTPStatus: http.StatusServiceUnavailable}
	if _, used := tried[binding.authID]; used {
		return nil, nil, stale
	}
	guard, err := m.lockAuth(ctx, binding.authID)
	if err != nil {
		if errors.Is(err, errCodexOwnerMissing) {
			return nil, nil, stale
		}
		return nil, nil, err
	}
	defer guard.unlock()
	a := guard.auths[binding.authID]
	if guard.pool == nil || a == nil || a.RegistrationEpoch != binding.epoch || !authSelectionEligibilityForRequest(ctx, opts).allows(a) || !m.authSupportsRouteModel(registry.GetGlobalRegistry(), a, model) {
		return nil, nil, stale
	}
	if rateCapRequiresPositiveWeight(m.shared.Load().selector) && authWeight(a) <= 0 {
		return nil, nil, stale
	}
	if blocked, _, _ := isAuthBlockedForModel(a, m.selectionModelForAuth(a, model), time.Now()); blocked {
		return nil, nil, stale
	}
	if ready, _ := m.pooledRateAvailableLocked(guard.pool, a, ctx); !ready {
		return nil, nil, stale
	}
	executor := m.shared.Load().executors[executorKeyFromAuth(a)]
	if executor == nil {
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	return a.Clone(), executor, nil
}
func (m *Manager) poolSessionKey(model string, opts exec.Options) (string, *SessionAffinitySelector) {
	selector, _ := m.shared.Load().selector.(*SessionAffinitySelector)
	if selector == nil {
		return "", nil
	}
	explicit, parent := extractExplicitSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
	if explicit != "" {
		if parent != "" {
			opts.Metadata[exec.ParentSessionIDMetadataKey] = session.BoundSessionIdentity(parent)
		}
		return session.BoundSessionIdentity(explicit), selector
	}
	if selector.matcher != nil {
		namespace := lcpAffinityNamespace("codex", model, opts.Metadata)
		if namespace != "" {
			fingerprints, minPrefix := selector.matcher.Prepare(session.ExtractCanonicalTurns(opts.SourceFormat, opts.OriginalRequest))
			if len(fingerprints) > 0 && minPrefix > 0 {
				opts.Metadata[exec.LCPFingerprintMetadataKey] = fingerprints
				opts.Metadata[exec.LCPMinPrefixLengthMetadataKey] = minPrefix
				if match, ok := selector.matcher.MatchFingerprints(namespace, fingerprints, minPrefix); ok {
					opts.Metadata[exec.LCPAffinitySessionIDMetadataKey] = match.SessionID
					opts.Metadata[exec.CanonicalSessionIDMetadataKey] = match.SessionID
					opts.Metadata[exec.LCPAccessGenerationMetadataKey] = match.AccessNumber
					if match.IsFork {
						opts.Metadata[exec.IsForkMetadataKey] = true
					} else {
						delete(opts.Metadata, exec.IsForkMetadataKey)
					}
					if match.ParentSessionID != "" {
						opts.Metadata[exec.ParentSessionIDMetadataKey] = match.ParentSessionID
					}
					return match.SessionID, selector
				}
			}
		}
	}
	return CanonicalSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata), selector
}
func (m *Manager) bindPoolSelection(ctx context.Context, key, model string, opts exec.Options, affinity *SessionAffinitySelector, selected *Auth, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	pools := m.codexPools.Load()
	ttl := time.Hour
	if affinity != nil && affinity.cache != nil {
		ttl = affinity.cache.ttl
	}
	if fingerprints, minPrefix := lcpFingerprintsFromMetadata(opts.Metadata); affinity != nil && affinity.matcher != nil && len(fingerprints) > 0 && minPrefix > 0 {
		binding := affinity.matcher.BindFingerprintsWithResult(lcpAffinityNamespace("codex", model, opts.Metadata), fingerprints, minPrefix, selected.ID)
		if binding.SessionID != "" {
			key = binding.SessionID
			opts.Metadata[exec.LCPAffinitySessionIDMetadataKey] = key
			opts.Metadata[exec.LCPAccessGenerationMetadataKey] = binding.AccessNumber
		}
	}
	if key == "" {
		return selected, m.executorFor("codex"), nil
	}
	opts.Metadata[exec.CanonicalSessionIDMetadataKey] = key
	bindingKey := codexBindingKey{key, canonicalModelKey(model)}
	for {
		p, err := pools.lockSession(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		current, exists := p.bindings[bindingKey]
		if !exists || !time.Now().Before(current.expires) {
			p.putBindingLocked(bindingKey, codexPoolBinding{selected.ID, selected.RegistrationEpoch, time.Now().Add(ttl)})
			p.mu.Unlock()
			return m.poolBoundAuth(ctx, codexPoolBinding{authID: selected.ID, epoch: selected.RegistrationEpoch}, model, opts, tried)
		}
		p.mu.Unlock()
		if winner, executor, err := m.poolBoundAuth(ctx, current, model, opts, tried); err == nil {
			p, err = pools.lockSession(ctx, key)
			if err != nil {
				return nil, nil, err
			}
			if p.bindings[bindingKey] == current {
				current.expires = time.Now().Add(ttl)
				p.bindings[bindingKey] = current
			}
			p.mu.Unlock()
			return winner, executor, nil
		}
		p, err = pools.lockSession(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		if p.bindings[bindingKey] == current {
			p.putBindingLocked(bindingKey, codexPoolBinding{selected.ID, selected.RegistrationEpoch, time.Now().Add(ttl)})
			p.mu.Unlock()
			return m.poolBoundAuth(ctx, codexPoolBinding{authID: selected.ID, epoch: selected.RegistrationEpoch}, model, opts, tried)
		}
		p.mu.Unlock()
	}
}
func (m *Manager) pickCodexPoolOnce(ctx context.Context, model string, opts exec.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	opts.EnsureMetadata()
	delete(opts.Metadata, poolStickyMetadataKey)
	opts.Metadata[exec.SessionAffinityProviderMetadataKey] = "codex"
	opts.Metadata[exec.SessionAffinityModelMetadataKey] = model
	pools := m.codexPools.Load()
	if pinned := pinnedAuthIDFromMetadata(opts.Metadata); pinned != "" {
		if _, used := tried[pinned]; used {
			return nil, nil, &Error{Code: "auth_not_found", Message: "pinned credential already attempted"}
		}
		snapshot, ok := m.GetByID(pinned)
		if !ok {
			return nil, nil, &Error{Code: "auth_not_found", Message: "pinned credential missing"}
		}
		return m.poolBoundAuth(ctx, codexPoolBinding{authID: pinned, epoch: snapshot.RegistrationEpoch}, model, opts, tried)
	}
	key, affinity := m.poolSessionKey(model, opts)
	if key != "" {
		p, err := pools.lockSession(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		bound, exists := p.bindings[codexBindingKey{key, canonicalModelKey(model)}]
		p.mu.Unlock()
		if exists && time.Now().Before(bound.expires) {
			if a, e, err := m.poolBoundAuth(ctx, bound, model, opts, tried); err == nil {
				opts.Metadata[poolStickyMetadataKey] = true
				return a, e, nil
			}
		}
	}
	if affinity != nil && key != "" {
		parent, _ := opts.Metadata[exec.ParentSessionIDMetadataKey].(string)
		isFork, _ := opts.Metadata[exec.IsForkMetadataKey].(bool)
		if parent != "" && parent != key && (isFork || !isSubagentSession(key, parent) || affinity.subagentAffinity) {
			p, err := pools.lockSession(ctx, parent)
			if err != nil {
				return nil, nil, err
			}
			binding, exists := p.bindings[codexBindingKey{parent, canonicalModelKey(model)}]
			p.mu.Unlock()
			if exists && time.Now().Before(binding.expires) {
				if a, e, err := m.poolBoundAuth(ctx, binding, model, opts, tried); err == nil {
					opts.Metadata[poolStickyMetadataKey] = true
					if preview, _ := ctx.Value(poolPreviewKey{}).(bool); preview {
						return a, e, nil
					}
					return m.bindPoolSelection(ctx, key, model, opts, affinity, a, tried)
				}
			}
		}
	}
	table := pools.table.Load()
	slot := uint16(rand.IntN(codexSlotCount))
	if key != "" {
		slot = codexSlot(key)
	}
	opts.Metadata[poolHintMetadataKey] = slot
	preferred := table.owners[slot]
	order := append([]*codexAccountPool(nil), table.pools...)
	sort.Slice(order, func(i, j int) bool {
		if order[i] == order[j] {
			return false
		}
		if order[i] == preferred {
			return true
		}
		if order[j] == preferred {
			return false
		}
		return poolRendezvous(key, slot, order[i].id) > poolRendezvous(key, slot, order[j].id)
	})
	var chosen *Auth
	var executor ProviderExecutor
	var bestErr error
	var sawSaturated bool
	observeSaturation := func(err error) {
		var soft *poolSaturatedError
		if errors.As(err, &soft) {
			sawSaturated = true
		}
	}
	sideFirst := pools.side.priorityBound(model) > pools.pooledPriorityBound(model)
	if sideFirst {
		chosen, executor, bestErr = m.poolPickOne(ctx, pools.side, model, opts, tried)
		observeSaturation(bestErr)
	}
	if chosen == nil || pools.pooledPriorityBound(model) > authPriority(chosen) {
		for i, p := range order {
			a, e, err := m.poolPickOne(ctx, p, model, opts, tried)
			if err == nil {
				if i > 0 {
					preferred.fallback.Add(1)
					if i > 3 {
						preferred.fullFallback.Add(1)
					}
				}
				if chosen == nil || authPriority(a) > authPriority(chosen) {
					chosen, executor = a, e
				}
				break
			}
			observeSaturation(err)
			bestErr = preferPoolError(bestErr, err)
		}
	}
	if !sideFirst && (chosen == nil || pools.side.priorityBound(model) > authPriority(chosen)) {
		a, e, err := m.poolPickOne(ctx, pools.side, model, opts, tried)
		if err == nil && (chosen == nil || authPriority(a) > authPriority(chosen)) {
			chosen, executor = a, e
		}
		observeSaturation(err)
		bestErr = preferPoolError(bestErr, err)
	}
	if chosen == nil {
		if sawSaturated {
			return nil, nil, &poolSaturatedError{cause: bestErr}
		}
		if bestErr == nil {
			bestErr = &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		return nil, nil, bestErr
	}
	if preview, _ := ctx.Value(poolPreviewKey{}).(bool); affinity != nil && !preview {
		return m.bindPoolSelection(ctx, key, model, opts, affinity, chosen, tried)
	}
	return chosen, executor, nil
}
func poolRendezvous(key string, slot uint16, id uint64) uint64 {
	h := fnv64(fmt.Sprintf("%s:%d:%d", key, slot, id))
	return h
}
func fnv64(value string) uint64 {
	var hash uint64 = 14695981039346656037
	for i := range len(value) {
		hash ^= uint64(value[i])
		hash *= 1099511628211
	}
	return hash
}
func preferPoolError(previous, next error) error {
	if next == nil {
		return previous
	}
	if previous == nil {
		return next
	}
	a, b := retryAfterFromError(previous), retryAfterFromError(next)
	if a != nil && b != nil && *b < *a {
		return next
	}
	if a == nil && b != nil {
		return next
	}
	return previous
}
func hasCodexProvider(providers []string) bool {
	for _, p := range providers {
		if strings.EqualFold(strings.TrimSpace(p), "codex") {
			return true
		}
	}
	return false
}

func (m *Manager) updatePoolAffinity(result Result, epoch uint64) {
	affinity, _ := m.shared.Load().selector.(*SessionAffinitySelector)
	if affinity == nil {
		return
	}
	if _, isLCP := result.Options.Metadata[exec.LCPAffinitySessionIDMetadataKey]; isLCP {
		affinity.OnResult(result)
	}
	key := CanonicalSessionID(result.Options.Headers, result.Options.OriginalRequest, result.Options.Metadata)
	if key == "" {
		return
	}
	model := result.RouteModel
	if model == "" {
		model = result.Model
	}
	p, err := m.codexPools.Load().lockSession(context.Background(), key)
	if err != nil {
		return
	}
	defer p.mu.Unlock()
	bindingKey := codexBindingKey{key, canonicalModelKey(model)}
	binding, ok := p.bindings[bindingKey]
	if !ok || binding.authID != result.AuthID || (epoch != 0 && binding.epoch != epoch) {
		return
	}
	if result.Success {
		binding.expires = time.Now().Add(affinity.cache.ttl)
		p.bindings[bindingKey] = binding
	} else if !shouldSkipCredentialCooldown(result.Error) {
		delete(p.bindings, bindingKey)
	}
}

func (m *Manager) pickCodexPool(ctx context.Context, model string, opts exec.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	opts.EnsureMetadata()
	delete(opts.Metadata, poolSaturatedMetadataKey)
	ctx = nonNilContext(ctx)
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		version := m.codexPools.Load().table.Load().version
		a, e, err := m.pickCodexPoolOnce(ctx, model, opts, tried)
		if nextCtx, nextTried, again := staleSelectionRetry(ctx, err, tried, ""); again {
			ctx, tried = nextCtx, nextTried
			continue
		}
		var saturated *poolSaturatedError
		if errors.As(err, &saturated) {
			ctx = context.WithValue(ctx, poolAllowSaturatedKey{}, true)
			opts.EnsureMetadata()
			opts.Metadata[poolSaturatedMetadataKey] = true
			continue
		}
		if err != nil && version != m.codexPools.Load().table.Load().version {
			continue
		}
		return a, e, m.pooledCapacityAfterPickFailure(ctx, []string{"codex"}, model, opts, err)
	}
}

type poolAllowSaturatedKey struct{}
type poolSaturatedError struct{ cause error }

func (e *poolSaturatedError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return "pool rate window saturated"
}
func (e *poolSaturatedError) Unwrap() error { return e.cause }

const poolSaturatedMetadataKey = "cliproxy.codex_pool_saturated"
