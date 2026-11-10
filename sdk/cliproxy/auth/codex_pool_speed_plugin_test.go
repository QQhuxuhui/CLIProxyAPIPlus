package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// A plugin scheduler's choice stays final while the speed ranking is selecting: it is neither
// bound to the session nor replaced by a fast account.
func TestCodexSpeedRankedPluginChoiceIsFinal(t *testing.T) {
	m, tr, clock, ids := speedSelectionManager(t, 24)
	slow := ids[23]
	m.SetPluginScheduler(&fakePluginScheduler{handled: true, resp: pluginapi.SchedulerPickResponse{Handled: true, AuthID: slow}})
	opts := sessionOpts("speed-plugin-session")
	key, _ := m.poolSessionKey("pool-model", opts)
	for i := 0; i < 2; i++ {
		a, _, err := m.pickCodexPool(context.Background(), "pool-model", sessionOpts("speed-plugin-session"), nil)
		if err != nil || a.ID != slow {
			t.Fatalf("pick %d: auth=%v err=%v; want the plugin choice %s", i, a, err, slow)
		}
	}
	m.SetPluginScheduler(&fakePluginScheduler{})
	a, _, err := m.pickCodexPool(context.Background(), "pool-model", sessionOpts("speed-plugin-session"), nil)
	if err != nil || !tr.inFast(a.ID, clock.Now()) {
		t.Fatalf("built-in pick=%v err=%v; want a fast account", a, err)
	}
	if tr.sessions.rebound(key, clock.Now()) {
		t.Fatal("a plugin choice must not seed a binding for the speed strategy to replace")
	}
}

// With a plugin scheduler active, a slow binding is still handed to the plugin first; the speed
// replacement only runs when the plugin leaves the choice to the built-in path.
func TestCodexSpeedBoundSessionAsksPluginBeforeReplacing(t *testing.T) {
	for _, handled := range []bool{true, false} {
		name := "declined"
		if handled {
			name = "handled"
		}
		t.Run(name, func(t *testing.T) {
			m, tr, clock, ids := speedSelectionManager(t, 24)
			opts := sessionOpts("speed-plugin-bound")
			key, affinity := m.poolSessionKey("pool-model", opts)
			slow, _ := m.GetByID(ids[23])
			if _, _, err := m.bindPoolSelection(context.Background(), key, "pool-model", opts, affinity, slow, nil, nil); err != nil {
				t.Fatal(err)
			}
			m.SetPluginScheduler(poolOutcomeSchedulerFunc(func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
				if handled {
					return pluginapi.SchedulerPickResponse{Handled: true, AuthID: slow.ID}, true, nil
				}
				return pluginapi.SchedulerPickResponse{}, false, nil
			}))
			a, _, err := m.pickCodexPool(context.Background(), "pool-model", sessionOpts("speed-plugin-bound"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if handled && (a.ID != slow.ID || tr.sessions.rebound(key, clock.Now())) {
				t.Fatalf("auth=%s; the plugin choice %s must not be replaced", a.ID, slow.ID)
			}
			if !handled && (!tr.inFast(a.ID, clock.Now()) || !tr.sessions.rebound(key, clock.Now())) {
				t.Fatalf("auth=%s; a declined plugin must leave the slow binding to the speed replacement", a.ID)
			}
		})
	}
}

// A plugin failure during a ranked pick is returned as is instead of becoming a failover signal.
func TestCodexSpeedRankedPluginErrorIsReturned(t *testing.T) {
	m, _, _, _ := speedSelectionManager(t, 24)
	expected := errors.New("plugin selection failed")
	calls := 0
	m.SetPluginScheduler(poolOutcomeSchedulerFunc(func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
		calls++
		return pluginapi.SchedulerPickResponse{}, true, expected
	}))
	a, _, err := m.pickCodexPool(context.Background(), "pool-model", sessionOpts("speed-plugin-error"), nil)
	if err != expected || a != nil || calls != 1 {
		t.Fatalf("auth=%v error=%v calls=%d; want the original plugin error after one call", a, err, calls)
	}
}
