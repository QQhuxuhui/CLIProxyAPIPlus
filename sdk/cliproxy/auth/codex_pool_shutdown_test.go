package auth

import (
	"context"
	"errors"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type poolRetryCooldownStore struct {
	recordingCooldownStateStore
	fail bool
}

func (s *poolRetryCooldownStore) Save(ctx context.Context, records []CooldownStateRecord) error {
	if s.fail {
		s.fail = false
		return errors.New("fixture save failure")
	}
	return s.recordingCooldownStateStore.Save(ctx, records)
}
func TestCodexPoolCooldownFailureRemainsPending(t *testing.T) {
	m := newPoolStorageManager(t, 2)
	m.SetConfig(&internalconfig.Config{SaveCooldownStatus: true})
	store := &poolRetryCooldownStore{fail: true}
	m.SetCooldownStateStore(store)
	_, err := m.Register(context.Background(), &Auth{ID: "save-retry", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	m.MarkResult(context.Background(), Result{AuthID: "save-retry", Provider: "codex", Model: "model", Error: &Error{HTTPStatus: 429, Message: "quota"}})
	p := m.codexPools.Load()
	if err := p.saveCooldowns(context.Background(), false); err == nil {
		t.Fatal("save failure not returned")
	}
	if !p.cooldownDirty.Load() {
		t.Fatal("failed save lost pending state")
	}
	if err := p.saveCooldowns(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(store.savedRecords()) == 0 {
		t.Fatal("retry did not persist cooldown")
	}
}
func TestCodexPoolFlushPersistsFinalState(t *testing.T) {
	m := newPoolStorageManager(t, 2)
	m.SetConfig(&internalconfig.Config{SaveCooldownStatus: true})
	store := &recordingCooldownStateStore{}
	m.SetCooldownStateStore(store)
	registerPoolModel(t, m, "final-flush")
	m.MarkResult(context.Background(), Result{AuthID: "final-flush", Provider: "codex", Model: "pool-model", Error: &Error{HTTPStatus: 429, Message: "quota"}})
	if err := m.StopSuccessResults(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.FlushCodexPools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.savedRecords()) == 0 {
		t.Fatal("final cooldown state was not saved")
	}
}
