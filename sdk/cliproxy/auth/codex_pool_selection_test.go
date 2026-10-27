package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func registerPoolModel(t *testing.T, m *Manager, id string) {
	t.Helper()
	_, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "pool-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	m.ReconcileRegistryModelStates(context.Background(), id)
}
func TestCodexPoolSelectionIgnoresGlobalLocks(t *testing.T) {
	m := newPoolStorageManager(t, 4)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	registerPoolModel(t, m, "pool-pick")
	m.mu.Lock()
	m.rateCap.mu.Lock()
	m.scheduler.mu.Lock()
	defer m.mu.Unlock()
	defer m.rateCap.mu.Unlock()
	defer m.scheduler.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		a, err := m.SelectAuth(context.Background(), "codex", "pool-model", exec.Options{})
		if err == nil {
			_, err = m.beginRateAttempt(context.Background(), a)
		}
		done <- err
	}()
	awaitPoolOperation(t, done)
}
func TestCodexPoolSelectionBindingSurvivesSlotMigration(t *testing.T) {
	m := newPoolStorageManager(t, 4)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	registerPoolModel(t, m, "pool-pick-a")
	registerPoolModel(t, m, "pool-pick-b")
	opts := exec.Options{Headers: http.Header{"Session-Id": {"pool-session"}}}
	first, err := m.SelectAuth(context.Background(), "codex", "pool-model", opts)
	if err != nil {
		t.Fatal(err)
	}
	pools := m.codexPools.Load()
	source := pools.table.Load().owners[codexSlot(first.ID)]
	var target *codexAccountPool
	for _, p := range pools.table.Load().pools {
		if p != source {
			target = p
			break
		}
	}
	if err := pools.moveSlot(context.Background(), codexSlot(first.ID), target); err != nil {
		t.Fatal(err)
	}
	second, err := m.SelectAuth(context.Background(), "codex", "pool-model", opts)
	if err != nil || second.ID != first.ID {
		t.Fatalf("binding changed: first=%v second=%v error=%v", first, second, err)
	}
}

func TestCodexPoolSubagentInheritsParentBinding(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	registerPoolModel(t, m, "parent-bind-a")
	registerPoolModel(t, m, "parent-bind-b")
	parent, err := m.SelectAuth(context.Background(), "codex", "pool-model", exec.Options{Headers: http.Header{"X-Claude-Code-Session-Id": {"root"}}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := m.SelectAuth(context.Background(), "codex", "pool-model", exec.Options{Headers: http.Header{"X-Claude-Code-Session-Id": {"root"}, "X-Claude-Code-Agent-Id": {"child"}}})
	if err != nil {
		t.Fatal(err)
	}
	if parent.ID != child.ID {
		t.Fatalf("subagent did not inherit parent: %s -> %s", parent.ID, child.ID)
	}
}
func TestCodexPoolBindingLookupSeesCurrentPool(t *testing.T) {
	m := newPoolStorageManager(t, 2)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	registerPoolModel(t, m, "lookup-pool")
	opts := exec.Options{Headers: http.Header{"Session-Id": {"lookup-session"}}}
	chosen, err := m.SelectAuth(context.Background(), "codex", "pool-model", opts)
	if err != nil {
		t.Fatal(err)
	}
	key := CanonicalSessionID(opts.Headers, nil, nil)
	found, status := m.LookupSessionAffinity("codex", "pool-model", key)
	if status != "bound" || found == nil || found.ID != chosen.ID {
		t.Fatalf("lookup=%v %s", found, status)
	}
}

func TestCodexPoolSidePriorityUsesAvailableCandidate(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	for _, spec := range []struct {
		id, priority string
		side         bool
	}{{"priority-high", "5", false}, {"priority-low", "1", false}, {"priority-side", "3", true}} {
		attrs := map[string]string{"priority": spec.priority}
		if spec.side {
			attrs[AttributeAuthKind] = AuthKindAPIKey
			attrs[AttributeSource] = AuthSourceConfig
			attrs[AttributeAPIKey] = "fixture"
		}
		if _, err := m.Register(context.Background(), &Auth{ID: spec.id, Provider: "codex", Attributes: attrs}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(spec.id, "codex", []*registry.ModelInfo{{ID: "pool-model"}})
		id := spec.id
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		m.RefreshSchedulerEntry(id)
	}
	m.MarkResult(context.Background(), Result{AuthID: "priority-high", Provider: "codex", Model: "pool-model", Error: &Error{HTTPStatus: 429, Message: "quota"}})
	chosen, err := m.SelectAuth(context.Background(), "codex", "pool-model", exec.Options{})
	if err != nil || chosen.ID != "priority-side" {
		t.Fatalf("selected=%v err=%v", chosen, err)
	}
}
func TestCodexPoolMixedProviderKeepsHigherLegacyPriority(t *testing.T) {
	m := newPoolStorageManager(t, 2)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "claude"})
	registerPoolModel(t, m, "mixed-pooled")
	_, err := m.Register(context.Background(), &Auth{ID: "mixed-legacy", Provider: "claude", Attributes: map[string]string{"priority": "10"}})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient("mixed-legacy", "claude", []*registry.ModelInfo{{ID: "pool-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("mixed-legacy") })
	m.RefreshSchedulerEntry("mixed-legacy")
	chosen, _, provider, err := m.pickNextMixed(context.Background(), []string{"codex", "claude"}, "pool-model", exec.Options{}, nil)
	if err != nil || provider != "claude" || chosen.ID != "mixed-legacy" {
		t.Fatalf("selected=%v provider=%s err=%v", chosen, provider, err)
	}
}
func TestCodexPoolLCPBindingKeepsGrowingConversation(t *testing.T) {
	m := newPoolStorageManager(t, 2)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	registerPoolModel(t, m, "lcp-pool-a")
	registerPoolModel(t, m, "lcp-pool-b")
	firstOpts := exec.Options{OriginalRequest: []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"first"}]}`), Metadata: map[string]any{exec.CallerScopeMetadataKey: "pool-lcp"}}
	first, err := m.SelectAuth(context.Background(), "codex", "pool-model", firstOpts)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := firstOpts.Metadata[exec.LCPAffinitySessionIDMetadataKey].(string)
	if id == "" {
		t.Fatal("LCP session identity was not created")
	}
	nextOpts := exec.Options{OriginalRequest: []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":"second"}]}`), Metadata: map[string]any{exec.CallerScopeMetadataKey: "pool-lcp"}}
	next, err := m.SelectAuth(context.Background(), "codex", "pool-model", nextOpts)
	if err != nil || next.ID != first.ID {
		t.Fatalf("LCP binding changed: first=%v next=%v err=%v", first, next, err)
	}
}
