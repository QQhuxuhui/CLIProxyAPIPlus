package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	pluginapi "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// A plugin that delegates to the built-in round robin must rotate across pooled picks.
func TestCodexPoolPluginDelegateRoundRobinRotates(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	registerPoolModel(t, m, "delegate-a")
	registerPoolModel(t, m, "delegate-b")
	m.SetPluginScheduler(&fakePluginScheduler{handled: true, resp: pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin}})
	first, err := m.SelectAuth(context.Background(), "codex", "pool-model", exec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.SelectAuth(context.Background(), "codex", "pool-model", exec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("delegated round robin picked %s twice", first.ID)
	}
}

// A session binding must not bypass the plugin scheduler: the plugin decides first.
func TestCodexPoolBindingDoesNotBypassPluginScheduler(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	registerPoolModel(t, m, "bind-plugin-a")
	registerPoolModel(t, m, "bind-plugin-b")
	var firstID string
	scheduler := &fakePluginScheduler{}
	scheduler.pick = func(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
		if scheduler.calls == 1 {
			return pluginapi.SchedulerPickResponse{}, false, nil
		}
		for _, c := range req.Candidates {
			if c.ID != firstID {
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: c.ID}, true, nil
			}
		}
		return pluginapi.SchedulerPickResponse{}, false, nil
	}
	m.SetPluginScheduler(scheduler)
	opts := func() exec.Options { return exec.Options{Headers: http.Header{"Session-Id": {"bind-plugin-session"}}} }
	first, err := m.SelectAuth(context.Background(), "codex", "pool-model", opts())
	if err != nil {
		t.Fatal(err)
	}
	firstID = first.ID
	second, err := m.SelectAuth(context.Background(), "codex", "pool-model", opts())
	if err != nil {
		t.Fatal(err)
	}
	if scheduler.calls != 2 {
		t.Fatalf("plugin scheduler called %d times, want 2", scheduler.calls)
	}
	if second.ID == firstID {
		t.Fatalf("binding overrode the plugin choice: got %s again", second.ID)
	}
	// Once the plugin stops handling, the binding still provides stickiness.
	scheduler.pick = func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
		return pluginapi.SchedulerPickResponse{}, false, nil
	}
	third, err := m.SelectAuth(context.Background(), "codex", "pool-model", opts())
	if err != nil {
		t.Fatal(err)
	}
	if third.ID != firstID {
		t.Fatalf("unhandled plugin pick lost the binding: got %s want %s", third.ID, firstID)
	}
}

// Dropping a cooled-down model on reconcile must mark the pooled cooldown records dirty.
func TestCodexPoolReconcileMarksClearedCooldownDirty(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.SetConfig(&internalconfig.Config{SaveCooldownStatus: true})
	store := &recordingCooldownStateStore{}
	m.SetCooldownStateStore(store)
	const id = "reconcile-cooldown"
	if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "old-model"}})
	t.Cleanup(func() { reg.UnregisterClient(id) })
	m.ReconcileRegistryModelStates(context.Background(), id)
	m.MarkResult(context.Background(), Result{AuthID: id, Provider: "codex", Model: "old-model", Error: &Error{HTTPStatus: 429, Message: "quota"}})
	p := m.codexPools.Load()
	if err := p.saveCooldowns(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if p.cooldownDirty.Load() {
		t.Fatal("fixture: cooldown still dirty after save")
	}
	reg.RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "new-model"}})
	m.ReconcileRegistryModelStates(context.Background(), id)
	if a, _ := m.GetByID(id); a != nil && a.ModelStates["old-model"] != nil {
		t.Fatal("fixture: old model state not cleared")
	}
	if !p.cooldownDirty.Load() {
		t.Fatal("cleared cooldown was not marked for persistence")
	}
}

func registerPoolModelWithPriority(t *testing.T, m *Manager, id, priority string) {
	t.Helper()
	if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex", Attributes: map[string]string{"priority": priority}}); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "pool-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	m.ReconcileRegistryModelStates(context.Background(), id)
}

// The mixed-provider route previews the pooled pick; a binding there must still ask the plugin.
func TestCodexPoolMixedBindingDoesNotBypassPluginScheduler(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "claude"})
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	registerPoolModel(t, m, "mixed-plugin-a")
	registerPoolModel(t, m, "mixed-plugin-b")
	if _, err := m.Register(context.Background(), &Auth{ID: "mixed-plugin-claude", Provider: "claude", Attributes: map[string]string{"priority": "-5"}}); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient("mixed-plugin-claude", "claude", []*registry.ModelInfo{{ID: "pool-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("mixed-plugin-claude") })
	var firstID string
	codexCalls := 0
	scheduler := &fakePluginScheduler{}
	scheduler.pick = func(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
		if req.Provider != "codex" {
			return pluginapi.SchedulerPickResponse{}, false, nil
		}
		codexCalls++
		if firstID == "" {
			return pluginapi.SchedulerPickResponse{}, false, nil
		}
		for _, c := range req.Candidates {
			if c.ID != firstID {
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: c.ID}, true, nil
			}
		}
		return pluginapi.SchedulerPickResponse{}, false, nil
	}
	m.SetPluginScheduler(scheduler)
	pick := func() *Auth {
		t.Helper()
		opts := exec.Options{Headers: http.Header{"Session-Id": {"mixed-plugin-session"}}}
		a, _, provider, err := m.pickPoolMixed(context.Background(), []string{"codex", "claude"}, "pool-model", opts, nil)
		if err != nil || provider != "codex" {
			t.Fatalf("pick: provider=%q err=%v", provider, err)
		}
		return a
	}
	firstID = pick().ID
	before := codexCalls
	second := pick()
	if codexCalls == before {
		t.Fatal("plugin scheduler was not consulted on the bound mixed pick")
	}
	if second.ID == firstID {
		t.Fatalf("binding overrode the plugin choice on the mixed route: got %s again", second.ID)
	}
}

// Without a plugin decision, a healthy lower-priority binding must survive a higher-priority
// credential becoming available, as with the legacy session-affinity selector.
func TestCodexPoolUnhandledPluginKeepsLowerPriorityBinding(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	m.SetPluginScheduler(&fakePluginScheduler{})
	registerPoolModelWithPriority(t, m, "prio-low", "0")
	opts := func() exec.Options { return exec.Options{Headers: http.Header{"Session-Id": {"prio-session"}}} }
	first, err := m.SelectAuth(context.Background(), "codex", "pool-model", opts())
	if err != nil || first.ID != "prio-low" {
		t.Fatalf("first pick = %v, %v", first, err)
	}
	registerPoolModelWithPriority(t, m, "prio-high", "10")
	second, err := m.SelectAuth(context.Background(), "codex", "pool-model", opts())
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != "prio-low" {
		t.Fatalf("session switched to %s after a higher-priority credential recovered", second.ID)
	}
}

// A plugin error on the bound path must be returned, not treated as a stale binding.
func TestCodexPoolBindingReturnsPluginError(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	registerPoolModel(t, m, "plugin-err-a")
	registerPoolModel(t, m, "plugin-err-b")
	errPlugin := errors.New("fixture plugin failure")
	scheduler := &fakePluginScheduler{}
	scheduler.pick = func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
		if scheduler.calls == 2 {
			return pluginapi.SchedulerPickResponse{}, true, errPlugin
		}
		return pluginapi.SchedulerPickResponse{}, false, nil
	}
	m.SetPluginScheduler(scheduler)
	opts := func() exec.Options { return exec.Options{Headers: http.Header{"Session-Id": {"plugin-err-session"}}} }
	if _, err := m.SelectAuth(context.Background(), "codex", "pool-model", opts()); err != nil {
		t.Fatal(err)
	}
	_, err := m.SelectAuth(context.Background(), "codex", "pool-model", opts())
	if !errors.Is(err, errPlugin) {
		t.Fatalf("plugin error swallowed: err=%v calls=%d", err, scheduler.calls)
	}
	if scheduler.calls != 2 {
		t.Fatalf("plugin called %d times, want 2 (no reselection after its error)", scheduler.calls)
	}
}
