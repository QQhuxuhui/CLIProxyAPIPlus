package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	log "github.com/sirupsen/logrus"
)

func (m *Manager) configureCodexPools(cfg *internalconfig.Config) {
	if cfg.Codex.Pools.Enable && !cfg.Home.Enabled && m.codexPools.Load() == nil {
		m.initializeCodexPools(1)
	}
	pools := m.codexPools.Load()
	if pools == nil {
		return
	}
	options := cfg.Codex.Pools.WithDefaults()
	pools.config.Store(&options)
	for _, p := range append(append([]*codexAccountPool{}, pools.table.Load().pools...), pools.side) {
		p.lock()
		p.rate.configure(cfg.ProviderRequestRateCap["codex"])
		p.catalogDirty.Store(true)
		p.availability.Store(nil)
		p.mu.Unlock()
	}
}
func (pools *codexPoolSet) run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastRebalance := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if now.Sub(lastRebalance) >= time.Duration(pools.config.Load().RebalanceIntervalSeconds)*time.Second {
				if err := pools.rebalance(ctx); err != nil && ctx.Err() == nil {
					log.Warnf("codex pool rebalance: %v", err)
				}
				lastRebalance = now
			}
			if err := pools.publishProjections(ctx); err != nil && ctx.Err() == nil {
				log.Warnf("codex pool projection: %v", err)
			}
			if err := pools.saveCooldowns(ctx, false); err != nil && ctx.Err() == nil {
				log.Warnf("codex pool cooldown persistence: %v", err)
			}
			pools.reapRetired(now)

		}
	}
}
func nextPoolProjectionAt(a *Auth, now time.Time) time.Time {
	var next time.Time
	consider := func(t time.Time) {
		if t.After(now) && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	if expiry, ok := a.AccessTokenExpirationTime(); ok {
		consider(expiry)
	}
	consider(a.NextRetryAfter)
	consider(a.Quota.NextRecoverAt)
	for _, state := range a.ModelStates {
		if state != nil {
			consider(state.NextRetryAfter)
			consider(state.Quota.NextRecoverAt)
		}
	}
	return next
}
func (pools *codexPoolSet) publishProjections(ctx context.Context) error {
	ctx = nonNilContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-pools.publishing:
	}
	defer func() { pools.publishing <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	m := pools.manager
	now := time.Now()
	reg := registry.GetGlobalRegistry()
	var tasks []registry.ClientProjectionUpdate
	queuedAt := make(map[string]time.Time)
	// A canceled publication retains IDs for the next worker. Rebuilding from
	// current owned state avoids retaining stale snapshots across registrations.
	defer func() {
		if ctx.Err() != nil {
			for _, task := range tasks {
				pools.queueProjectionRetry(task.ClientID, task.AuthEpoch, queuedAt[task.ClientID])
			}
		}
	}()
	pools.projectionRetry.Range(func(key, value any) bool {
		id := key.(string)
		guard, err := m.lockAuth(ctx, id)
		if err != nil {
			if errors.Is(err, errCodexOwnerMissing) {
				pools.projectionRetry.Delete(id)
			}
			return ctx.Err() == nil
		}
		if guard.pool != nil {
			retry := value.(codexProjectionRetry)
			if a := guard.auths[id]; a != nil && a.RegistrationEpoch == retry.epoch {
				guard.pool.markDirtyAtLocked(id, retry.since)
			}
		}
		guard.unlock()
		pools.projectionRetry.Delete(id)
		return true
	})
	if err := ctx.Err(); err != nil {
		return err
	}

	for _, p := range append(append([]*codexAccountPool{}, pools.table.Load().pools...), pools.side) {
		p.lock()
		for id, due := range p.projectionDue {
			if !due.IsZero() && !now.Before(due) {
				p.markDirtyAtLocked(id, due)
				delete(p.projectionDue, id)
			}
		}
		dirty := p.dirty
		p.dirty = make(map[string]struct{})
		for id := range dirty {
			a := p.auths[id]
			since := p.dirtySince[id]
			if since.IsZero() {
				since = now
			}
			queuedAt[id] = since
			delete(p.dirtySince, id)
			if old := p.inflightSince[id]; old.IsZero() || since.Before(old) {
				p.inflightSince[id] = since
			}
			if a == nil {
				delete(p.inflightSince, id)
				continue
			}
			models, epoch := reg.GetModelsAndEpochForClient(id)
			task := registry.ClientProjectionUpdate{ClientID: id, AuthEpoch: a.RegistrationEpoch, Epoch: epoch, Generation: a.Generation}
			for _, model := range models {
				if model != nil {
					task.Projections = append(task.Projections, m.clientModelProjectionForPoolAuth(a, model.ID, now))
				}
			}
			if due := nextPoolProjectionAt(a, now); !due.IsZero() {
				p.projectionDue[id] = due
			}
			if len(task.Projections) > 0 {
				tasks = append(tasks, task)
			} else {
				delete(p.inflightSince, id)
			}
		}
		if p.catalogDirty.Load() {
			p.refreshCatalogLocked()
		}
		for key, binding := range p.bindings {
			if !now.Before(binding.expires) {
				p.deleteBindingLocked(key)
			}
		}
		p.mu.Unlock()
	}
	valid := tasks[:0]
	for _, task := range tasks {
		guard, err := m.lockAuth(ctx, task.ClientID)
		if err != nil {
			continue
		}
		a := guard.auths[task.ClientID]
		ok := a != nil && a.RegistrationEpoch == task.AuthEpoch
		guard.unlock()
		if ok {
			valid = append(valid, task)
		}
	}
	applied := reg.ApplyClientModelProjectionBatch(valid)
	for i, ok := range applied {
		if !ok {
			pools.queueProjectionRetry(valid[i].ClientID, valid[i].AuthEpoch, queuedAt[valid[i].ClientID])
		}
	}

	failed := make(map[string]bool)
	for i, ok := range applied {
		if !ok {
			failed[valid[i].ClientID] = true
		}
	}
	for _, task := range valid {
		if !failed[task.ClientID] {
			m.projectionCompleted(ctx, task.ClientID, task.AuthEpoch)
		}
	}
	return nonNilContext(ctx).Err()
}

func (m *Manager) clientModelProjectionForPoolAuth(auth *Auth, model string, now time.Time) registry.ClientModelProjection {
	projection := m.clientModelProjectionForAuth(auth, model, now)
	blocked, _, _ := isAuthBlockedForModel(auth, m.selectionModelForAuth(auth, model), now)
	if !blocked {
		projection.Suspended = false
		projection.SuspendReason = ""
		projection.QuotaExceeded = false
		projection.QuotaRecoverAt = time.Time{}
	} else {
		projection.Suspended = true
		if projection.SuspendReason == "" {
			projection.SuspendReason = "unavailable"
		}
	}
	return projection
}

// Immediate legacy updates also carry their auth epoch when pooling is active;
// a delayed legacy result must not overwrite a replacement in a pooled container.
func (m *Manager) applyRegistryProjection(snapshot *Auth, epoch uint64, projections []registry.ClientModelProjection) {
	if snapshot == nil {
		return
	}
	reg := registry.GetGlobalRegistry()
	if m.codexPools.Load() == nil {
		reg.ApplyClientModelProjections(snapshot.ID, epoch, snapshot.Generation, projections)
		return
	}
	reg.ApplyClientModelProjectionBatch([]registry.ClientProjectionUpdate{{ClientID: snapshot.ID, AuthEpoch: snapshot.RegistrationEpoch, Epoch: epoch, Generation: snapshot.Generation, Projections: projections}})
}

func (pools *codexPoolSet) saveCooldowns(ctx context.Context, force bool) (err error) {
	ctx = nonNilContext(ctx)
	pending := pools.cooldownDirty.Swap(false)
	if !force && !pending {
		return nil
	}
	defer func() {
		if err != nil {
			pools.cooldownDirty.Store(true)
		}
	}()
	if err = ctx.Err(); err != nil {
		return err
	}
	m := pools.manager
	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	if cfg := m.runtimeConfigSnapshot(); cfg == nil || !cfg.SaveCooldownStatus {
		return nil
	}
	store := m.shared.Load().cooldownStore
	if store == nil {
		return nil
	}
	records, err := m.cooldownStateRecordsSnapshotContext(ctx)
	if err != nil {
		return err
	}
	if err = store.Save(ctx, records); err != nil {
		return fmt.Errorf("save pooled cooldowns: %w", err)
	}
	return ctx.Err()
}

// FlushCodexPools is called after request/result draining, before stores and
// plugins are stopped, so the last asynchronous cooldown/projection is retained.
func (m *Manager) FlushCodexPools(ctx context.Context) error {
	pools := m.codexPools.Load()
	if pools == nil {
		return nil
	}
	if err := pools.publishProjections(ctx); err != nil {
		return err
	}
	return pools.saveCooldowns(ctx, true)
}
func (pools *codexPoolSet) reapRetired(now time.Time) {
	pools.all.Range(func(key, value any) bool {
		p := value.(*codexAccountPool)
		p.lock()
		expired := p.retired && !p.retiredAt.IsZero() && now.Sub(p.retiredAt) > time.Hour
		p.mu.Unlock()
		if !expired {
			return true
		}
		b := &p.results
		b.mu.Lock()
		finished := b.queue == nil
		if b.done != nil {
			select {
			case <-b.done:
				finished = true
			default:
			}
		}
		if finished {
			b.queue = nil
			b.done = nil
		}
		b.mu.Unlock()
		if finished {
			pools.all.CompareAndDelete(key, p)
		}
		return true
	})
}
