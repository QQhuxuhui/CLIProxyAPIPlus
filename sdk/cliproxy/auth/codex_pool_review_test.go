package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestPoolReviewReconcilePersistsOnlyChanges(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			store := &countingStore{}
			m := NewManager(store, nil, nil)
			if enabled {
				m.initializeCodexPools(1)
			}
			id := fmt.Sprintf("reconcile-changes-%v", enabled)
			_, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex", Metadata: map[string]any{"type": "codex"}})
			if err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "review-model"}})
			defer registry.GetGlobalRegistry().UnregisterClient(id)
			before := store.saveCount.Load()
			m.ReconcileRegistryModelStates(context.Background(), id)
			m.ReconcileRegistryModelStates(context.Background(), id)
			if n := store.saveCount.Load(); n != before {
				t.Fatalf("unchanged reconcile saved %d times", n-before)
			}
		})
	}
}
func TestPoolReviewQuotaHintPreserves429(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.SetConfig(&internalconfig.Config{ProviderRequestRateCap: map[string]internalconfig.RequestRateCapConfig{"codex": {Enabled: true, FailFast: true}}})
	registerPoolModel(t, m, "quota-hint")
	g, _ := m.lockAuth(context.Background(), "quota-hint")
	a := g.auths["quota-hint"]
	a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: time.Now().Add(time.Minute)}
	a.Unavailable = true
	a.NextRetryAfter = a.Quota.NextRecoverAt
	g.unlock()
	for range 3 {
		_, err := m.SelectAuth(context.Background(), "codex", "pool-model", exec.Options{})
		if statusCodeFromError(err) != 429 {
			t.Fatalf("quota error=%v status=%d", err, statusCodeFromError(err))
		}
	}
}

func TestPoolReviewCanceledMutationParity(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			if enabled {
				m.initializeCodexPools(1)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			a, err := m.Register(ctx, &Auth{ID: "canceled", Provider: "codex"})
			if err != nil || a == nil {
				t.Fatalf("register: %v", err)
			}
			m.MarkResult(ctx, Result{AuthID: a.ID, Provider: "codex", Success: true})
			a, _ = m.GetByID(a.ID)
			if a.Success != 1 {
				t.Fatal("completion dropped with canceled context")
			}
			a.Label = "updated"
			if _, err = m.Update(ctx, a); err != nil {
				t.Fatal(err)
			}
			m.Remove(ctx, a.ID)
			if _, ok := m.GetByID(a.ID); ok {
				t.Fatal("canceled remove left account alive")
			}
		})
	}
}
func TestPoolReviewPinnedCooldown(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	registerPoolModel(t, m, "pinned-quota")
	m.MarkResult(context.Background(), Result{AuthID: "pinned-quota", Provider: "codex", Model: "pool-model", Error: &Error{HTTPStatus: 429, Message: "quota"}})
	_, err := m.SelectAuth(context.Background(), "codex", "pool-model", exec.Options{Metadata: map[string]any{"pinned_auth_id": "pinned-quota"}})
	if statusCodeFromError(err) != 429 {
		t.Fatalf("pinned cooldown = %v", err)
	}
}
func TestPoolReviewReusedBindingSurvivesStaleOrderEntry(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	p := m.codexPools.Load().table.Load().pools[0]
	key := codexBindingKey{"reused", "model"}
	p.putBindingLocked(key, codexPoolBinding{authID: "a", expires: time.Now().Add(-time.Hour)})
	if err := m.codexPools.Load().publishProjections(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := range 4095 {
		p.putBindingLocked(codexBindingKey{fmt.Sprint(i), "model"}, codexPoolBinding{authID: "a", expires: time.Now().Add(time.Hour)})
	}
	p.putBindingLocked(key, codexPoolBinding{authID: "new", expires: time.Now().Add(time.Hour)})
	p.putBindingLocked(codexBindingKey{"extra", "model"}, codexPoolBinding{authID: "a", expires: time.Now().Add(time.Hour)})
	if p.bindings[key].authID != "new" {
		t.Fatal("fresh binding evicted by stale order entry")
	}
}

func TestPoolReviewBatchLifecycleAndInitialTopology(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-start; m.codexPools.Load().seedEmptyTopology(16) }()
	go func() {
		defer wg.Done()
		<-start
		for i := range 1000 {
			_, err := m.Register(context.Background(), &Auth{ID: fmt.Sprintf("bulk-%d", i), Provider: "codex"})
			if err != nil {
				t.Error(err)
			}
		}
	}()
	close(start)
	wg.Wait()
	for i := range 1000 {
		id := fmt.Sprintf("bulk-%d", i)
		if _, ok := m.GetByID(id); !ok {
			t.Fatalf("registered auth lost: %s", id)
		}
		m.Remove(context.Background(), id)
	}
	if len(m.List()) != 0 {
		t.Fatal("bulk delete left live accounts")
	}
}
func TestPoolReviewConcurrentFirstBinding(t *testing.T) {
	m := newPoolStorageManager(t, 4)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	for i := range 12 {
		registerPoolModel(t, m, fmt.Sprint("binding-", i))
	}
	start := make(chan struct{})
	results := make(chan string, 32)
	for range 32 {
		go func() {
			<-start
			a, err := m.SelectAuth(context.Background(), "codex", "pool-model", exec.Options{Headers: http.Header{"Session-Id": {"same-first-session"}}})
			if err != nil {
				t.Error(err)
				results <- ""
				return
			}
			results <- a.ID
		}()
	}
	close(start)
	first := <-results
	for range 31 {
		if got := <-results; got != first {
			t.Fatalf("concurrent first binding split: %s / %s", first, got)
		}
	}
}
func TestPoolReviewOldSuccessKeepsNewFailure(t *testing.T) {
	for _, provider := range []string{"codex", "antigravity"} {
		m := newPoolStorageManager(t, 1)
		a, _ := m.Register(context.Background(), &Auth{ID: provider, Provider: provider})
		m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: provider, Model: "model", Error: &Error{HTTPStatus: 429, Message: "new failure"}})
		m.applyPoolResultBatch(nil, []queuedResult{{ctx: context.Background(), result: Result{AuthID: a.ID, Provider: provider, Model: "model", Success: true}, registrationEpoch: a.RegistrationEpoch, completedAt: time.Now().Add(-time.Hour)}})
		got, _ := m.GetByID(a.ID)
		if got.Success != 1 || got.Failed != 1 || got.ModelStates["model"].LastError == nil {
			t.Fatalf("stale success reset failure: %+v", got)
		}
	}
}
func TestPoolReviewMovedRateCallbacks(t *testing.T) {
	m := newPoolStorageManager(t, 2)
	a, _ := m.Register(context.Background(), &Auth{ID: "moved-rate", Provider: "codex"})
	ctx, err := m.beginRateAttempt(exec.WithUpstreamAttemptTracker(context.Background()), a)
	if err != nil {
		t.Fatal(err)
	}
	ref := ctx.Value(rateAttemptContextKey{}).(*rateAttemptRef)
	first := ref.token.Load()
	pools := m.codexPools.Load()
	source := pools.table.Load().owners[codexSlot(a.ID)]
	target := pools.table.Load().pools[0]
	if target == source {
		target = pools.table.Load().pools[1]
	}
	if err = pools.moveSlot(context.Background(), codexSlot(a.ID), target); err != nil {
		t.Fatal(err)
	}
	exec.ObserveUpstreamRequestRetry(ctx)
	if ref.token.Load() == first {
		t.Fatal("migrated retry reused attempt ID")
	}
	m.finishPooledRateDispatch(ctx, ref)
	g, _ := m.lockAuth(context.Background(), a.ID)
	count, _, _ := g.pool.rate.accountLocked(a.ID, g.pool.rate.nowFunc())
	g.unlock()
	if count != 1 {
		t.Fatalf("migrated refund left count=%d", count)
	}
}

func TestPoolReviewMixedQuotaAndRateHint(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	registerPoolModel(t, m, "mixed-quota")
	registerPoolModel(t, m, "mixed-rate")
	g, _ := m.lockAuth(context.Background(), "mixed-quota")
	a := g.auths["mixed-quota"]
	a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: time.Now().Add(time.Minute)}
	a.Unavailable = true
	a.NextRetryAfter = a.Quota.NextRecoverAt
	r := g.pool.rate
	r.config.Enabled = true
	r.config.FailFast = true
	_, limit, _ := r.accountLocked("mixed-rate", r.nowFunc())
	for range limit {
		r.startLocked("mixed-rate", r.nowFunc())
	}
	g.unlock()
	for _, pinned := range []string{"", "mixed-quota"} {
		for range 3 {
			opts := exec.Options{}
			if pinned != "" {
				opts.Metadata = map[string]any{exec.PinnedAuthMetadataKey: pinned}
			}
			_, err := m.SelectAuth(context.Background(), "codex", "pool-model", opts)
			if statusCodeFromError(err) != 429 {
				t.Fatalf("pinned=%s error=%v", pinned, err)
			}
		}
	}
}
func TestPoolReviewClearDisabledCooldownCoversLegacyAndPooled(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	until := time.Now().Add(time.Hour)
	for _, provider := range []string{"codex", "antigravity"} {
		id := "clear-" + provider
		if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: provider}); err != nil {
			t.Fatal(err)
		}
		guard, err := m.lockAuth(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		a := guard.auths[id]
		a.Disabled, a.Status, a.Unavailable, a.NextRetryAfter = true, StatusDisabled, true, until
		guard.unlock()
	}
	if !m.clearDisabledCooldownStates(&internalconfig.Config{}) {
		t.Fatal("expected cooldown states to be cleared")
	}
	for _, provider := range []string{"codex", "antigravity"} {
		got, _ := m.GetByID("clear-" + provider)
		if got == nil || got.Unavailable || !got.NextRetryAfter.IsZero() {
			t.Fatalf("%s cooldown not cleared: %+v", provider, got)
		}
	}
}
