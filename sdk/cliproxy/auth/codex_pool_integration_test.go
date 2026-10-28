package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestCodexPoolsConfiguredExecuteDuringMigration(t *testing.T) {
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	m.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{Pools: internalconfig.CodexPoolsConfig{Enable: true, MaxAccounts: 30, MinAccounts: 8, MigrationSlotsPerTick: 1024}}})
	m.SetRetryConfig(2, 6, 0)
	m.SetAsyncSuccessResults(true)
	t.Cleanup(func() { _ = m.StopSuccessResults(context.Background()) })
	var calls atomic.Int64
	executor := &customStreamMockExecutor{identifier: "codex", mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, _ *Auth, _ exec.Request, _ exec.Options) (exec.Response, error) {
		exec.MarkUpstreamAttempt(ctx)
		if calls.Add(1)%11 == 0 {
			return exec.Response{}, customStatusError{code: 503, msg: "overloaded"}
		}
		return exec.Response{Payload: []byte("ok")}, nil
	}}, streamFn: func(ctx context.Context, _ *Auth, _ exec.Request, _ exec.Options) (*exec.StreamResult, error) {
		exec.MarkUpstreamAttempt(ctx)
		calls.Add(1)
		ch := make(chan exec.StreamChunk, 1)
		ch <- exec.StreamChunk{Payload: []byte("data: ok\n\n")}
		close(ch)
		return &exec.StreamResult{Chunks: ch}, nil
	}}
	m.RegisterExecutor(executor)
	for i := range 120 {
		id := fmt.Sprintf("pool-integration-%d", i)
		_, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex", Metadata: map[string]any{"access_token": "fixture", "refresh_token": "fixture"}})
		if err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "pool-integration-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		m.RefreshSchedulerEntry(id)
	}
	pools := m.codexPools.Load()
	for range 12 {
		if err := pools.rebalance(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(pools.table.Load().pools) < 4 {
		t.Fatal("configured pools did not split")
	}
	var wg sync.WaitGroup
	for worker := range 16 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range 24 {
				opts := exec.Options{Headers: http.Header{"Session-Id": {fmt.Sprintf("worker-%d", worker)}}}
				if i%2 == 0 {
					_, err := m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: "pool-integration-model"}, opts)
					if err != nil {
						t.Error(err)
					}
				} else {
					stream, err := m.ExecuteStream(context.Background(), []string{"codex"}, exec.Request{Model: "pool-integration-model"}, opts)
					if err != nil {
						t.Error(err)
						continue
					}
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							t.Error(chunk.Err)
						}
					}
				}
			}
		}(worker)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 60 {
			table := pools.table.Load()
			slot := uint16(i)
			source := table.owners[slot]
			for _, target := range table.pools {
				if target != source {
					if err := pools.moveSlot(context.Background(), slot, target); err != nil {
						t.Error(err)
					}
					break
				}
			}
			if i%10 == 0 {
				if err := pools.publishProjections(context.Background()); err != nil {
					t.Error(err)
				}
			}
		}
	}()
	wg.Wait()
	m.flushSuccessResults()
	var successes int64
	for _, a := range m.List() {
		successes += a.Success
	}
	if successes != 16*24 {
		t.Fatalf("successes=%d want384; calls=%d", successes, calls.Load())
	}
	if len(m.auths) != 0 {
		t.Fatal("pooled execution repopulated global auth map")
	}
}
func TestCodexPoolCategoryMoveCarriesRateWindow(t *testing.T) {
	m := newPoolStorageManager(t, 2)
	a, err := m.Register(context.Background(), &Auth{ID: "category-rate", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := m.beginRateAttempt(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	a.Attributes = map[string]string{AttributeAuthKind: AuthKindAPIKey, AttributeSource: AuthSourceConfig, AttributeAPIKey: "fixture"}
	next, err := m.Update(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := m.lockAuth(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if guard.pool != m.codexPools.Load().side {
		guard.unlock()
		t.Fatal("account did not enter side pool")
	}
	count, _, _ := guard.pool.rate.accountLocked(a.ID, time.Now())
	guard.unlock()
	if count != 1 {
		t.Fatalf("rate window reset during category move: %d", count)
	}
	if next.RegistrationEpoch == a.RegistrationEpoch {
		t.Fatal("category replacement retained old result epoch")
	}
	_ = ctx
}
