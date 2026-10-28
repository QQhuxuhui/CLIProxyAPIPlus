package auth

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func awaitPoolOperation(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("operation waited on an unrelated lock")
	}
}
func newPoolStorageManager(t *testing.T, count int) *Manager {
	t.Helper()
	m := NewManager(nil, nil, nil)
	m.initializeCodexPools(count)
	return m
}
func TestCodexPoolStorageOutsideLegacyMap(t *testing.T) {
	m := newPoolStorageManager(t, 4)
	a, err := m.Register(context.Background(), &Auth{ID: "codex-pooled", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := m.auths[a.ID]; exists {
		t.Fatal("Codex still stored in legacy map")
	}
	got, ok := m.GetByID(a.ID)
	if !ok || got.RegistrationEpoch != a.RegistrationEpoch {
		t.Fatal("pooled lookup lost credential")
	}
	if len(m.List()) != 1 {
		t.Fatal("pooled credential missing from management list")
	}
}
func TestCodexPoolStorageGetIgnoresLegacyMutex(t *testing.T) {
	m := newPoolStorageManager(t, 4)
	_, err := m.Register(context.Background(), &Auth{ID: "codex-pooled", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, ok := m.GetByID("codex-pooled")
		if !ok {
			done <- fmt.Errorf("missing pooled auth")
			return
		}
		done <- nil
	}()
	awaitPoolOperation(t, done)
}
func TestCodexPoolStorageMovePreservesCredential(t *testing.T) {
	m := newPoolStorageManager(t, 4)
	a, err := m.Register(context.Background(), &Auth{ID: "codex-pooled", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	pools := m.codexPools.Load()
	slot := codexSlot(a.ID)
	before := pools.table.Load().owners[slot]
	var target *codexAccountPool
	for _, p := range pools.table.Load().pools {
		if p != before {
			target = p
			break
		}
	}
	if err := pools.moveSlot(context.Background(), slot, target); err != nil {
		t.Fatal(err)
	}
	got, ok := m.GetByID(a.ID)
	if !ok || got.RegistrationEpoch != a.RegistrationEpoch || got.Generation != a.Generation {
		t.Fatalf("migration changed credential: %+v", got)
	}
	before.mu.Lock()
	_, old := before.auths[a.ID]
	before.mu.Unlock()
	if old {
		t.Fatal("credential still in source")
	}
}

func TestCodexPoolStorageUpdateAndResultIgnoreGlobalLocks(t *testing.T) {
	m := newPoolStorageManager(t, 4)
	a, err := m.Register(context.Background(), &Auth{ID: "codex-pooled", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.scheduler.mu.Lock()
	defer m.mu.Unlock()
	defer m.scheduler.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		a.Label = "updated"
		_, err := m.Update(context.Background(), a)
		if err == nil {
			m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: "codex", Model: "gpt", Success: true})
		}
		done <- err
	}()
	awaitPoolOperation(t, done)
}

func TestCodexPoolStorageRegistrationEpochNeverRegresses(t *testing.T) {
	m := newPoolStorageManager(t, 2)
	first, err := m.Register(context.Background(), &Auth{ID: "epoch-highwater", Provider: "codex", RegistrationEpoch: 100})
	if err != nil {
		t.Fatal(err)
	}
	m.Remove(context.Background(), first.ID)
	second, err := m.Register(context.Background(), &Auth{ID: first.ID, Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if second.RegistrationEpoch <= first.RegistrationEpoch {
		t.Fatalf("registration epoch regressed: %d -> %d", first.RegistrationEpoch, second.RegistrationEpoch)
	}
}
