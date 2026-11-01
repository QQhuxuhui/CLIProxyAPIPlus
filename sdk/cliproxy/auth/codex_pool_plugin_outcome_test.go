package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type poolOutcomeSchedulerFunc func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error)

func (f poolOutcomeSchedulerFunc) PickAuth(ctx context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	return f(ctx, req)
}

func TestCodexPoolMixedPluginErrorStopsFallback(t *testing.T) {
	for _, bound := range []bool{false, true} {
		name := "initial"
		if bound {
			name = "bound"
		}
		t.Run(name, func(t *testing.T) {
			m := newPoolStorageManager(t, 1)
			m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
			m.RegisterExecutor(&retryRoundCallExecutor{identifier: "claude"})
			m.SetSelector(NewSessionAffinitySelector(&FillFirstSelector{}))
			registerPoolModel(t, m, "outcome-codex")
			if _, err := m.Register(context.Background(), &Auth{ID: "outcome-claude", Provider: "claude", Attributes: map[string]string{"priority": "-5"}}); err != nil {
				t.Fatal(err)
			}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient("outcome-claude", "claude", []*registry.ModelInfo{{ID: "pool-model"}})
			t.Cleanup(func() { reg.UnregisterClient("outcome-claude") })
			m.ReconcileRegistryModelStates(context.Background(), "outcome-claude")
			opts := exec.Options{Headers: http.Header{"Session-Id": {"outcome-mixed"}}}
			if bound {
				if _, _, _, err := m.pickNextMixed(context.Background(), []string{"codex", "claude"}, "pool-model", opts, nil); err != nil {
					t.Fatal(err)
				}
			}
			expected := errors.New("plugin selection failed")
			calls := 0
			m.SetPluginScheduler(poolOutcomeSchedulerFunc(func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
				calls++
				if calls == 1 {
					return pluginapi.SchedulerPickResponse{}, true, expected
				}
				return pluginapi.SchedulerPickResponse{}, false, nil
			}))
			a, _, _, err := m.pickNextMixed(context.Background(), []string{"codex", "claude"}, "pool-model", opts, nil)
			if err != expected || a != nil || calls != 1 {
				t.Fatalf("auth=%v error=%v calls=%d; want original plugin error and one call", a, err, calls)
			}
		})
	}
}

func TestCodexPoolMixedLegacyPluginErrorStopsFallback(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "claude"})
	registerPoolModel(t, m, "legacy-error-codex")
	if _, err := m.Register(context.Background(), &Auth{ID: "legacy-error-claude", Provider: "claude", Attributes: map[string]string{"priority": "5"}}); err != nil {
		t.Fatal(err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("legacy-error-claude", "claude", []*registry.ModelInfo{{ID: "pool-model"}})
	t.Cleanup(func() { reg.UnregisterClient("legacy-error-claude") })
	m.ReconcileRegistryModelStates(context.Background(), "legacy-error-claude")
	expected := errors.New("legacy plugin selection failed")
	calls := 0
	m.SetPluginScheduler(poolOutcomeSchedulerFunc(func(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
		calls++
		if req.Provider == "codex" {
			return pluginapi.SchedulerPickResponse{}, false, nil
		}
		return pluginapi.SchedulerPickResponse{}, true, expected
	}))
	a, _, _, err := m.pickNextMixed(context.Background(), []string{"codex", "claude"}, "pool-model", exec.Options{}, nil)
	if err != expected || a != nil || calls != 2 {
		t.Fatalf("auth=%v error=%v calls=%d; want original legacy plugin error after two calls", a, err, calls)
	}
}

func TestCodexPoolMixedPluginErrorSurvivesStaleSelectionRetry(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "claude"})
	registerPoolModel(t, m, "retry-error-codex")
	reg := registry.GetGlobalRegistry()
	for _, id := range []string{"retry-error-claude-a", "retry-error-claude-b"} {
		if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "claude", Attributes: map[string]string{"priority": "5"}}); err != nil {
			t.Fatal(err)
		}
		reg.RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "pool-model"}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
		m.ReconcileRegistryModelStates(context.Background(), id)
	}
	expected := errors.New("plugin failed after stale selection")
	legacyCalls := 0
	m.SetPluginScheduler(poolOutcomeSchedulerFunc(func(ctx context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
		if req.Provider == "codex" {
			return pluginapi.SchedulerPickResponse{}, false, nil
		}
		legacyCalls++
		if legacyCalls == 1 {
			// Replacing the selected credential invalidates the selection snapshot.
			a, _ := m.GetByID("retry-error-claude-a")
			if _, err := m.Register(ctx, a); err != nil {
				return pluginapi.SchedulerPickResponse{}, true, err
			}
			return pluginapi.SchedulerPickResponse{Handled: true, AuthID: a.ID}, true, nil
		}
		return pluginapi.SchedulerPickResponse{}, true, expected
	}))
	a, _, _, err := m.pickNextMixed(context.Background(), []string{"codex", "claude"}, "pool-model", exec.Options{}, nil)
	if err != expected || a != nil || legacyCalls != 2 {
		t.Fatalf("auth=%v error=%v legacy calls=%d; want original plugin error from retry", a, err, legacyCalls)
	}
}

func TestCodexPoolConcurrentPluginChoicesSurviveBinding(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		name := "single"
		if mixed {
			name = "mixed"
		}
		t.Run(name, func(t *testing.T) {
			m := newPoolStorageManager(t, 1)
			m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
			m.SetSelector(NewSessionAffinitySelector(&FillFirstSelector{}))
			registerPoolModel(t, m, "outcome-a")
			registerPoolModel(t, m, "outcome-b")
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			type requestKey struct{}
			m.SetPluginScheduler(poolOutcomeSchedulerFunc(func(ctx context.Context, _ pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
				id := ctx.Value(requestKey{}).(string)
				if id == "outcome-a" {
					close(entered)
					<-release
				}
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: id}, true, nil
			}))
			pick := func(id string) (*Auth, error) {
				ctx := context.WithValue(context.Background(), requestKey{}, id)
				opts := exec.Options{Headers: http.Header{"Session-Id": {"outcome-concurrent"}}}
				if mixed {
					a, _, _, err := m.pickNextMixed(ctx, []string{"codex", "claude"}, "pool-model", opts, nil)
					return a, err
				}
				return m.SelectAuth(ctx, "codex", "pool-model", opts)
			}
			type outcome struct {
				auth *Auth
				err  error
			}
			done := make(chan outcome, 1)
			go func() { a, err := pick("outcome-a"); done <- outcome{a, err} }()
			<-entered
			b, err := pick("outcome-b")
			unblock()
			first := <-done
			if err != nil || first.err != nil {
				t.Fatalf("selection errors: %v / %v", err, first.err)
			}
			if first.auth.ID != "outcome-a" || b.ID != "outcome-b" {
				t.Fatalf("plugin choices overwritten: first=%s second=%s", first.auth.ID, b.ID)
			}
		})
	}
}

func TestCodexPoolHandledPickDoesNotSeedAffinity(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		name := "single"
		if mixed {
			name = "mixed"
		}
		t.Run(name, func(t *testing.T) {
			m := newPoolStorageManager(t, 1)
			m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
			m.SetSelector(NewSessionAffinitySelector(&FillFirstSelector{}))
			registerPoolModel(t, m, "outcome-a")
			registerPoolModel(t, m, "outcome-b")
			m.SetPluginScheduler(&fakePluginScheduler{handled: true, resp: pluginapi.SchedulerPickResponse{Handled: true, AuthID: "outcome-b"}})
			pick := func() (*Auth, error) {
				opts := exec.Options{Headers: http.Header{"Session-Id": {"outcome-lifecycle"}}}
				if mixed {
					a, _, _, err := m.pickNextMixed(context.Background(), []string{"codex", "claude"}, "pool-model", opts, nil)
					return a, err
				}
				return m.SelectAuth(context.Background(), "codex", "pool-model", opts)
			}
			first, err := pick()
			if err != nil || first.ID != "outcome-b" {
				t.Fatalf("plugin pick=%v err=%v", first, err)
			}
			m.SetPluginScheduler(&fakePluginScheduler{})
			second, err := pick()
			if err != nil || second.ID != "outcome-a" {
				t.Fatalf("built-in pick=%v err=%v; want fill-first outcome-a", second, err)
			}
		})
	}
}
