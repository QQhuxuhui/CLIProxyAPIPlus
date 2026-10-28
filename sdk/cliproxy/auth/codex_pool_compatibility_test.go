package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"

	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestCodexPoolCategoryMoveRechecksCurrentPresence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newPoolStorageManager(t, 1)
		a, err := m.Register(context.Background(), &Auth{ID: "presence-move", Provider: "codex"})
		if err != nil {
			t.Fatal(err)
		}
		gate := m.codexPools.Load().owners.gate(a.ID)
		<-gate
		defer func() {
			select {
			case gate <- struct{}{}:
			default:
			}
		}()
		ctx := context.Background()
		moved := a.Clone()
		moved.Attributes = map[string]string{AttributeAuthKind: AuthKindAPIKey, AttributeSource: AuthSourceConfig, AttributeAPIKey: "fixture-key"}
		done := make(chan error, 1)
		go func() { _, err := m.Update(ctx, moved); done <- err }()
		synctest.Wait()
		a.Disabled = true
		if _, err := m.Update(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		gate <- struct{}{}
		awaitPoolOperation(t, done)
		if !m.HasProviderAuth("codex") {
			t.Fatal("enabled migrated credential missing from provider presence")
		}
	})
}

func TestCodexPoolCapacityPreservesSelectionErrors(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	registerPoolModel(t, m, "capacity-errors")
	g, _ := m.lockAuth(context.Background(), "capacity-errors")
	r := g.pool.rate
	r.config.Enabled, r.config.FailFast = true, true
	_, limit, _ := r.accountLocked("capacity-errors", r.nowFunc())
	for range limit {
		r.startLocked("capacity-errors", r.nowFunc())
	}
	g.unlock()
	for _, original := range []error{context.Canceled, errors.New("plugin selection failed"), &Error{Code: "invalid_request", HTTPStatus: 400}} {
		if got := m.pooledCapacityAfterPickFailure(context.Background(), []string{"codex"}, "pool-model", exec.Options{}, original); got != original {
			t.Errorf("selection error %v replaced by %v", original, got)
		}
	}
}

func TestInjectCredentialsConcurrentExecutorReload(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "legacy"
		if enabled {
			name = "pooled"
		}
		t.Run(name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			if enabled {
				m.initializeCodexPools(1)
			}
			if _, err := m.Register(context.Background(), &Auth{ID: "inject-reload", Provider: "codex"}); err != nil {
				t.Fatal(err)
			}
			m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				for range 256 {
					m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				for range 256 {
					req, err := http.NewRequest(http.MethodGet, "http://fixture.test", nil)
					if err != nil {
						t.Error(err)
						return
					}
					if err := m.InjectCredentials(req, "inject-reload"); err != nil {
						t.Error(err)
						return
					}
				}
			}()
			close(start)
			wg.Wait()
		})
	}
}

func TestCodexPoolBindingRechecksConcurrentCooldown(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	registerPoolModel(t, m, "binding-cooldown-a")
	registerPoolModel(t, m, "binding-cooldown-b")
	selected, _ := m.GetByID("binding-cooldown-a")
	opts := exec.Options{Headers: http.Header{"Session-Id": {"binding-cooldown"}}}
	opts.EnsureMetadata()
	key, affinity := m.poolSessionKey("pool-model", opts)
	m.MarkResult(context.Background(), Result{AuthID: selected.ID, Provider: "codex", Model: "pool-model", Error: &Error{HTTPStatus: 503, Message: "fixture overload"}})
	_, _, err := m.bindPoolSelection(context.Background(), key, "pool-model", opts, affinity, selected, nil)
	ctx, tried, retry := staleSelectionRetry(context.Background(), err, nil, selected.ID)
	if !retry {
		t.Fatalf("binding race must trigger reselection, got %v", err)
	}
	a, _, err := m.pickCodexPool(ctx, "pool-model", opts, tried)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == selected.ID {
		t.Fatal("cooling credential selected again")
	}
}

func TestCodexPoolCompatibilityNonFailFastParity(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	registerPoolModel(t, m, "audit-rate")
	g, _ := m.lockAuth(context.Background(), "audit-rate")
	r := g.pool.rate
	r.config.Enabled = true
	r.config.FailFast = false
	_, limit, _ := r.accountLocked("audit-rate", r.nowFunc())
	for range limit {
		r.startLocked("audit-rate", r.nowFunc())
	}
	g.unlock()
	if _, err := m.SelectAuth(context.Background(), "codex", "pool-model", exec.Options{}); err != nil {
		t.Fatalf("non-fail-fast selection should remain dispatchable: %v", err)
	}
}
func TestCodexPoolCompatibilityBoundZeroWeight(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.SetSelector(NewSessionAffinitySelector(&WeightedRoundRobinSelector{}))
	registerPoolModel(t, m, "audit-weight-a")
	registerPoolModel(t, m, "audit-weight-b")
	opts := exec.Options{Headers: http.Header{"Session-Id": []string{"audit-session"}}}
	a, err := m.SelectAuth(context.Background(), "codex", "pool-model", opts)
	if err != nil {
		t.Fatal(err)
	}
	a.Attributes = map[string]string{AttributeWeight: "0"}
	if _, err = m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	b, err := m.SelectAuth(context.Background(), "codex", "pool-model", opts)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID == a.ID {
		t.Fatal("bound zero-weight credential was selected")
	}
}
func TestCodexPoolCompatibilityPresenceAfterBan(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.Register(context.Background(), &Auth{ID: "audit-ban", Provider: "antigravity"})
	m.MarkResult(context.Background(), Result{AuthID: "audit-ban", Provider: "antigravity", Error: &Error{HTTPStatus: 403, Message: "violation of terms of service"}})
	if m.HasProviderAuth("antigravity") {
		t.Fatal("banned sole provider remains advertised")
	}
}
