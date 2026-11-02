package auth

import (
	"container/list"
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
	candidates, selector, across, err := m.poolCandidates(ctx, p, model, opts, tried)
	if err != nil {
		return nil, nil, err
	}
	shared := m.shared.Load()
	if !across {
		candidates = highestPriorityAuths(candidates)
	}
	selected, handled, err := m.poolPluginPick(ctx, shared.pluginScheduler, model, opts, candidates)
	if err != nil {
		return nil, nil, err
	}
	if handled && selected != nil {
		notePick(ctx, selected.ID)
	} else {
		notePick(ctx, "")
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

// poolCandidates snapshots this pool's eligible candidates under its lock: eligibility, model,
// cooldown, concurrency and RPM filters, across priorities. across reports whether the plugin
// scheduler wants candidates of every priority.
func (m *Manager) poolCandidates(ctx context.Context, p *codexAccountPool, model string, opts exec.Options, tried map[string]struct{}) ([]*Auth, Selector, bool, error) {
	if err := nonNilContext(ctx).Err(); err != nil {
		return nil, nil, false, err
	}
	shared := m.shared.Load()
	if hint := p.availability.Load(); hint != nil && pinnedAuthIDFromMetadata(opts.Metadata) == "" && len(tried) == 0 && hint.potential == 0 && !rateBypassed(ctx) && time.Now().Before(hint.until) {
		delay := time.Until(hint.until)
		if hint.capacity {
			return nil, nil, false, newPoolCapacityError(delay)
		}
		return nil, nil, false, newModelCooldownErrorWithCause(model, "codex", delay, nil)
	}
	across := false
	if plugin, ok := shared.pluginScheduler.(PluginSchedulerAcrossPriorities); ok {
		across = plugin.SchedulerWantsAcrossPriorities()
	}
	p.lock()
	if p.retired {
		p.mu.Unlock()
		return nil, nil, false, &Error{Code: "auth_not_found", Message: "pool retired"}
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
	concurrencyActive := concurrencyFilterActive(ctx, m.accountConcurrency)
	concurrencyFull, concurrencyReady := false, false
	pickTime := time.Now()
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
		// Eligibility, model and cooldown come first, then concurrency, then RPM.
		if concurrencyActive {
			if blocked, _, _ := isAuthBlockedForModel(a, m.selectionModelForAuth(a, model), pickTime); !blocked {
				if m.accountConcurrency.full(a) {
					concurrencyFull = true
					continue
				}
				concurrencyReady = true
			}
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
	if concurrencyFull && !concurrencyReady {
		p.mu.Unlock()
		return nil, nil, false, newConcurrencyCapacityError()
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
		return nil, nil, false, err
	}
	candidates = snapshotAuthsForSelection(selector, available)
	p.mu.Unlock()
	return candidates, selector, across, nil
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
	if p.bindingEntries == nil {
		p.bindingEntries = make(map[codexBindingKey]*list.Element)
	}
	if element := p.bindingEntries[key]; element != nil {
		p.bindingOrder.MoveToBack(element)
	} else {
		if len(p.bindings) >= 4096 {
			p.deleteBindingLocked(p.bindingOrder.Front().Value.(codexBindingKey))
		}
		p.bindingEntries[key] = p.bindingOrder.PushBack(key)
	}
	p.bindings[key] = value
}
func (p *codexAccountPool) deleteBindingLocked(key codexBindingKey) {
	delete(p.bindings, key)
	if element := p.bindingEntries[key]; element != nil {
		p.bindingOrder.Remove(element)
		delete(p.bindingEntries, key)
	}
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
	if m.accountConcurrencyFull(ctx, a) {
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

// bindPoolSelection binds the session to selected. replaceIf, when set, is a binding to an
// account outside the fast group that the speed strategy replaces if it is still unchanged.
func (m *Manager) bindPoolSelection(ctx context.Context, key, model string, opts exec.Options, affinity *SessionAffinitySelector, selected *Auth, tried map[string]struct{}, replaceIf *codexPoolBinding) (*Auth, ProviderExecutor, error) {
	pools := m.codexPools.Load()
	ttl := time.Hour
	if affinity != nil && affinity.cache != nil {
		ttl = affinity.cache.ttl
	}
	speed := CodexSpeed()
	originalKey := key
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
		if replaceIf != nil && exists && current == *replaceIf {
			// Speed-strategy replacement of a binding outside the fast group, still unchanged.
			p.putBindingLocked(bindingKey, codexPoolBinding{authID: selected.ID, epoch: selected.RegistrationEpoch, expires: time.Now().Add(ttl)})
			p.mu.Unlock()
			speed.sessions.markRebound(originalKey, ttl, speed.now())
			if key != originalKey {
				speed.sessions.markRebound(key, ttl, speed.now())
			}
			return m.poolBoundAuth(ctx, codexPoolBinding{authID: selected.ID, epoch: selected.RegistrationEpoch}, model, opts, tried)
		}
		if !exists || !time.Now().Before(current.expires) {
			p.putBindingLocked(bindingKey, codexPoolBinding{authID: selected.ID, epoch: selected.RegistrationEpoch, expires: time.Now().Add(ttl)})
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
				p.putBindingLocked(bindingKey, current)
			}
			p.mu.Unlock()
			return winner, executor, nil
		}
		p, err = pools.lockSession(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		if p.bindings[bindingKey] == current {
			p.putBindingLocked(bindingKey, codexPoolBinding{authID: selected.ID, epoch: selected.RegistrationEpoch, expires: time.Now().Add(ttl)})
			p.mu.Unlock()
			return m.poolBoundAuth(ctx, codexPoolBinding{authID: selected.ID, epoch: selected.RegistrationEpoch}, model, opts, tried)
		}
		p.mu.Unlock()
	}
}
func (m *Manager) pickCodexPoolOnce(ctx context.Context, model string, opts exec.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	opts.EnsureMetadata()
	delete(opts.Metadata, poolStickyMetadataKey)
	delete(opts.Metadata, poolSpeedReplaceMetadataKey)
	opts.Metadata[exec.SessionAffinityProviderMetadataKey] = "codex"
	opts.Metadata[exec.SessionAffinityModelMetadataKey] = model
	pools := m.codexPools.Load()
	if pinned := pinnedAuthIDFromMetadata(opts.Metadata); pinned != "" {
		if _, used := tried[pinned]; used {
			return nil, nil, &Error{Code: "auth_not_found", Message: "pinned credential already attempted"}
		}
		guard, err := m.lockAuth(ctx, pinned)
		if errors.Is(err, errCodexOwnerMissing) {
			return nil, nil, &Error{Code: "auth_not_found", Message: "pinned credential missing"}
		}
		if err != nil {
			return nil, nil, err
		}
		defer guard.unlock()
		a := guard.auths[pinned]
		if a == nil || guard.pool == nil || a.Disabled || a.Status == StatusDisabled || !authSelectionEligibilityForRequest(ctx, opts).allows(a) || !m.authSupportsRouteModel(registry.GetGlobalRegistry(), a, model) || (rateCapRequiresPositiveWeight(m.shared.Load().selector) && authWeight(a) <= 0) {
			return nil, nil, &Error{Code: "auth_not_found", Message: "pinned credential unavailable"}
		}
		if blocked, _, _ := isAuthBlockedForModel(a, m.selectionModelForAuth(a, model), time.Now()); !blocked && m.accountConcurrencyFull(ctx, a) {
			return nil, nil, newConcurrencyCapacityError()
		}
		if ready, next := m.pooledRateAvailableLocked(guard.pool, a, ctx); !ready && guard.pool.rate.config.FailFast {
			return nil, nil, newPoolCapacityError(next.Sub(guard.pool.rate.nowFunc()))
		}
		if _, err = m.availableAuthsForRouteModelAcrossPriorities([]*Auth{a}, "codex", model, time.Now()); err != nil {
			return nil, nil, err
		}
		executor := m.shared.Load().executors[executorKeyFromAuth(a)]
		if executor == nil {
			return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
		}
		return a.Clone(), executor, nil
	}
	key, affinity := m.poolSessionKey(model, opts)
	preview, _ := ctx.Value(poolPreviewKey{}).(bool)
	speed := CodexSpeed()
	ranked := speed.selecting()
	table := pools.table.Load()
	slot := uint16(rand.IntN(codexSlotCount))
	if key != "" {
		slot = codexSlot(key)
	}
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
	if key != "" {
		p, err := pools.lockSession(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		bound, exists := p.bindings[codexBindingKey{key, canonicalModelKey(model)}]
		p.mu.Unlock()
		if exists && time.Now().Before(bound.expires) {
			// A binding that is no longer usable falls through to a full pick (failover); that
			// does not spend the session's speed replacement.
			if a, e, err := m.poolBoundAuth(ctx, bound, model, opts, tried); err == nil {
				if ranked && !speed.inFast(bound.authID, speed.now()) && !speed.sessions.rebound(key, speed.now()) {
					// Speed up the session once: replace the binding, but only with a fast account.
					fastTried := make(map[string]struct{}, len(tried)+1)
					for id := range tried {
						fastTried[id] = struct{}{}
					}
					fastTried[bound.authID] = struct{}{}
					if fast, fastExec, errFast := m.pickCodexPoolRanked(ctx, order, preferred, model, opts, fastTried, true); errFast == nil {
						b := bound
						if preview {
							// The mixed-provider caller binds once it settles on Codex.
							opts.Metadata[poolSpeedReplaceMetadataKey] = b
							return fast, fastExec, nil
						}
						if affinity != nil {
							return m.bindPoolSelection(ctx, key, model, opts, affinity, fast, tried, &b)
						}
						return fast, fastExec, nil
					}
				}
				notePick(ctx, "")
				if !preview {
					// Reusing the binding keeps its replacement mark alive with it.
					speed.sessions.touch(key, codexSpeedMarkTTL(affinity), speed.now())
				}
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
					notePick(ctx, "")
					opts.Metadata[poolStickyMetadataKey] = true
					if preview {
						return a, e, nil
					}
					return m.bindPoolSelection(ctx, key, model, opts, affinity, a, tried, nil)
				}
			}
		}
	}
	opts.Metadata[poolHintMetadataKey] = slot
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
		if ranked {
			a, e, err := m.pickCodexPoolRanked(ctx, order, preferred, model, opts, tried, false)
			if err == nil {
				if chosen == nil || authPriority(a) > authPriority(chosen) {
					chosen, executor = a, e
				}
			} else {
				observeSaturation(err)
				bestErr = preferPoolError(bestErr, err)
			}
		} else {
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
	if affinity != nil && !preview {
		return m.bindPoolSelection(ctx, key, model, opts, affinity, chosen, tried, nil)
	}
	return chosen, executor, nil
}

// codexSpeedMarkTTL is the lifetime of a session's replacement mark: the binding TTL.
func codexSpeedMarkTTL(affinity *SessionAffinitySelector) time.Duration {
	if affinity != nil && affinity.cache != nil {
		return affinity.cache.ttl
	}
	return codexSpeedSessionTTL
}

// poolSpeedReplaceMetadataKey carries a binding that a mixed-provider preview decided to replace
// with a fast account; the caller passes it to bindPoolSelection once it settles on Codex.
const poolSpeedReplaceMetadataKey = "cliproxy.codex_speed_replace"

// errSpeedNoFast reports that a fast-only pick found no available fast account.
var errSpeedNoFast = &Error{Code: "auth_not_found", Message: "no fast account available"}

// pickCodexPoolRanked gathers the candidates of every pool and picks by the speed ranking: a
// random fast account, otherwise the best-ranked overflow account. fastOnly never overflows and
// leaves the choice to a plugin scheduler (reported as errSpeedNoFast). A plugin scheduler keeps
// its contract: it sees candidates across priorities when it asks for them, otherwise the top
// priority; the built-in ranking always picks within the global top priority.
func (m *Manager) pickCodexPoolRanked(ctx context.Context, order []*codexAccountPool, preferred *codexAccountPool, model string, opts exec.Options, tried map[string]struct{}, fastOnly bool) (*Auth, ProviderExecutor, error) {
	for {
		var all []*Auth
		owner := map[string]int{}
		var bestErr error
		sawSaturated, across := false, false
		for i, p := range order {
			candidates, _, wantsAcross, err := m.poolCandidates(ctx, p, model, opts, tried)
			if err != nil {
				var soft *poolSaturatedError
				if errors.As(err, &soft) {
					sawSaturated = true
				}
				bestErr = preferPoolError(bestErr, err)
				continue
			}
			across = across || wantsAcross
			for _, a := range candidates {
				owner[a.ID] = i
			}
			all = append(all, candidates...)
		}
		if len(all) == 0 {
			if bestErr == nil {
				bestErr = &Error{Code: "auth_not_found", Message: "no auth available"}
			}
			if sawSaturated {
				return nil, nil, &poolSaturatedError{cause: bestErr}
			}
			return nil, nil, bestErr
		}
		pluginCandidates := all
		if !across {
			pluginCandidates = highestPriorityAuths(all)
		}
		selected, handled, err := m.poolPluginPick(ctx, m.shared.Load().pluginScheduler, model, opts, pluginCandidates)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case handled && fastOnly:
			return nil, nil, errSpeedNoFast
		case handled && selected != nil:
			notePick(ctx, selected.ID)
		default:
			selected = CodexSpeed().pickRanked(preferCodexWebsocketAuths(ctx, "codex", highestPriorityAuths(all)), fastOnly)
			if selected == nil {
				if fastOnly {
					return nil, nil, errSpeedNoFast
				}
				return nil, nil, &Error{Code: "auth_not_found", Message: "pool selector returned no credential"}
			}
			notePick(ctx, "")
		}
		a, e, err := m.resolveSelectionSnapshot(ctx, selected, model, opts)
		if nextCtx, nextTried, again := staleSelectionRetry(ctx, err, tried, selected.ID); again {
			ctx, tried = nextCtx, nextTried
			continue
		}
		if err == nil && preferred != nil {
			if i := owner[selected.ID]; i > 0 {
				preferred.fallback.Add(1)
				if i > 3 {
					preferred.fullFallback.Add(1)
				}
			}
		}
		return a, e, err
	}
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
	if isConcurrencyCapacityError(previous) {
		return previous
	}
	if isConcurrencyCapacityError(next) {
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
		p.putBindingLocked(bindingKey, binding)
		speed := CodexSpeed()
		speed.sessions.touch(key, affinity.cache.ttl, speed.now())
	} else if !shouldSkipCredentialCooldown(result.Error) {
		p.deleteBindingLocked(bindingKey)
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
