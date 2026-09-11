package executor

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestAntigravitySessionLockSerializesSameKey(t *testing.T) {
	manager := newAntigravitySessionLockManager(8)
	releaseFirst, err := manager.Acquire(context.Background(), "same")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()

	acquired := make(chan struct{})
	releaseSecond := make(chan func(), 1)
	go func() {
		release, errAcquire := manager.Acquire(context.Background(), "same")
		if errAcquire == nil {
			releaseSecond <- release
			close(acquired)
		}
	}()
	deadline := time.After(time.Second)
	for manager.refCount("same") != 2 {
		select {
		case <-deadline:
			t.Fatal("same-key waiter did not register")
		default:
		}
	}
	select {
	case <-acquired:
		t.Fatal("same-key waiter acquired before first release")
	default:
	}
	releaseFirst()
	select {
	case release := <-releaseSecond:
		release()
	case <-time.After(time.Second):
		t.Fatal("same-key waiter did not acquire after release")
	}
}

func TestAntigravitySessionLockAllowsDifferentKeys(t *testing.T) {
	manager := newAntigravitySessionLockManager(8)
	releaseA, errA := manager.Acquire(context.Background(), "a")
	if errA != nil {
		t.Fatal(errA)
	}
	defer releaseA()
	releaseB, errB := manager.Acquire(context.Background(), "b")
	if errB != nil {
		t.Fatal(errB)
	}
	releaseB()
}

func TestAntigravitySessionLockCancellationRemovesWaiter(t *testing.T) {
	manager := newAntigravitySessionLockManager(8)
	release, err := manager.Acquire(context.Background(), "cancel")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, errAcquire := manager.Acquire(ctx, "cancel"); errAcquire != context.Canceled {
		t.Fatalf("Acquire() error = %v, want context canceled", errAcquire)
	}
	release()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.entries) != 0 {
		t.Fatalf("idle lock entries = %d, want 0", len(manager.entries))
	}
}

func TestAntigravitySessionLockWaitsForCapacity(t *testing.T) {
	manager := newAntigravitySessionLockManager(1)
	releaseFirst, err := manager.Acquire(context.Background(), "first")
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	releaseSecond := make(chan func(), 1)
	go func() {
		release, errAcquire := manager.Acquire(context.Background(), "second")
		if errAcquire == nil {
			releaseSecond <- release
		}
		result <- errAcquire
	}()
	runtime.Gosched()
	select {
	case errAcquire := <-result:
		t.Fatalf("capacity waiter returned before release: %v", errAcquire)
	default:
	}
	releaseFirst()
	select {
	case errAcquire := <-result:
		if errAcquire != nil {
			t.Fatal(errAcquire)
		}
		(<-releaseSecond)()
	case <-time.After(time.Second):
		t.Fatal("capacity waiter did not acquire after release")
	}
}
