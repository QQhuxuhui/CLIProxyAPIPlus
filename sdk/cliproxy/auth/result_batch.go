package auth

import (
	"context"
	"sync"
	"time"
)

const (
	// successResultQueueSize bounds queued successes; a full queue falls back to a
	// synchronous MarkResult so results are never dropped.
	successResultQueueSize = 4096
	// successResultBatchMax bounds how many results share one m.mu critical section.
	successResultBatchMax = 256
)

type queuedResult struct {
	ctx    context.Context
	result Result
	// completedAt is when the request finished; a failure applied after it wins.
	completedAt time.Time
	// registrationEpoch identifies the credential that executed the request, so a result
	// never lands on a credential re-registered under the same ID. Zero skips the check.
	registrationEpoch uint64
	// done, when set, marks a flush request and is closed once its batch is applied.
	done chan struct{}
}

// successResultBatcher applies successful execution results in batches. Every finished
// request used to take the manager write lock on its own; under load those writers queue
// behind each other and the stream cannot end until its result is recorded. One applier
// goroutine instead drains whatever accumulated while the lock was busy and applies it in
// a single critical section.
type successResultBatcher struct {
	mu      sync.Mutex
	enabled bool
	stopped bool
	queue   chan queuedResult
	done    chan struct{}
}

// SetAsyncSuccessResults enables batched, asynchronous recording of successful execution
// results. Failures are always recorded synchronously.
func (m *Manager) SetAsyncSuccessResults(enabled bool) {
	if m != nil {
		m.poolAsyncResults.Store(enabled)
	}
	if m == nil {
		return
	}
	m.successResults.mu.Lock()
	m.successResults.enabled = enabled
	m.successResults.mu.Unlock()
}

// StopSuccessResults stops accepting queued results, applies everything already queued,
// and stops the applier. Later successes are recorded synchronously. Call it after the
// HTTP server stopped accepting requests.
func (m *Manager) StopSuccessResults(ctx context.Context) error {
	if m != nil && m.codexPools.Load() != nil {
		return m.stopPoolResults(ctx)
	}
	if m == nil {
		return nil
	}
	b := &m.successResults
	b.mu.Lock()
	b.enabled = false
	if b.stopped {
		b.mu.Unlock()
		return nil
	}
	b.stopped = true
	queue, done := b.queue, b.done
	if queue != nil {
		// Producers send while holding b.mu and check stopped first, so no send can race this close.
		close(queue)
	}
	b.mu.Unlock()
	if queue == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// enqueueSuccessResult queues a successful result and reports whether it was accepted.
func (m *Manager) enqueueSuccessResult(ctx context.Context, result Result, auth *Auth) bool {
	if m != nil && m.codexPools.Load() != nil {
		return m.enqueuePoolSuccess(ctx, result, auth)
	}
	if m == nil || result.AuthID == "" {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	item := queuedResult{
		// The request may finish before the result is applied; keep its values, drop its cancel.
		ctx:         context.WithoutCancel(ctx),
		result:      result,
		completedAt: time.Now(),
	}
	if auth != nil && auth.ID == result.AuthID {
		item.registrationEpoch = auth.RegistrationEpoch
	}
	b := &m.successResults
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.enabled || b.stopped {
		return false
	}
	if b.queue == nil {
		b.queue = make(chan queuedResult, successResultQueueSize)
		b.done = make(chan struct{})
		go m.runSuccessResultApplier(b.queue, b.done)
	}
	select {
	case b.queue <- item:
		return true
	default:
		return false
	}
}

func (m *Manager) runSuccessResultApplier(queue <-chan queuedResult, done chan<- struct{}) {
	defer close(done)
	batch := make([]queuedResult, 0, successResultBatchMax)
	for first := range queue {
		batch = append(batch[:0], first)
	drain:
		for len(batch) < successResultBatchMax {
			select {
			case next, ok := <-queue:
				if !ok {
					break drain
				}
				batch = append(batch, next)
			default:
				break drain
			}
		}
		m.applyResultBatch(batch)
		for _, item := range batch {
			if item.done != nil {
				close(item.done)
			}
		}
	}
}

// flushSuccessResults blocks until every success queued before the call is applied.
func (m *Manager) flushSuccessResults() {
	if m.codexPools.Load() != nil {
		m.flushPoolResults()
		return
	}
	b := &m.successResults
	b.mu.Lock()
	if b.queue == nil || b.stopped {
		b.mu.Unlock()
		return
	}
	done := make(chan struct{})
	b.queue <- queuedResult{done: done}
	b.mu.Unlock()
	<-done
}

// applyResultBatch records results with one manager write lock for the whole batch.
func (m *Manager) applyResultBatch(batch []queuedResult) {
	if m.codexPools.Load() != nil {
		m.applyPoolResultBatch(nil, batch)
		return
	}
	m.applyLegacyResultBatch(batch)
}

// applyLegacyResultBatch retains one manager write lock for non-pooled accounts.
func (m *Manager) applyLegacyResultBatch(batch []queuedResult) {
	type prepared struct {
		item   queuedResult
		ctx    context.Context
		result Result
		update markResultUpdate
		apply  bool
	}
	ready := make([]prepared, 0, len(batch))
	for _, item := range batch {
		if item.done != nil {
			continue
		}
		ctx, result, ok := m.prepareMarkResult(item.ctx, item.result, false)
		if ok {
			ready = append(ready, prepared{item: item, ctx: ctx, result: result})
		}
	}
	if len(ready) == 0 {
		return
	}
	m.mu.Lock()
	for i := range ready {
		p := &ready[i]
		current := m.auths[p.result.AuthID]
		if current == nil || (p.item.registrationEpoch != 0 && current.RegistrationEpoch != p.item.registrationEpoch) {
			continue
		}
		p.apply = true
		if current.lastFailureAt.After(p.item.completedAt) {
			p.update = m.markStaleSuccessLocked(current, p.result)
			continue
		}
		p.update = m.markResultLocked(p.ctx, p.result, canonicalModelKey(p.result.Model), false)
	}
	m.mu.Unlock()
	for _, p := range ready {
		if p.apply {
			m.finishMarkResult(p.ctx, p.result, false, p.update)
		}
	}
}

// markStaleSuccessLocked counts a success that completed before the credential's latest
// failure without clearing that failure's status or cooldown. The caller holds m.mu.
func (m *Manager) markStaleSuccessLocked(auth *Auth, result Result) markResultUpdate {
	now := time.Now()
	auth.recordRecentRequest(now, true)
	auth.Success++
	auth.Generation++
	auth.UpdatedAt = now
	return markResultUpdate{authSnapshot: auth.Clone(), modelKey: canonicalModelKey(result.Model), now: now}
}
