package auth

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newResultBatchTestManager(t *testing.T, ids ...string) *Manager {
	t.Helper()
	m := NewManager(nil, nil, nil)
	for _, id := range ids {
		m.auths[id] = &Auth{ID: id, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "t"}}
	}
	return m
}

func TestRecordExecutionResultBatchesSuccessesWhenEnabled(t *testing.T) {
	m := newResultBatchTestManager(t, "a", "b")
	m.SetAsyncSuccessResults(true)
	ctx, cancel := context.WithCancel(context.Background())
	for i := 0; i < 50; i++ {
		id := []string{"a", "b"}[i%2]
		m.recordExecutionResult(ctx, Result{AuthID: id, Provider: "codex", Model: "gpt-6-luna", Success: true}, nil, false)
	}
	// The request context may end before the batch is applied.
	cancel()
	m.flushSuccessResults()
	if a, b := m.auths["a"].Success, m.auths["b"].Success; a != 25 || b != 25 {
		t.Fatalf("success counters = %d/%d, want 25/25", a, b)
	}
}

func TestRecordExecutionResultFailureStaysSynchronous(t *testing.T) {
	m := newResultBatchTestManager(t, "a")
	m.SetAsyncSuccessResults(true)
	m.recordExecutionResult(context.Background(), Result{AuthID: "a", Provider: "codex", Model: "gpt-6-luna", Error: &Error{HTTPStatus: 500, Message: "boom"}}, nil, false)
	// No flush: the failure must already be visible to the next pick.
	if got := m.auths["a"].Failed; got != 1 {
		t.Fatalf("failed counter = %d, want 1 immediately", got)
	}
}

func TestRecordExecutionResultSynchronousWhenDisabled(t *testing.T) {
	m := newResultBatchTestManager(t, "a")
	m.recordExecutionResult(context.Background(), Result{AuthID: "a", Provider: "codex", Model: "gpt-6-luna", Success: true}, nil, false)
	if got := m.auths["a"].Success; got != 1 {
		t.Fatalf("success counter = %d, want 1 without batching", got)
	}
	if m.successResults.queue != nil {
		t.Fatal("applier started while batching is disabled")
	}
}

func TestEnqueueSuccessResultFallsBackWhenQueueFull(t *testing.T) {
	m := newResultBatchTestManager(t, "a")
	m.SetAsyncSuccessResults(true)
	// Hold the manager lock so the applier cannot drain while the queue fills up.
	m.mu.Lock()
	accepted := 0
	for i := 0; i < successResultQueueSize+successResultBatchMax+10; i++ {
		if m.enqueueSuccessResult(context.Background(), Result{AuthID: "a", Provider: "codex", Success: true}, nil) {
			accepted++
		}
	}
	m.mu.Unlock()
	if accepted >= successResultQueueSize+successResultBatchMax+10 {
		t.Fatalf("queue never reported full (accepted %d)", accepted)
	}
	m.flushSuccessResults()
	if got := m.auths["a"].Success; got != int64(accepted) {
		t.Fatalf("success counter = %d, want %d accepted results", got, accepted)
	}
}

func TestApplyResultBatchMatchesSequentialMarkResult(t *testing.T) {
	results := []Result{
		{AuthID: "a", Provider: "codex", Model: "m1", Success: true},
		{AuthID: "b", Provider: "codex", Model: "m1", Success: true},
		{AuthID: "missing", Provider: "codex", Model: "m1", Success: true},
		{AuthID: "", Provider: "codex", Success: true},
		{AuthID: "a", Provider: "codex", Model: "m2", Success: true},
	}
	batched := newResultBatchTestManager(t, "a", "b")
	sequential := newResultBatchTestManager(t, "a", "b")
	items := make([]queuedResult, 0, len(results))
	for _, r := range results {
		items = append(items, queuedResult{ctx: context.Background(), result: r})
		sequential.MarkResult(context.Background(), r)
	}
	batched.applyResultBatch(items)
	for _, id := range []string{"a", "b"} {
		got, want := batched.auths[id], sequential.auths[id]
		if got.Success != want.Success || got.Generation != want.Generation || len(got.ModelStates) != len(want.ModelStates) {
			t.Fatalf("auth %s: batched success=%d gen=%d models=%d, sequential success=%d gen=%d models=%d",
				id, got.Success, got.Generation, len(got.ModelStates), want.Success, want.Generation, len(want.ModelStates))
		}
	}
}

func TestSuccessResultBatcherConcurrentProducers(t *testing.T) {
	m := newResultBatchTestManager(t)
	for i := range 8 {
		id := fmt.Sprint(i)
		m.auths[id] = &Auth{ID: id, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "t"}}
	}
	m.SetAsyncSuccessResults(true)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for range 200 {
				m.recordExecutionResult(context.Background(), Result{AuthID: fmt.Sprint(w), Provider: "codex", Model: "m", Success: true}, nil, false)
				if w%2 == 0 {
					m.recordExecutionResult(context.Background(), Result{AuthID: fmt.Sprint(w), Provider: "codex", Model: "m", Error: &Error{HTTPStatus: 500, Message: "x"}}, nil, false)
				}
			}
		})
	}
	wg.Wait()
	m.flushSuccessResults()
	for i := range 8 {
		if got := m.auths[fmt.Sprint(i)].Success; got != 200 {
			t.Fatalf("auth %d success = %d, want 200", i, got)
		}
	}
}

func TestQueuedSuccessDoesNotClearNewerFailureCooldown(t *testing.T) {
	m := newResultBatchTestManager(t, "a")
	ctx := context.Background()
	success := queuedResult{
		ctx:               ctx,
		result:            Result{AuthID: "a", Provider: "codex", Model: "gpt-6-luna", Success: true},
		completedAt:       time.Now().Add(-time.Second),
		registrationEpoch: m.auths["a"].RegistrationEpoch,
	}
	// A newer request fails with 429 while the earlier success is still queued.
	m.MarkResult(ctx, Result{AuthID: "a", Provider: "codex", Model: "gpt-6-luna", Error: &Error{HTTPStatus: 429, Message: "rate limited"}})
	if blocked, _, _ := isAuthBlockedForModel(m.auths["a"], "gpt-6-luna", time.Now()); !blocked {
		t.Fatal("429 did not put the model into cooldown")
	}
	m.applyResultBatch([]queuedResult{success})
	if blocked, _, _ := isAuthBlockedForModel(m.auths["a"], "gpt-6-luna", time.Now()); !blocked {
		t.Fatal("an older queued success cleared the newer 429 cooldown")
	}
	if got := m.auths["a"].Success; got != 1 {
		t.Fatalf("stale success not counted: success=%d", got)
	}
	// A success that completed after the failure still clears it.
	m.applyResultBatch([]queuedResult{{ctx: ctx, result: success.result, completedAt: time.Now(), registrationEpoch: success.registrationEpoch}})
	if blocked, _, _ := isAuthBlockedForModel(m.auths["a"], "gpt-6-luna", time.Now()); blocked {
		t.Fatal("a newer success did not clear the cooldown")
	}
}

func TestQueuedSuccessDroppedForReregisteredAuth(t *testing.T) {
	m := newResultBatchTestManager(t)
	ctx := context.Background()
	if _, err := m.Register(ctx, &Auth{ID: "a", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "t"}}); err != nil {
		t.Fatal(err)
	}
	executed := m.auths["a"].Clone()
	m.mu.Lock()
	delete(m.auths, "a")
	m.mu.Unlock()
	if _, err := m.Register(ctx, &Auth{ID: "a", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "t2"}}); err != nil {
		t.Fatal(err)
	}
	if m.auths["a"].RegistrationEpoch == executed.RegistrationEpoch {
		t.Fatal("test setup: re-registration kept the same epoch")
	}
	m.applyResultBatch([]queuedResult{{
		ctx:               ctx,
		result:            Result{AuthID: "a", Provider: "codex", Success: true},
		completedAt:       time.Now(),
		registrationEpoch: executed.RegistrationEpoch,
	}})
	if got := m.auths["a"].Success; got != 0 {
		t.Fatalf("result of the old registration landed on the new one: success=%d", got)
	}
}

func TestStopSuccessResultsDrainsQueueAndStopsApplier(t *testing.T) {
	m := newResultBatchTestManager(t, "a")
	m.SetAsyncSuccessResults(true)
	var hookCalls atomic.Int32
	m.hook = resultCountingHook{calls: &hookCalls}
	m.mu.Lock()
	for range 20 {
		m.enqueueSuccessResult(context.Background(), Result{AuthID: "a", Provider: "codex", Success: true}, nil)
	}
	m.mu.Unlock()
	if err := m.StopSuccessResults(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.auths["a"].Success; got != 20 {
		t.Fatalf("queued successes not drained before stop: success=%d", got)
	}
	if got := hookCalls.Load(); got != 20 {
		t.Fatalf("hook calls = %d, want 20 before stop returns", got)
	}
	select {
	case <-m.successResults.done:
	default:
		t.Fatal("applier goroutine still running after stop")
	}
	// After stop, successes are recorded synchronously and the call is idempotent.
	m.recordExecutionResult(context.Background(), Result{AuthID: "a", Provider: "codex", Success: true}, nil, false)
	if got := m.auths["a"].Success; got != 21 {
		t.Fatalf("success after stop = %d, want 21 (synchronous)", got)
	}
	if err := m.StopSuccessResults(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type resultCountingHook struct {
	NoopHook
	calls *atomic.Int32
}

func (h resultCountingHook) OnResult(context.Context, Result) { h.calls.Add(1) }
