package auth

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// blockAuthDuringPick puts id into credential cooldown while the selection runs.
func blockAuthDuringPick(m *Manager, id string) {
	m.mu.Lock()
	a := m.auths[id]
	a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: time.Now().Add(time.Hour)}
	a.Generation++
	m.mu.Unlock()
}

func TestManagerSelectionSnapshotRepicksStaleAuth(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint(mixed), func(t *testing.T) {
			m := NewManager(nil, &FillFirstSelector{}, nil)
			m.executors["codex"] = schedulerBenchmarkExecutor{id: "codex"}
			m.auths["a"] = selectionSnapshotFixture("a", 1)
			m.auths["b"] = selectionSnapshotFixture("b", 1)
			var picks atomic.Int32
			m.SetPluginScheduler(&fakePluginScheduler{pick: func(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
				if picks.Add(1) == 1 {
					blockAuthDuringPick(m, "a")
					return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "a"}, true, nil
				}
				for _, c := range req.Candidates {
					if c.ID == "a" {
						t.Error("stale auth offered again after re-pick")
					}
				}
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "b"}, true, nil
			}})
			tried := map[string]struct{}{}
			var got *Auth
			var err error
			if mixed {
				got, _, _, err = m.pickNextMixedLegacy(context.Background(), []string{"codex"}, "", cliproxyexecutor.Options{}, tried)
			} else {
				got, _, err = m.pickNextLegacy(context.Background(), "codex", "", cliproxyexecutor.Options{}, tried)
			}
			if err != nil || got == nil || got.ID != "b" {
				t.Fatalf("stale selection was not re-picked: auth=%v err=%v", got, err)
			}
			if len(tried) != 0 {
				t.Fatalf("caller tried set modified: %v", tried)
			}
		})
	}
}

func TestManagerSelectionSnapshotStaleRetriesAreBounded(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint(mixed), func(t *testing.T) {
			m := NewManager(nil, &FillFirstSelector{}, nil)
			m.executors["codex"] = schedulerBenchmarkExecutor{id: "codex"}
			for i := range 10 {
				id := fmt.Sprintf("a%d", i)
				m.auths[id] = selectionSnapshotFixture(id, 1)
			}
			var picks atomic.Int32
			m.SetPluginScheduler(&fakePluginScheduler{pick: func(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
				picks.Add(1)
				id := req.Candidates[0].ID
				blockAuthDuringPick(m, id) // every selection goes stale
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: id}, true, nil
			}})
			var err error
			if mixed {
				_, _, _, err = m.pickNextMixedLegacy(context.Background(), []string{"codex"}, "", cliproxyexecutor.Options{}, nil)
			} else {
				_, _, err = m.pickNextLegacy(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
			}
			var authErr *Error
			if !errors.As(err, &authErr) || authErr.Code != "auth_unavailable" || !authErr.Retryable {
				t.Fatalf("err = %v, want retryable auth_unavailable", err)
			}
			if got := picks.Load(); got != maxStaleSelectionRetries+1 {
				t.Fatalf("picks = %d, want %d", got, maxStaleSelectionRetries+1)
			}
		})
	}
}

func TestManagerExecuteSucceedsAfterStaleSelection(t *testing.T) {
	affinity := NewSessionAffinitySelector(&FillFirstSelector{})
	t.Cleanup(affinity.Stop)
	m := NewManager(nil, affinity, nil)
	m.executors["codex"] = schedulerBenchmarkExecutor{id: "codex"}
	for _, id := range []string{"a", "b"} {
		a := selectionSnapshotFixture(id, 1)
		delete(a.Attributes, "api_key")
		m.auths[id] = a
	}
	var first atomic.Bool
	m.SetPluginScheduler(&fakePluginScheduler{pick: func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
		if !first.Swap(true) {
			blockAuthDuringPick(m, "a")
			return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "a"}, true, nil
		}
		return pluginapi.SchedulerPickResponse{}, false, nil
	}})
	if _, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("request failed after a stale selection: %v", err)
	}
}
