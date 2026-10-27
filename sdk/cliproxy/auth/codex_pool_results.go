package auth

import (
	"context"
	"time"
)

func (m *Manager) poolResultOwner(ctx context.Context, id string) (*codexAccountPool, error) {
	pools := m.codexPools.Load()
	lease, err := pools.owners.resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	switch lease.kind {
	case codexOwnerSide:
		return pools.side, nil
	case codexOwnerPooled:
		return pools.table.Load().owners[codexSlot(id)], nil
	default:
		return nil, nil
	}
}
func (m *Manager) queuePoolResult(item queuedResult, owner *codexAccountPool) bool {
	if !m.poolAsyncResults.Load() || m.poolResultsStopped.Load() {
		return false
	}
	b := &m.successResults
	if owner != nil {
		b = &owner.results
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped || m.poolResultsStopped.Load() || !m.poolAsyncResults.Load() {
		return false
	}
	if b.queue == nil {
		b.queue = make(chan queuedResult, successResultQueueSize)
		b.done = make(chan struct{})
		go m.runPoolResultApplier(owner, b.queue, b.done)
	}
	select {
	case b.queue <- item:
		return true
	default:
		return false
	}
}
func (m *Manager) enqueuePoolSuccess(ctx context.Context, result Result, auth *Auth) bool {
	if !m.poolAsyncResults.Load() || m.poolResultsStopped.Load() {
		return false
	}
	ctx = context.WithoutCancel(nonNilContext(ctx))
	owner, err := m.poolResultOwner(ctx, result.AuthID)
	if err != nil {
		return false
	}
	item := queuedResult{ctx: ctx, result: result, completedAt: time.Now()}
	if auth != nil {
		item.registrationEpoch = auth.RegistrationEpoch
	}
	return m.queuePoolResult(item, owner)
}
func (m *Manager) runPoolResultApplier(owner *codexAccountPool, queue <-chan queuedResult, done chan<- struct{}) {
	defer close(done)
	batch := make([]queuedResult, 0, successResultBatchMax)
	for first := range queue {
		batch = append(batch[:0], first)
	drain:
		for len(batch) < successResultBatchMax {
			select {
			case item, ok := <-queue:
				if !ok {
					break drain
				}
				batch = append(batch, item)
			default:
				break drain
			}
		}
		m.applyPoolResultBatch(owner, batch)
		for _, item := range batch {
			if item.done != nil {
				close(item.done)
			}
		}
	}
}
func (m *Manager) applyPoolResultBatch(origin *codexAccountPool, batch []queuedResult) {
	for _, item := range batch {
		if item.done != nil {
			continue
		}
		owner, err := m.poolResultOwner(item.ctx, item.result.AuthID)
		if err != nil {
			continue
		}
		if owner != origin && m.queuePoolResult(item, owner) {
			continue
		}
		ctx, result, ok := m.prepareMarkResult(item.ctx, item.result, false)
		if !ok {
			continue
		}
		guard, err := m.lockAuth(ctx, result.AuthID)
		if err != nil {
			continue
		}
		current := guard.auths[result.AuthID]
		if current == nil || (item.registrationEpoch != 0 && current.RegistrationEpoch != item.registrationEpoch) {
			guard.unlock()
			continue
		}
		var update markResultUpdate
		if current.lastFailureAt.After(item.completedAt) {
			update = m.markStaleSuccessLocked(current, result)
		} else {
			update = m.markAuthResultLocked(ctx, result, canonicalModelKey(result.Model), false, current)
		}
		update.pooled = guard.pool != nil
		if guard.pool != nil {
			guard.pool.markDirtyLocked(result.AuthID)
		}
		guard.unlock()
		m.finishMarkResult(ctx, result, false, update)
	}
}
func (m *Manager) poolResultBatchers() []*successResultBatcher {
	out := []*successResultBatcher{&m.successResults}
	if pools := m.codexPools.Load(); pools != nil {
		pools.all.Range(func(_, value any) bool { out = append(out, &value.(*codexAccountPool).results); return true })
	}
	return out
}
func (m *Manager) stopPoolResults(ctx context.Context) error {
	m.poolResultsStopped.Store(true)
	m.poolAsyncResults.Store(false)
	var completions []<-chan struct{}
	for _, b := range m.poolResultBatchers() {
		b.mu.Lock()
		if !b.stopped {
			b.stopped = true
			b.enabled = false
			if b.queue != nil {
				close(b.queue)
			}
		}
		if b.done != nil {
			completions = append(completions, b.done)
		}
		b.mu.Unlock()
	}
	for _, done := range completions {
		select {
		case <-done:
		case <-nonNilContext(ctx).Done():
			return ctx.Err()
		}
	}
	return nil
}

func (m *Manager) flushPoolResults() {
	pools := m.codexPools.Load()
	pools.topology.Lock()
	defer pools.topology.Unlock()
	// With topology frozen, an old queue redirects each item at most once. The
	// second pass also observes items forwarded after the destination's first marker.
	for range 2 {
		for _, b := range m.poolResultBatchers() {
			b.mu.Lock()
			if b.stopped {
				done := b.done
				b.mu.Unlock()
				if done != nil {
					<-done
				}
				continue
			}
			if b.queue == nil {
				b.mu.Unlock()
				continue
			}
			done := make(chan struct{})
			b.queue <- queuedResult{done: done}
			b.mu.Unlock()
			<-done
		}
	}
}

type poolResultEpochKey struct{}

func (m *Manager) markExecutionResult(ctx context.Context, result Result, auth *Auth) {
	if m.codexPools.Load() != nil && auth != nil {
		ctx = context.WithValue(nonNilContext(ctx), poolResultEpochKey{}, auth.RegistrationEpoch)
	}
	m.MarkResult(ctx, result)
}

func (m *Manager) recordNeutralExecutionResult(ctx context.Context, result Result, auth *Auth) {
	if m.codexPools.Load() != nil && auth != nil {
		ctx = context.WithValue(nonNilContext(ctx), poolResultEpochKey{}, auth.RegistrationEpoch)
	}
	m.recordAvailabilityNeutralResult(ctx, result)
}
