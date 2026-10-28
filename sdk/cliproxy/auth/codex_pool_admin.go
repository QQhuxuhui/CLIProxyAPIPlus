package auth

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type CodexPoolPartitionSnapshot struct {
	FullFallbacks       uint64 `json:"full_fallbacks"`
	ProjectionLagMillis int64  `json:"projection_lag_ms"`
	LockWaits           uint64 `json:"lock_waits"`
	ID                  uint64 `json:"id"`
	Side                bool   `json:"side"`
	Accounts            int    `json:"accounts"`
	Available           int    `json:"available"`
	Slots               int    `json:"slots"`
	LockWaitNanos       int64  `json:"lock_wait_ns"`
	Fallbacks           uint64 `json:"fallbacks"`
	CapacityFailures    uint64 `json:"capacity_failures"`
	SuccessQueue        int    `json:"success_queue"`
	PendingProjections  int    `json:"pending_projections"`
}
type CodexPoolsState struct {
	UnattributedCapacityFailures uint64                       `json:"unattributed_capacity_failures"`
	Configured                   bool                         `json:"configured"`
	RestartRequired              bool                         `json:"restart_required"`
	Enabled                      bool                         `json:"enabled"`
	HomeMode                     bool                         `json:"home_mode"`
	Version                      uint64                       `json:"version"`
	Pools                        []CodexPoolPartitionSnapshot `json:"pools"`
}

func (m *Manager) CodexPoolsSnapshot() CodexPoolsState {
	pools := m.codexPools.Load()
	out := CodexPoolsState{Enabled: pools != nil, HomeMode: m.HomeEnabled(), Pools: []CodexPoolPartitionSnapshot{}}
	out.Configured = m.runtimeConfigSnapshot().Codex.Pools.Enable
	out.RestartRequired = out.Configured != (pools != nil)
	if pools == nil {
		return out
	}
	out.UnattributedCapacityFailures = pools.fastFailures.Load()
	table := pools.table.Load()
	out.Version = table.version
	for _, p := range append(append([]*codexAccountPool{}, table.pools...), pools.side) {
		p.lock()
		item := CodexPoolPartitionSnapshot{ID: p.id, Side: p == pools.side, Accounts: len(p.auths), LockWaitNanos: p.lockWait.Load(), LockWaits: p.lockWaits.Load(), Fallbacks: p.fallback.Load(), FullFallbacks: p.fullFallback.Load(), CapacityFailures: p.fastFailures.Load(), PendingProjections: len(p.dirty)}
		var oldest time.Time
		pending := make(map[string]struct{})
		for id, at := range p.dirtySince {
			pending[id] = struct{}{}
			if oldest.IsZero() || at.Before(oldest) {
				oldest = at
			}
		}
		for id, at := range p.inflightSince {
			pending[id] = struct{}{}
			if oldest.IsZero() || at.Before(oldest) {
				oldest = at
			}
		}
		item.PendingProjections = len(pending)
		if !oldest.IsZero() {
			item.ProjectionLagMillis = time.Since(oldest).Milliseconds()
		}
		for slot := range codexSlotCount {
			if p.owns(uint16(slot)) {
				item.Slots++
			}
		}
		for _, a := range p.auths {
			if blocked, _, _ := isAuthBlockedForModel(a, "", time.Now()); !blocked {
				if ready, _ := m.pooledRateAvailableLocked(p, a, context.Background()); ready {
					item.Available++
				}
			}
		}
		p.mu.Unlock()
		p.results.mu.Lock()
		item.SuccessQueue = len(p.results.queue)
		p.results.mu.Unlock()
		out.Pools = append(out.Pools, item)
	}
	return out
}
func (m *Manager) aggregateCodexPoolSnapshot() CodexPoolSnapshot {
	pools := m.codexPools.Load()
	cfg := m.runtimeConfigSnapshot()
	out := CodexPoolSnapshot{Config: cfg.ProviderRequestRateCap["codex"].WithDefaults(), HomeMode: m.HomeEnabled(), RPMAvailable: !m.HomeEnabled(), CoolingReasons: make(map[string]int), Points: []PoolPoint{}, Limits: []PoolLimitBucket{}}
	out.AlgorithmActive = out.Config.Enabled && !out.HomeMode
	points := make(map[time.Time]PoolPoint)
	limits := make(map[int]int)
	pools.all.Range(func(_, value any) bool {
		p := value.(*codexAccountPool)
		p.lock()
		snap := snapshotCodexPoolLocked(p.auths, p.rate)
		p.mu.Unlock()
		out.Schedulable += snap.Schedulable
		out.Cooling += snap.Cooling
		out.Disabled += snap.Disabled
		out.Expired += snap.Expired
		out.Full += snap.Full
		out.Capacity += snap.Capacity
		out.RPM += snap.RPM
		out.Remaining += snap.Remaining
		for reason, count := range snap.CoolingReasons {
			out.CoolingReasons[reason] += count
		}
		for _, bucket := range snap.Limits {
			limits[bucket.Limit] += bucket.Accounts
		}
		for _, point := range snap.Points {
			combined := points[point.Time]
			combined.Time = point.Time
			combined.Attempts += point.Attempts
			combined.Capacity += point.Capacity
			combined.Success += point.Success
			combined.Overload += point.Overload
			combined.RateLimits += point.RateLimits
			combined.QuotaExhausted += point.QuotaExhausted
			combined.OtherFailures += point.OtherFailures
			combined.FastFailures += point.FastFailures
			for i, n := range point.histogram {
				combined.histogram[i] += n
			}
			points[point.Time] = combined
		}
		return true
	})
	var histogram [len(poolTTFTBounds)]int
	success, total, overloads := 0, 0, 0
	now := time.Now().Truncate(time.Minute)
	for i := 59; i >= 0; i-- {
		at := now.Add(-time.Duration(i) * time.Minute)
		point := points[at]
		point.Time = at
		point.TTFTP50 = poolPercentile(point.histogram, .5)
		point.TTFTP90 = poolPercentile(point.histogram, .9)
		out.Points = append(out.Points, point)
		out.FastFailures += point.FastFailures
		if i < 5 {
			success += point.Success
			overloads += point.Overload
			total += point.Success + point.Overload + point.RateLimits + point.QuotaExhausted + point.OtherFailures
			for j, n := range point.histogram {
				histogram[j] += n
			}
		}
	}
	if out.HomeMode {
		out.Capacity = 0
		out.Schedulable = 0
		out.Full = 0
		out.Remaining = 0
	}
	out.EffectiveCapacity = float64(out.Capacity)
	if total > 0 {
		out.SuccessRate = float64(success) / float64(total)
		out.EffectiveCapacity *= 1 - float64(overloads)/float64(total)
	}
	out.TTFTP50 = poolPercentile(histogram, .5)
	out.TTFTP90 = poolPercentile(histogram, .9)
	for limit, count := range limits {
		out.Limits = append(out.Limits, PoolLimitBucket{limit, count})
	}
	sort.Slice(out.Limits, func(i, j int) bool { return out.Limits[i].Limit < out.Limits[j].Limit })
	return out
}
func (m *Manager) recordPoolFailureWithOptions(err error, opts exec.Options) {
	pools := m.codexPools.Load()
	if pools == nil {
		m.recordPoolFailure(err)
		return
	}
	if !isPoolCapacityError(err) {
		return
	}
	slot, hasSlot := opts.Metadata[poolHintMetadataKey].(uint16)
	if !hasSlot && pinnedAuthIDFromMetadata(opts.Metadata) == "" {
		pools.fastFailures.Add(1)
		return
	}
	p := pools.table.Load().owners[slot]
	if id := pinnedAuthIDFromMetadata(opts.Metadata); id != "" {
		if pools.owners.entry(id).lease.kind == codexOwnerSide {
			p = pools.side
		} else {
			p = pools.table.Load().owners[codexSlot(id)]
		}
	}
	p.fastFailures.Add(1)
	p.lock()
	p.rate.metrics.bucket(p.rate.nowFunc()).FastFailures++
	p.mu.Unlock()
}
func (m *Manager) pooledUsage(ctx context.Context, record usage.Record) {
	if record.Provider != "codex" {
		return
	}
	guard, err := m.lockAuth(context.WithoutCancel(nonNilContext(ctx)), record.AuthID)
	if err != nil {
		return
	}
	defer guard.unlock()
	if guard.pool != nil {
		guard.pool.rate.handleUsageLocked(ctx, record)
	}
}
func (m *Manager) acquirePooledProbeAuth(model string) (*Auth, string) {
	pools := m.codexPools.Load()
	var best *Auth
	bestRemaining := 0
	for _, p := range pools.table.Load().pools {
		p.lock()
		for _, a := range p.auths {
			if a.AuthKind() == AuthKindAPIKey || strings.TrimSpace(authAttribute(a, AttributeAPIKey)) != "" {
				continue
			}
			if token, _ := a.Metadata["access_token"].(string); token == "" {
				continue
			}
			if blocked, _, _ := isAuthBlockedForModel(a, model, time.Now()); blocked {
				continue
			}
			if !m.authSupportsRouteModel(registry.GetGlobalRegistry(), a, model) {
				continue
			}
			count, limit, _ := p.rate.accountLocked(a.ID, p.rate.nowFunc())
			if limit-count > bestRemaining {
				best = a.Clone()
				bestRemaining = limit - count
			}
		}
		p.mu.Unlock()
	}
	if best == nil {
		return nil, "no schedulable codex oauth credential"
	}
	guard, err := m.lockAuth(context.Background(), best.ID)
	if err != nil {
		return nil, "credential changed"
	}
	defer guard.unlock()
	current := guard.auths[best.ID]
	if guard.pool == nil || current == nil || current.RegistrationEpoch != best.RegistrationEpoch {
		return nil, "credential changed"
	}
	r := guard.pool.rate
	count, limit, _ := r.accountLocked(best.ID, r.nowFunc())
	if count >= limit {
		return nil, "all codex oauth credentials at rate window limit"
	}
	token := codexPoolRateSequence.Add(1)
	r.getLocked(best.ID, r.nowFunc()).attempts = append(r.getLocked(best.ID, r.nowFunc()).attempts, rateAttempt{id: token, at: r.nowFunc()})
	best = current.Clone()
	best.codexProbeAttempt = &rateAttemptRef{authID: best.ID, epoch: best.RegistrationEpoch}
	best.codexProbeAttempt.token.Store(token)
	return best, ""
}
