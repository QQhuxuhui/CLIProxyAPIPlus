package auth

import (
	"fmt"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCodexSpeedSamplesSurviveRestart(t *testing.T) {
	old, clock := newTestSpeedTracker(t, nil)
	ids := rankPool(old, "acct", 40)
	snap := old.ExportSamples()
	if len(snap.Accounts) != 40 {
		t.Fatalf("exported %d accounts, want 40", len(snap.Accounts))
	}

	clock.Advance(2 * time.Minute)
	fresh := NewCodexSpeedTracker(clock.Now)
	fresh.SetConfig(internalconfig.CodexSpeedPriorityConfig{Enable: true}, true, true)
	if got := fresh.ImportSamples(snap); got != 40 {
		t.Fatalf("restored %d accounts, want 40", got)
	}
	fresh.Recompute(infosOf(ids...))
	view := fresh.view.Load()
	if !fresh.Ready() || view.scored != 40 || !view.fast[ids[0]] || view.fast[ids[39]] {
		t.Fatalf("after restart: ready=%v scored=%d", fresh.Ready(), view.scored)
	}
	if view.score[ids[4]] != 5*time.Second {
		t.Fatalf("score = %v, want 5s", view.score[ids[4]])
	}
	if again := fresh.ImportSamples(snap); again != 0 {
		t.Fatalf("second import restored %d accounts, want 0", again)
	}
}

func TestCodexSpeedRestoredSamplesStillExpire(t *testing.T) {
	old, clock := newTestSpeedTracker(t, nil)
	ids := rankPool(old, "acct", 40)
	snap := old.ExportSamples()

	// Restored samples keep their timestamps, so they still leave the sample window.
	clock.Advance(sampleWindow(old.config(), 0) + time.Minute)
	fresh := NewCodexSpeedTracker(clock.Now)
	fresh.SetConfig(old.config(), true, true)
	fresh.ImportSamples(snap)
	fresh.Recompute(infosOf(ids...))
	if scored := fresh.view.Load().scored; scored != 0 {
		t.Fatalf("expired samples scored %d accounts", scored)
	}

	clock.Advance(codexSpeedEntryTTL)
	stale := NewCodexSpeedTracker(clock.Now)
	if got := stale.ImportSamples(snap); got != 0 {
		t.Fatalf("restored %d accounts from day-old samples, want 0", got)
	}
}

func TestCodexSpeedRestoredProbeSamplesUseSavedCycle(t *testing.T) {
	old, clock := newTestSpeedTracker(t, nil)
	ids := make([]string, 30)
	for i := range ids {
		ids[i] = fmt.Sprintf("probe-%03d", i)
	}
	// Three probe rounds four hours apart, as when slow probes keep rounds far over the target.
	for round := 0; round < 3; round++ {
		for i, id := range ids {
			old.ObserveProbe(CodexSpeedProbeResult{AuthID: id, Success: true, Gap: time.Duration(i+1) * time.Second})
		}
		clock.Advance(4 * time.Hour)
	}
	snap := old.ExportSamples()
	if len(snap.Accounts) != 0 {
		t.Fatalf("without a known cycle the old probes must not export, got %d", len(snap.Accounts))
	}

	seeded := emptyCodexSpeedView()
	seeded.probeCycle = 4 * time.Hour
	old.view.Store(seeded)
	snap = old.ExportSamples()
	if len(snap.Accounts) != 30 || snap.ProbeCycle != 4*time.Hour {
		t.Fatalf("exported %d accounts, cycle %v", len(snap.Accounts), snap.ProbeCycle)
	}

	fresh := NewCodexSpeedTracker(clock.Now)
	fresh.SetConfig(old.config(), true, true)
	fresh.ImportSamples(snap)
	fresh.Recompute(infosOf(ids...))
	if scored := fresh.view.Load().scored; scored != 30 {
		t.Fatalf("first recompute after restart scored %d accounts, want 30", scored)
	}
	if last := fresh.LastProbe(ids[0]); !last.IsZero() {
		t.Fatalf("unreserved probes must not invent a last probe time, got %v", last)
	}
}

// A high sample floor keeps samples valid for longer than a day; a restart must not cut them.
func TestCodexSpeedRestoreKeepsSamplesOlderThanADay(t *testing.T) {
	old, clock := newTestSpeedTracker(t, func(c *internalconfig.CodexSpeedPriorityConfig) { c.ScoreMinSamples = 32 })
	round := old.config().Probe.Round()
	for probe := 0; probe < 32; probe++ {
		if probe > 0 {
			clock.Advance(round)
		}
		old.ObserveProbe(CodexSpeedProbeResult{AuthID: "patient", Success: true, Gap: time.Second})
	}
	if age := 31 * round; age <= codexSpeedEntryTTL {
		t.Fatalf("setup: the oldest sample must be over a day old, is %v", age)
	}
	fresh := NewCodexSpeedTracker(clock.Now)
	fresh.SetConfig(old.config(), true, true)
	if got := fresh.ImportSamples(old.ExportSamples()); got != 1 {
		t.Fatalf("restored %d accounts, want 1", got)
	}
	fresh.Recompute(infosOf("patient"))
	if _, ok := fresh.view.Load().score["patient"]; !ok {
		t.Fatal("all 32 samples must survive the restart")
	}
}

func TestCodexSpeedRestoreRemembersRealTraffic(t *testing.T) {
	old, clock := newTestSpeedTracker(t, func(c *internalconfig.CodexSpeedPriorityConfig) { c.ScoreMinSamples = 1 })
	feed(old, "busy", 1, time.Second)
	clock.Advance(10 * time.Minute)
	fresh := NewCodexSpeedTracker(clock.Now)
	fresh.SetConfig(old.config(), true, true)
	fresh.ImportSamples(old.ExportSamples())
	if fresh.ReserveProbe("busy") {
		t.Fatal("an account measured by real traffic before the restart must not be probed in the same round")
	}
}
