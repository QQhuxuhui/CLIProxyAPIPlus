package auth

import (
	"context"
	"sync"
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
	queue   chan queuedResult
	once    sync.Once
}

// SetAsyncSuccessResults enables batched, asynchronous recording of successful execution
// results. Failures are always recorded synchronously.
func (m *Manager) SetAsyncSuccessResults(enabled bool) {
	if m == nil {
		return
	}
	m.successResults.mu.Lock()
	m.successResults.enabled = enabled
	m.successResults.mu.Unlock()
}

// enqueueSuccessResult queues a successful result and reports whether it was accepted.
func (m *Manager) enqueueSuccessResult(ctx context.Context, result Result) bool {
	if m == nil || result.AuthID == "" {
		return false
	}
	b := &m.successResults
	b.mu.Lock()
	enabled := b.enabled
	b.mu.Unlock()
	if !enabled {
		return false
	}
	b.once.Do(func() {
		queue := make(chan queuedResult, successResultQueueSize)
		b.mu.Lock()
		b.queue = queue
		b.mu.Unlock()
		go m.runSuccessResultApplier(queue)
	})
	b.mu.Lock()
	queue := b.queue
	b.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	// The request may finish before the result is applied; keep its values, drop its cancel.
	select {
	case queue <- queuedResult{ctx: context.WithoutCancel(ctx), result: result}:
		return true
	default:
		return false
	}
}

func (m *Manager) runSuccessResultApplier(queue <-chan queuedResult) {
	batch := make([]queuedResult, 0, successResultBatchMax)
	for first := range queue {
		batch = append(batch[:0], first)
	drain:
		for len(batch) < successResultBatchMax {
			select {
			case next := <-queue:
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
	b := &m.successResults
	b.mu.Lock()
	queue := b.queue
	b.mu.Unlock()
	if queue == nil {
		return
	}
	done := make(chan struct{})
	queue <- queuedResult{done: done}
	<-done
}

// applyResultBatch records results with one manager write lock for the whole batch.
func (m *Manager) applyResultBatch(batch []queuedResult) {
	type prepared struct {
		ctx    context.Context
		result Result
		update markResultUpdate
	}
	ready := make([]prepared, 0, len(batch))
	for _, item := range batch {
		if item.done != nil {
			continue
		}
		ctx, result, ok := m.prepareMarkResult(item.ctx, item.result, false)
		if ok {
			ready = append(ready, prepared{ctx: ctx, result: result})
		}
	}
	if len(ready) == 0 {
		return
	}
	m.mu.Lock()
	for i := range ready {
		ready[i].update = m.markResultLocked(ready[i].ctx, ready[i].result, canonicalModelKey(ready[i].result.Model), false)
	}
	m.mu.Unlock()
	for _, item := range ready {
		m.finishMarkResult(item.ctx, item.result, false, item.update)
	}
}
