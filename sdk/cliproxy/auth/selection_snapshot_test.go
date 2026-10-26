package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func selectionSnapshotFixture(id string, models int) *Auth {
	a := &Auth{
		ID: id, Provider: "codex", Status: StatusActive, RegistrationEpoch: 1, Generation: 1,
		Attributes:  map[string]string{"priority": "2", "team": "alpha", "api_key": "fixture-key"},
		Metadata:    map[string]any{"access_token": "fixture-token", "tenant": "one", AttributeWeight: int64(2), "websockets": true},
		Quota:       QuotaState{Signals: map[string]string{"source": "fixture"}},
		ModelStates: make(map[string]*ModelState, models),
	}
	for i := range models {
		a.ModelStates[fmt.Sprintf("model-%d", i)] = &ModelState{Status: StatusActive, Quota: QuotaState{Signals: map[string]string{"source": "fixture"}}}
	}
	a.EnsureIndex()
	return a
}

func pickSelectionSnapshot(m *Manager, mixed bool, opts cliproxyexecutor.Options) (*Auth, error) {
	if mixed {
		a, _, _, err := m.pickNextMixedLegacy(context.Background(), []string{"codex", "gemini"}, "", opts, nil)
		return a, err
	}
	a, _, err := m.pickNextLegacy(context.Background(), "codex", "", opts, nil)
	return a, err
}

func TestManagerSelectionSnapshotRejectsChangedAuth(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, change := range []string{"disabled", "status", "removed", "registered-again", "cooldown", "provider"} {
			t.Run(fmt.Sprintf("mixed=%v/%s", mixed, change), func(t *testing.T) {
				m := NewManager(nil, &RoundRobinSelector{}, nil)
				m.executors["codex"] = schedulerBenchmarkExecutor{id: "codex"}
				m.auths["a"] = selectionSnapshotFixture("a", 2)
				m.SetPluginScheduler(&fakePluginScheduler{pick: func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
					if change == "registered-again" {
						if _, err := m.Register(context.Background(), selectionSnapshotFixture("a", 2)); err != nil {
							t.Fatal(err)
						}
					} else {
						m.mu.Lock()
						a := m.auths["a"]
						switch change {
						case "disabled":
							a.Disabled = true
						case "status":
							a.Status = StatusDisabled
						case "removed":
							delete(m.auths, "a")
						case "cooldown":
							a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: time.Now().Add(time.Hour)}
						case "provider":
							a.Provider = "gemini"
						}
						a.Generation++
						m.mu.Unlock()
					}
					return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "a"}, true, nil
				}})
				got, err := pickSelectionSnapshot(m, mixed, cliproxyexecutor.Options{})
				var authErr *Error
				if got != nil || !errors.As(err, &authErr) || authErr.Code != "auth_unavailable" || !authErr.Retryable {
					t.Fatalf("selection returned stale auth: auth=%v err=%v; want retryable auth_unavailable", got != nil, err)
				}
			})
		}
	}
}

func TestManagerSelectionSnapshotUsesCurrentCredentials(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint(mixed), func(t *testing.T) {
			m := NewManager(nil, &RoundRobinSelector{}, nil)
			m.executors["codex"] = schedulerBenchmarkExecutor{id: "codex"}
			m.auths["a"] = selectionSnapshotFixture("a", 3)
			m.SetPluginScheduler(&fakePluginScheduler{pick: func(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
				if req.Candidates[0].Attributes["team"] != "alpha" || req.Candidates[0].Attributes["api_key"] != "" {
					t.Fatal("plugin projection changed or exposed credentials")
				}
				m.mu.Lock()
				m.auths["a"].Metadata["access_token"] = "refreshed-token"
				m.auths["a"].Generation++
				m.mu.Unlock()
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "a"}, true, nil
			}})
			got, err := pickSelectionSnapshot(m, mixed, cliproxyexecutor.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Metadata["access_token"] != "refreshed-token" || len(got.ModelStates) != 3 || got.Quota.Signals["source"] != "fixture" {
				t.Fatal("execution did not receive the current full credential")
			}
			got.Metadata["access_token"] = "private-change"
			got.ModelStates["model-0"].Quota.Signals["source"] = "private-change"
			if m.auths["a"].Metadata["access_token"] != "refreshed-token" || m.auths["a"].ModelStates["model-0"].Quota.Signals["source"] != "fixture" {
				t.Fatal("execution snapshot aliases manager maps")
			}
		})
	}
}

type selectionSnapshotSelectorFunc func(context.Context, string, string, cliproxyexecutor.Options, []*Auth) (*Auth, error)

func (f selectionSnapshotSelectorFunc) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	return f(ctx, provider, model, opts, auths)
}

func TestManagerSelectionSnapshotCustomFallback(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, depth := range []int{0, 1, 2} {
			t.Run(fmt.Sprintf("mixed=%v/affinity-depth=%d", mixed, depth), func(t *testing.T) {
				called := false
				var selector Selector = selectionSnapshotSelectorFunc(func(_ context.Context, _, _ string, _ cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
					called = true
					a := auths[0]
					if a.Metadata["tenant"] != "one" || len(a.ModelStates) != 3 || a.Quota.Signals["source"] != "fixture" {
						t.Fatal("custom selector did not receive its full snapshot")
					}
					a.Metadata["tenant"] = "selector-result"
					return a, nil
				})
				for range depth {
					wrapped := NewSessionAffinitySelector(selector)
					t.Cleanup(wrapped.Stop)
					selector = wrapped
				}
				m := NewManager(nil, selector, nil)
				m.executors["codex"] = schedulerBenchmarkExecutor{id: "codex"}
				m.auths["a"] = selectionSnapshotFixture("a", 3)
				m.SetPluginScheduler(&fakePluginScheduler{pick: func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
					return pluginapi.SchedulerPickResponse{}, false, nil
				}})
				got, err := pickSelectionSnapshot(m, mixed, cliproxyexecutor.Options{})
				if err != nil || !called || got.Metadata["tenant"] != "selector-result" {
					t.Fatalf("custom fallback result changed: called=%v err=%v", called, err)
				}
				if m.auths["a"].Metadata["tenant"] != "one" {
					t.Fatal("custom selector changed manager metadata")
				}
			})
		}
	}
}

func TestManagerSelectionSnapshotRevalidatesModel(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, change := range []string{"target-cooldown", "unrelated-cooldown", "model-removed"} {
			t.Run(fmt.Sprintf("mixed=%v/%s", mixed, change), func(t *testing.T) {
				m := NewManager(nil, &RoundRobinSelector{}, nil)
				m.executors["codex"] = schedulerBenchmarkExecutor{id: "codex"}
				a := selectionSnapshotFixture(t.Name(), 3)
				delete(a.Attributes, "api_key")
				m.auths[a.ID] = a
				const route, target = "snapshot-alias", "model-0"
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(a.ID, "codex", []*registry.ModelInfo{{ID: route}, {ID: target}})
				t.Cleanup(func() { reg.UnregisterClient(a.ID) })
				m.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
					"codex": {{Name: target, Alias: route, Fork: true}},
				})
				m.SetPluginScheduler(&fakePluginScheduler{pick: func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
					if change == "model-removed" {
						reg.UnregisterClient(a.ID)
					} else {
						m.mu.Lock()
						model := target
						if change == "unrelated-cooldown" {
							model = route
						}
						a.ModelStates[model] = &ModelState{Status: StatusError, Unavailable: true, NextRetryAfter: time.Now().Add(time.Hour)}
						a.Generation++
						m.mu.Unlock()
					}
					return pluginapi.SchedulerPickResponse{Handled: true, AuthID: a.ID}, true, nil
				}})
				var got *Auth
				var err error
				if mixed {
					got, _, _, err = m.pickNextMixedLegacy(context.Background(), []string{"codex"}, route, cliproxyexecutor.Options{}, nil)
				} else {
					got, _, err = m.pickNextLegacy(context.Background(), "codex", route, cliproxyexecutor.Options{}, nil)
				}
				if change == "unrelated-cooldown" {
					if err != nil || got == nil {
						t.Fatalf("unrelated alias cooldown blocked healthy target: %v", err)
					}
				} else {
					var authErr *Error
					if got != nil || !errors.As(err, &authErr) || authErr.Code != "auth_unavailable" || !authErr.Retryable {
						t.Fatalf("model change was not revalidated: auth=%v err=%v", got != nil, err)
					}
				}
			})
		}
	}
}

func TestManagerSelectionSnapshotUsesCurrentExecutor(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint(mixed), func(t *testing.T) {
			m := NewManager(nil, &RoundRobinSelector{}, nil)
			m.executors["codex"] = &schedulerBenchmarkExecutor{id: "codex"}
			m.auths["a"] = selectionSnapshotFixture("a", 2)
			replacement := &schedulerBenchmarkExecutor{id: "codex"}
			m.SetPluginScheduler(&fakePluginScheduler{pick: func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
				m.RegisterExecutor(replacement)
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "a"}, true, nil
			}})
			var exec ProviderExecutor
			var err error
			if mixed {
				_, exec, _, err = m.pickNextMixedLegacy(context.Background(), []string{"codex"}, "", cliproxyexecutor.Options{}, nil)
			} else {
				_, exec, err = m.pickNextLegacy(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
			}
			if err != nil || exec != replacement {
				t.Fatalf("executor was not refreshed: err=%v", err)
			}
		})
	}
}

func TestManagerSelectionSnapshotRevalidatesCredentialKind(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint(mixed), func(t *testing.T) {
			m := NewManager(nil, &RoundRobinSelector{}, nil)
			m.executors["codex"] = schedulerBenchmarkExecutor{id: "codex"}
			m.auths["a"] = selectionSnapshotFixture("a", 2)
			m.SetPluginScheduler(&fakePluginScheduler{pick: func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
				m.mu.Lock()
				delete(m.auths["a"].Attributes, "api_key")
				m.auths["a"].Generation++
				m.mu.Unlock()
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "a"}, true, nil
			}})
			ctx := withRequiredAuthKind(context.Background(), AuthKindAPIKey)
			var got *Auth
			var err error
			if mixed {
				got, _, _, err = m.pickNextMixedLegacy(ctx, []string{"codex"}, "", cliproxyexecutor.Options{}, nil)
			} else {
				got, _, err = m.pickNextLegacy(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
			}
			var authErr *Error
			if got != nil || !errors.As(err, &authErr) || authErr.Code != "auth_unavailable" || !authErr.Retryable {
				t.Fatalf("credential kind changed after selection: auth=%v err=%v", got != nil, err)
			}
		})
	}
}

func TestSelectionSnapshotIsolation(t *testing.T) {
	m := NewManager(nil, &WeightedRoundRobinSelector{}, nil)
	original := selectionSnapshotFixture("a", 3)
	m.mu.RLock()
	_, snapshots, err := m.availableAuthsForSelector(context.Background(), m.selector, []*Auth{original}, "codex", "", time.Now())
	m.mu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	original.Attributes["priority"] = "20"
	original.Metadata[AttributeWeight] = int64(7)
	original.Metadata["websockets"] = false
	if authPriority(snapshots[0]) != 2 || authWeight(snapshots[0]) != 2 || !authWebsocketsEnabled(snapshots[0]) {
		t.Fatal("selection snapshot changed after manager mutation")
	}
	if !reflect.DeepEqual(m.cachedSchedulerCandidates(snapshots), schedulerAuthCandidates(snapshots)) {
		t.Fatal("cached plugin projection differs from uncached projection")
	}
}

func TestSelectionSnapshotAllocationsIgnoreModelStateCount(t *testing.T) {
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	allocations := func(models int) float64 {
		auths := []*Auth{selectionSnapshotFixture("a", models)}
		return testing.AllocsPerRun(50, func() {
			m.mu.RLock()
			_, _, err := m.availableAuthsForSelector(context.Background(), m.selector, auths, "codex", "", time.Now())
			m.mu.RUnlock()
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	small, large := allocations(1), allocations(64)
	if large > small+2 {
		t.Fatalf("candidate allocations grow with unrelated model state: 1 model=%g, 64 models=%g", small, large)
	}
}

func TestManagerSelectionSnapshotWeightedMetadata(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint(mixed), func(t *testing.T) {
			affinity := NewSessionAffinitySelector(&WeightedRoundRobinSelector{})
			t.Cleanup(affinity.Stop)
			m := NewManager(nil, affinity, nil)
			m.executors["codex"] = schedulerBenchmarkExecutor{id: "codex"}
			for id, weight := range map[string]any{"a": json.Number("0"), "b": "1", "c": float64(3)} {
				a := selectionSnapshotFixture(id, 2)
				a.Metadata[AttributeWeight] = weight
				m.auths[id] = a
			}
			counts := make(map[string]int)
			for range 8 {
				a, err := pickSelectionSnapshot(m, mixed, cliproxyexecutor.Options{})
				if err != nil {
					t.Fatal(err)
				}
				counts[a.ID]++
			}
			if counts["a"] != 0 || counts["b"] != 2 || counts["c"] != 6 {
				t.Fatalf("metadata weights were not preserved: %v", counts)
			}
		})
	}
}

func TestManagerSelectionSnapshotWebsocketPreference(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	m.executors["codex"] = schedulerBenchmarkExecutor{id: "codex"}
	first, second := selectionSnapshotFixture("a", 2), selectionSnapshotFixture("b", 2)
	first.Metadata["websockets"] = true
	first.Attributes["websockets"] = "false"
	second.Metadata["websockets"] = "true"
	m.auths[first.ID], m.auths[second.ID] = first, second
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	a, _, err := m.pickNextLegacy(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
	if err != nil || a == nil || a.ID != second.ID {
		t.Fatalf("websocket preference changed: auth=%v err=%v", a, err)
	}
}

func TestManagerSelectionSnapshotConcurrentResults(t *testing.T) {
	affinity := NewSessionAffinitySelector(&WeightedRoundRobinSelector{})
	t.Cleanup(affinity.Stop)
	m := NewManager(nil, affinity, nil)
	m.executors["codex"] = schedulerBenchmarkExecutor{id: "codex"}
	for i := range 16 {
		id := fmt.Sprint(i)
		m.auths[id] = selectionSnapshotFixture(id, 8)
	}
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Go(func() {
			for range 30 {
				got, err := pickSelectionSnapshot(m, worker%2 == 0, cliproxyexecutor.Options{})
				if err != nil {
					t.Error(err)
					return
				}
				m.MarkResult(context.Background(), Result{AuthID: got.ID, Provider: "codex", Model: "model-0", Success: true})
			}
		})
	}
	wg.Wait()
}

type selectionSnapshotJSONScheduler struct{}

func (selectionSnapshotJSONScheduler) ReadsSchedulerCandidatesOnly() bool { return true }

func (selectionSnapshotJSONScheduler) PickAuth(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	// Include serialization cost without retaining requests or invoking an external plugin.
	if _, err := json.Marshal(req); err != nil {
		return pluginapi.SchedulerPickResponse{}, true, err
	}
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: req.Candidates[0].ID}, true, nil
}

func BenchmarkSelectionSnapshot(b *testing.B) {
	for _, plugin := range []bool{false, true} {
		b.Run(fmt.Sprintf("4000/plugin=%v", plugin), func(b *testing.B) {
			affinity := NewSessionAffinitySelector(&RoundRobinSelector{})
			b.Cleanup(affinity.Stop)
			m := NewManager(nil, affinity, nil)
			m.executors["codex"] = schedulerBenchmarkExecutor{id: "codex"}
			for i := range 4000 {
				id := fmt.Sprintf("bench-%04d", i)
				m.auths[id] = selectionSnapshotFixture(id, 24)
			}
			if plugin {
				m.SetPluginScheduler(selectionSnapshotJSONScheduler{})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := pickSelectionSnapshot(m, false, cliproxyexecutor.Options{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
