package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// useSpeedTracker swaps the process-wide tracker for one test.
func useSpeedTracker(t *testing.T, tr *CodexSpeedTracker) {
	t.Helper()
	previous := defaultCodexSpeedTracker
	defaultCodexSpeedTracker = tr
	t.Cleanup(func() { defaultCodexSpeedTracker = previous })
}

func boostID(prefix string) string {
	for i := 0; ; i++ {
		id := fmt.Sprintf("%s-%d", prefix, i)
		if codexSpeedCohortOf(id) == CodexSpeedCohortBoost {
			return id
		}
	}
}

// speedRequest returns a context whose request state is fixed to group.
func speedRequest(tr *CodexSpeedTracker, group string) (context.Context, *codexSpeedRequestState) {
	state := &codexSpeedRequestState{}
	gen, _, _ := tr.ab.generation()
	state.once.Do(func() {})
	state.inited, state.group, state.gen = true, group, gen
	return context.WithValue(context.Background(), codexSpeedStateKey{}, state), state
}

func speedSelectionManager(t *testing.T, ids ...string) (*Manager, *CodexSpeedTracker, *speedClock) {
	t.Helper()
	m := newPoolStorageManager(t, 1)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	for _, id := range ids {
		registerPoolModel(t, m, id)
	}
	tr, clock := newTestSpeedTracker(t, func(c *internalconfig.CodexSpeedPriorityConfig) { c.Percent = 50 })
	useSpeedTracker(t, tr)
	return m, tr, clock
}

func sessionOpts(id string) exec.Options {
	opts := exec.Options{Headers: http.Header{"Session-Id": {id}}}
	opts.EnsureMetadata()
	return opts
}

func TestCodexSpeedTreatmentReplacesSlowBindingOnce(t *testing.T) {
	fast := boostID("speed-fast")
	m, tr, clock := speedSelectionManager(t, "speed-slow", fast)
	slowPool(tr, 25)
	feed(tr, "speed-slow", 3, 9*time.Second)
	feed(tr, fast, 3, time.Second)
	tr.Recompute(nil)

	opts := sessionOpts("speed-session")
	key, affinity := m.poolSessionKey("pool-model", opts)
	slow, _ := m.GetByID("speed-slow")
	if _, _, err := m.bindPoolSelection(context.Background(), key, "pool-model", opts, affinity, slow, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	ctx, _ := speedRequest(tr, CodexSpeedGroupTreatment)
	a, _, err := m.pickCodexPool(ctx, "pool-model", sessionOpts("speed-session"), nil)
	if err != nil || a.ID != fast {
		t.Fatalf("slow binding must be replaced by the fast account: %v %v", a, err)
	}
	if !tr.ab.rebound(key, clock.now) {
		t.Fatal("the session must be marked as rebound")
	}
	// Within the session lifetime the new account turns slow too; the session already used
	// its replacement.
	clock.Advance(30 * time.Minute)
	slowPool(tr, 25)
	feed(tr, fast, 6, 9*time.Second)
	feed(tr, "speed-slow", 6, time.Second)
	tr.Recompute(nil)
	if tr.TierOf(fast) != CodexSpeedTierSlow || tr.TierOf("speed-slow") != CodexSpeedTierFast {
		t.Fatal("setup: tiers must swap")
	}
	ctx, _ = speedRequest(tr, CodexSpeedGroupTreatment)
	a, _, err = m.pickCodexPool(ctx, "pool-model", sessionOpts("speed-session"), nil)
	if err != nil || a.ID != fast {
		t.Fatalf("second replacement must not happen: %v %v", a, err)
	}
}

func TestCodexSpeedReplacementFallsBackWithoutAlternative(t *testing.T) {
	m, tr, clock := speedSelectionManager(t, "only-slow")
	slowPool(tr, 25)
	feed(tr, "only-slow", 3, 9*time.Second)
	tr.Recompute(nil)
	opts := sessionOpts("lonely")
	key, affinity := m.poolSessionKey("pool-model", opts)
	slow, _ := m.GetByID("only-slow")
	if _, _, err := m.bindPoolSelection(context.Background(), key, "pool-model", opts, affinity, slow, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	ctx, _ := speedRequest(tr, CodexSpeedGroupTreatment)
	a, _, err := m.pickCodexPool(ctx, "pool-model", sessionOpts("lonely"), nil)
	if err != nil || a.ID != "only-slow" {
		t.Fatalf("without an alternative the slow binding stays: %v %v", a, err)
	}
	if tr.ab.rebound(key, clock.now) {
		t.Fatal("a failed replacement must not spend the session's chance")
	}
}

func TestCodexSpeedControlReusingTreatmentBindingIsExcluded(t *testing.T) {
	fast := boostID("excl-fast")
	m, tr, _ := speedSelectionManager(t, fast, "excl-other")
	slowPool(tr, 25)
	feed(tr, fast, 3, time.Second)
	tr.Recompute(nil)
	ctx, _ := speedRequest(tr, CodexSpeedGroupTreatment)
	first, _, err := m.pickCodexPool(ctx, "pool-model", sessionOpts("shared"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, state := speedRequest(tr, CodexSpeedGroupControl)
	again, _, err := m.pickCodexPool(ctx, "pool-model", sessionOpts("shared"), nil)
	if err != nil || again.ID != first.ID {
		t.Fatalf("control request must reuse the binding: %v %v", again, err)
	}
	if group, _, _, _ := state.get(); group != CodexSpeedGroupExcluded {
		t.Fatalf("control on a treatment binding must be excluded, got %s", group)
	}
}

func TestCodexSpeedTreatmentWeightsFreshPicks(t *testing.T) {
	fast := boostID("weight-fast")
	m, tr, _ := speedSelectionManager(t, "weight-slow", fast)
	slowPool(tr, 25)
	feed(tr, "weight-slow", 3, 9*time.Second)
	feed(tr, fast, 3, time.Second)
	tr.Recompute(nil)
	counts := map[string]int{}
	for i := 0; i < 11; i++ {
		ctx, _ := speedRequest(tr, CodexSpeedGroupTreatment)
		a, _, err := m.pickCodexPool(ctx, "pool-model", exec.Options{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		counts[a.ID]++
	}
	if counts[fast] != 10 || counts["weight-slow"] != 1 {
		t.Fatalf("treatment picks follow 10/1 weights: %v", counts)
	}
}

func TestCodexSpeedPreviewNeitherGroupsNorWeights(t *testing.T) {
	m, tr, _ := speedSelectionManager(t, "preview-a")
	ctx := context.WithValue(context.Background(), poolPreviewKey{}, true)
	state := &codexSpeedRequestState{}
	ctx = context.WithValue(ctx, codexSpeedStateKey{}, state)
	if _, _, err := m.pickCodexPool(ctx, "pool-model", sessionOpts("p"), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, _, inited := state.get(); inited {
		t.Fatal("a preview pick must not fix the request group")
	}
	_ = tr
}
