package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func seedCodexOwner(t *testing.T, index *codexOwnerIndex, id string, kind codexOwnerKind) codexOwnerLease {
	t.Helper()
	tx, err := index.begin(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !tx.commit(kind) {
		t.Fatal("initial commit failed")
	}
	lease, err := index.resolve(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}
func TestCodexOwnerIndexMovingInvalidatesStaleLease(t *testing.T) {
	index := newCodexOwnerIndex()
	old := seedCodexOwner(t, index, "a", codexOwnerPooled)
	tx, err := index.begin(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer tx.abort()
	if index.valid("a", old) {
		t.Fatal("moving account accepted an old lease")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = index.resolve(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("moving wait=%v", err)
	}
	if !tx.commit(codexOwnerSide) {
		t.Fatal("migration commit failed")
	}
	current, err := index.resolve(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if current.kind != codexOwnerSide || current.version != old.version+2 || index.valid("a", old) {
		t.Fatalf("old=%+v current=%+v", old, current)
	}
}
func TestCodexOwnerIndexMigrationDoesNotBlockOtherIDs(t *testing.T) {
	index := newCodexOwnerIndex()
	seedCodexOwner(t, index, "a", codexOwnerPooled)
	held, err := index.begin(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer held.abort()
	done := make(chan error, 1)
	go func() {
		tx, err := index.begin(context.Background(), "b")
		if err == nil {
			tx.commit(codexOwnerLegacy)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unrelated ID waited for migration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = index.begin(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("same-ID lifecycle wait=%v", err)
	}
}
func TestCodexOwnerIndexAbortWakesReaders(t *testing.T) {
	index := newCodexOwnerIndex()
	old := seedCodexOwner(t, index, "a", codexOwnerPooled)
	tx, err := index.begin(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer tx.abort()
	done := make(chan codexOwnerLease, 1)
	go func() { lease, _ := index.resolve(context.Background(), "a"); done <- lease }()
	tx.abort()
	select {
	case lease := <-done:
		if lease.kind != old.kind || lease.version <= old.version {
			t.Fatalf("abort lease=%+v", lease)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader was not awakened")
	}
	if tx.commit(codexOwnerSide) {
		t.Fatal("completed transaction was committed twice")
	}
}
func TestCodexOwnerIndexDeleteReregisterPreservesVersion(t *testing.T) {
	index := newCodexOwnerIndex()
	old := seedCodexOwner(t, index, "a", codexOwnerPooled)
	tx, err := index.begin(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	tx.commit(codexOwnerMissing)
	if _, err = index.resolve(context.Background(), "a"); !errors.Is(err, errCodexOwnerMissing) {
		t.Fatalf("deleted owner=%v", err)
	}
	fresh := seedCodexOwner(t, index, "a", codexOwnerSide)
	if fresh.version <= old.version || index.valid("a", old) {
		t.Fatal("registration reused a stale version")
	}
}
func TestCodexOwnerIndexConcurrentTransactions(t *testing.T) {
	index := newCodexOwnerIndex()
	seedCodexOwner(t, index, "a", codexOwnerPooled)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				tx, err := index.begin(context.Background(), "a")
				if err != nil {
					t.Error(err)
					return
				}
				tx.commit(codexOwnerPooled)
			}
		}()
	}
	wg.Wait()
	lease, err := index.resolve(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if lease.version != 2+8*50*2 {
		t.Fatalf("version=%d", lease.version)
	}
}

type codexOwnerWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *codexOwnerWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}
func TestCodexOwnerIndexCancellationDuringWait(t *testing.T) {
	for _, operation := range []string{"resolve", "lifecycle"} {
		t.Run(operation, func(t *testing.T) {
			index := newCodexOwnerIndex()
			seedCodexOwner(t, index, "a", codexOwnerPooled)
			held, err := index.begin(context.Background(), "a")
			if err != nil {
				t.Fatal(err)
			}
			defer held.abort()
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &codexOwnerWaitContext{Context: parent, entered: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				if operation == "resolve" {
					_, err := index.resolve(ctx, "a")
					done <- err
				} else {
					tx, err := index.begin(ctx, "a")
					if tx != nil {
						tx.abort()
					}
					done <- err
				}
			}()
			select {
			case <-ctx.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("operation did not enter its wait")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("wait error=%v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation did not interrupt wait")
			}
		})
	}
}

func TestCodexOwnerIndexPublishedRouteKeepsLifecycleGate(t *testing.T) {
	index := newCodexOwnerIndex()
	tx, err := index.begin(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer tx.abort()
	tx.publish(codexOwnerPooled)
	if _, err := index.resolve(context.Background(), "a"); err != nil {
		t.Fatal("stable owner not visible before housekeeping")
	}
	parent, cancel := context.WithCancel(context.Background())
	ctx := &codexOwnerWaitContext{Context: parent, entered: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		next, err := index.begin(ctx, "a")
		if next != nil {
			next.abort()
		}
		done <- err
	}()
	<-ctx.entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("lifecycle gate was released too early: %v", err)
	}
	tx.release()
	next, err := index.begin(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	next.abort()
}
