package auth

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestCodexSpeedRestoredGenerationLabelsOpenHour(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, nil)
	_, hash, start := tr.ab.generation()
	data, err := tr.MarshalState()
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Hour)
	restarted := NewCodexSpeedTracker(clock.Now)
	restarted.SetConfig(internalconfig.CodexSpeedPriorityConfig{Enable: true}, true, true)
	if err = restarted.RestoreState(data); err != nil {
		t.Fatal(err)
	}
	gen, _, _ := restarted.ab.generation()
	restarted.ab.observeRequest(CodexSpeedGroupControl, gen, false, 0, clock.now)
	clock.Advance(time.Hour)
	rolled := restarted.RollHours(nil)
	want := codexSpeedGenerationLabel(hash, start)
	if len(rolled) != 1 || rolled[0].Generation != want || rolled[0].Control.Requests != 1 {
		t.Fatalf("restored generation must label the open hour: %+v want %s", rolled, want)
	}
	if live, _ := restarted.generationLabel(); live != want {
		t.Fatalf("live generation %s != %s", live, want)
	}
}

func TestCodexSpeedConfigChangeSealsHoursUnderOldGeneration(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, nil)
	oldLabel, _ := tr.generationLabel()
	gen, _, _ := tr.ab.generation()
	tr.ab.observeRequest(CodexSpeedGroupControl, gen, false, 0, clock.now)
	// Same hour: the config changes, then the new generation records a request.
	tr.SetConfig(internalconfig.CodexSpeedPriorityConfig{Enable: true, Percent: 30}, true, true)
	newLabel, _ := tr.generationLabel()
	gen2, _, _ := tr.ab.generation()
	tr.ab.observeRequest(CodexSpeedGroupControl, gen2, false, 0, clock.now)
	clock.Advance(time.Hour)
	rolled := tr.RollHours(nil)
	if len(rolled) != 2 || rolled[0].Generation != oldLabel || rolled[1].Generation != newLabel || rolled[0].Control.Requests != 1 || rolled[1].Control.Requests != 1 {
		t.Fatalf("both segments of the hour must be kept with their generations: %+v", rolled)
	}
}

func TestCodexSpeedConfigChangeAfterHourKeepsFinishedHour(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, nil)
	oldLabel, _ := tr.generationLabel()
	gen, _, _ := tr.ab.generation()
	tr.ab.observeRequest(CodexSpeedGroupControl, gen, false, 0, clock.now)
	clock.Advance(time.Hour) // no request after the hour; the config changes before the tick
	tr.SetConfig(internalconfig.CodexSpeedPriorityConfig{Enable: true, Percent: 30}, true, true)
	rolled := tr.RollHours(nil)
	if len(rolled) != 1 || rolled[0].Generation != oldLabel || rolled[0].Control.Requests != 1 {
		t.Fatalf("the finished hour must survive the config change: %+v", rolled)
	}
}

func TestCodexSpeedMidnightProbeOnlyHourKeepsYesterday(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, nil)
	// 23:30 Beijing time on 2026-11-03.
	clock.now = time.Date(2026, 11, 3, 15, 30, 0, 0, time.UTC)
	yesterday := codexSpeedDay(clock.now)
	slowPool(tr, 25)
	feed(tr, "acct", 3, time.Second)
	tr.Recompute(nil)
	tr.RollHours(nil) // settle earlier hours
	clock.Advance(20 * time.Minute)
	slowPool(tr, 25)
	feed(tr, "acct", 6, 9*time.Second)
	tr.Recompute(nil) // fast -> slow at 23:50, probe-only hour (no A/B requests)
	tr.ObserveProbe(CodexSpeedProbeResult{AuthID: "acct", Success: true, Gap: 9 * time.Second})
	clock.Advance(15 * time.Minute) // 00:05: page read and config change before the tick
	tr.Snapshot()
	tr.Recompute(nil)
	tr.SetConfig(internalconfig.CodexSpeedPriorityConfig{Enable: true, Percent: 40}, true, true)
	rolled := tr.RollHours(nil)
	if len(rolled) == 0 {
		t.Fatal("yesterday's last hour must be sealed")
	}
	last := rolled[len(rolled)-1]
	if last.Boost.Day != yesterday || last.Boost.FastToSlow+last.Hold.FastToSlow != 1 || last.Probes != 1 {
		t.Fatalf("yesterday's hour = %+v", last)
	}
}

func TestCodexSpeedFirstRecomputeAfterMidnightCountsTransition(t *testing.T) {
	for _, tc := range []struct{ from, to time.Duration }{{time.Second, 9 * time.Second}, {9 * time.Second, time.Second}} {
		tr, clock := newTestSpeedTracker(t, nil)
		clock.now = time.Date(2026, 11, 3, 15, 50, 0, 0, time.UTC) // 23:50 Beijing
		slowPool(tr, 25)
		feed(tr, "acct", 3, tc.from)
		tr.Recompute(nil)
		clock.Advance(15 * time.Minute) // 00:05, first recompute of the day flips the tier
		slowPool(tr, 25)
		feed(tr, "acct", 9, tc.to)
		tr.Recompute(nil)
		boost, hold := tr.cohort.summaries(codexSpeedDay(clock.now))
		if tc.to == 9*time.Second {
			if boost.FastSet+hold.FastSet != 1 || boost.FastToSlow+hold.FastToSlow != 1 {
				t.Fatalf("fast->slow after midnight: %+v %+v", boost, hold)
			}
		} else if boost.SlowSet+hold.SlowSet < 1 || boost.SlowToFast+hold.SlowToFast != 1 {
			t.Fatalf("slow->fast after midnight: %+v %+v", boost, hold)
		}
	}
}

func TestCodexSpeedChildInheritsTreatmentOriginAcrossTurns(t *testing.T) {
	fast := boostID("child-fast")
	m, tr, _ := speedSelectionManager(t, fast)
	slowPool(tr, 25)
	feed(tr, fast, 3, time.Second)
	tr.Recompute(nil)
	ctx, _ := speedRequest(tr, CodexSpeedGroupTreatment)
	if _, _, err := m.pickCodexPool(ctx, "pool-model", sessionOpts("parent-session"), nil); err != nil {
		t.Fatal(err)
	}
	childOpts := func() (context.Context, *codexSpeedRequestState) {
		return speedRequest(tr, CodexSpeedGroupControl)
	}
	for turn := 1; turn <= 2; turn++ {
		ctx, state := childOpts()
		opts := sessionOpts("child-session")
		opts.OriginalRequest = []byte(`{"parent_session_id":"parent-session"}`)
		if _, _, err := m.pickCodexPool(ctx, "pool-model", opts, nil); err != nil {
			t.Fatal(err)
		}
		if group, _, _, _ := state.get(); group != CodexSpeedGroupExcluded {
			t.Fatalf("turn %d: control child on an inherited treatment binding must be excluded, got %s", turn, group)
		}
	}
}

func TestCodexSpeedAuthInfosRequireProbeModel(t *testing.T) {
	m := newPoolStorageManager(t, 1)
	registerPoolModel(t, m, "supports")
	if _, err := m.Register(context.Background(), &Auth{ID: "no-model", Provider: "codex", Metadata: map[string]any{"access_token": "x"}}); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient("no-model", "codex", []*registry.ModelInfo{{ID: "other-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("no-model") })
	for _, info := range m.CodexSpeedAuthInfos("pool-model") {
		if info.ID == "no-model" && info.Schedulable {
			t.Fatal("an account without the probe model must not be schedulable for probes")
		}
	}
}
