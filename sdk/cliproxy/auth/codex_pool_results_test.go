package auth

import (
	"context"
	"testing"
	"time"
)

func TestCodexPoolResultsApplyAfterMigration(t *testing.T) {
	m := newPoolStorageManager(t, 4)
	m.SetAsyncSuccessResults(true)
	t.Cleanup(func() { _ = m.StopSuccessResults(context.Background()) })
	a, err := m.Register(context.Background(), &Auth{ID: "queued-move", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	pools := m.codexPools.Load()
	source := pools.table.Load().owners[codexSlot(a.ID)]
	var target *codexAccountPool
	for _, p := range pools.table.Load().pools {
		if p != source {
			target = p
			break
		}
	}
	item := queuedResult{ctx: context.Background(), result: Result{AuthID: a.ID, Provider: "codex", Success: true, Model: "model"}, registrationEpoch: a.RegistrationEpoch, completedAt: time.Now()}
	if err := pools.moveSlot(context.Background(), codexSlot(a.ID), target); err != nil {
		t.Fatal(err)
	}
	m.applyPoolResultBatch(source, []queuedResult{item})
	m.flushSuccessResults()
	got, _ := m.GetByID(a.ID)
	if got.Success != 1 {
		t.Fatalf("successes=%d want1", got.Success)
	}
}
func TestCodexPoolResultsOldEpochIsDiscarded(t *testing.T) {
	m := newPoolStorageManager(t, 2)
	a, _ := m.Register(context.Background(), &Auth{ID: "epoch-result", Provider: "codex"})
	newer, _ := m.Register(context.Background(), &Auth{ID: a.ID, Provider: "codex"})
	m.applyPoolResultBatch(nil, []queuedResult{{ctx: context.Background(), result: Result{AuthID: a.ID, Provider: "codex", Success: true}, registrationEpoch: a.RegistrationEpoch, completedAt: time.Now()}})
	got, _ := m.GetByID(a.ID)
	if got.Success != 0 || got.RegistrationEpoch != newer.RegistrationEpoch {
		t.Fatal("old result affected replacement")
	}
}

func TestCodexPoolNeutralResultDropsOldRegistration(t *testing.T) {
	m := newPoolStorageManager(t, 2)
	old, _ := m.Register(context.Background(), &Auth{ID: "neutral-epoch", Provider: "codex"})
	m.Remove(context.Background(), old.ID)
	fresh, _ := m.Register(context.Background(), &Auth{ID: old.ID, Provider: "codex"})
	ctx := context.WithValue(context.Background(), poolResultEpochKey{}, old.RegistrationEpoch)
	m.recordAvailabilityNeutralResult(ctx, Result{AuthID: old.ID, Provider: "codex", Error: &Error{HTTPStatus: 400}})
	got, _ := m.GetByID(old.ID)
	if got.Failed != 0 || got.Generation != fresh.Generation {
		t.Fatal("old neutral result changed replacement")
	}
}
