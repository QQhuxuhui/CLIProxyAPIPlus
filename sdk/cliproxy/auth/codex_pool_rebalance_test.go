package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestCodexPoolRebalanceSplitsAndMerges(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	pools := m.codexPools.Load()
	cfg := internalconfig.CodexPoolsConfig{MaxAccounts: 8, MinAccounts: 2, MigrationSlotsPerTick: 64}.WithDefaults()
	pools.config.Store(&cfg)
	for i := range 24 {
		_, err := m.Register(context.Background(), &Auth{ID: fmt.Sprintf("rebalance-%d", i), Provider: "codex"})
		if err != nil {
			t.Fatal(err)
		}
	}
	for range 80 {
		if err := pools.rebalance(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(pools.table.Load().pools) < 3 {
		t.Fatal("oversized pool was not split")
	}
	for _, p := range pools.table.Load().pools {
		p.mu.Lock()
		count := len(p.auths)
		p.mu.Unlock()
		if count > 8 {
			t.Fatalf("pool still oversized: %d", count)
		}
	}
	for _, a := range m.List() {
		m.Remove(context.Background(), a.ID)
	}
	for range 80 {
		if err := pools.rebalance(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(pools.table.Load().pools) != 1 {
		t.Fatalf("empty pools not merged: %d", len(pools.table.Load().pools))
	}
}
func TestCodexPoolProjectionPublishesCurrentState(t *testing.T) {
	m := newPoolStorageManager(t, 2)
	registerPoolModel(t, m, "projected-auth")
	m.MarkResult(context.Background(), Result{AuthID: "projected-auth", Provider: "codex", Model: "pool-model", Error: &Error{HTTPStatus: 503, Message: "overload"}})
	if err := m.codexPools.Load().publishProjections(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, _ := m.GetByID("projected-auth")
	if a.Failed != 1 {
		t.Fatal("result not applied")
	}
	if _, _, err := m.ResetQuota(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.codexPools.Load().publishProjections(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCodexPoolProjectionRecoversWithoutNewResult(t *testing.T) {
	m := newPoolStorageManager(t, 2)
	registerPoolModel(t, m, "deadline-project")
	m.MarkResult(context.Background(), Result{AuthID: "deadline-project", Provider: "codex", Model: "pool-model", Error: &Error{HTTPStatus: 503, Message: "overload"}})
	pools := m.codexPools.Load()
	if err := pools.publishProjections(context.Background()); err != nil {
		t.Fatal(err)
	}
	guard, err := m.lockAuth(context.Background(), "deadline-project")
	if err != nil {
		t.Fatal(err)
	}
	state := guard.auths["deadline-project"].ModelStates["pool-model"]
	state.NextRetryAfter = time.Now().Add(-time.Second)
	guard.pool.projectionDue["deadline-project"] = state.NextRetryAfter
	guard.unlock()
	if err := pools.publishProjections(context.Background()); err != nil {
		t.Fatal(err)
	}
	if registry.GetGlobalRegistry().GetModelCount("pool-model") != 1 {
		t.Fatal("expired model stayed suspended")
	}
}
