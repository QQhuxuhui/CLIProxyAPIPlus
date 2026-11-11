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

	// Passive samples are valid for the 30-minute score window only.
	clock.Advance(31 * time.Minute)
	fresh := NewCodexSpeedTracker(clock.Now)
	fresh.SetConfig(internalconfig.CodexSpeedPriorityConfig{Enable: true}, true, true)
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
	// Three probe rounds two hours apart, as on a pool too large for the probe interval.
	for round := 0; round < 3; round++ {
		for i, id := range ids {
			old.ObserveProbe(CodexSpeedProbeResult{AuthID: id, Success: true, Gap: time.Duration(i+1) * time.Second})
		}
		clock.Advance(2 * time.Hour)
	}
	snap := old.ExportSamples()
	snap.ProbeCycle = 2 * time.Hour
	if len(snap.Accounts) != 0 {
		t.Fatalf("without a known cycle the old probes must not export, got %d", len(snap.Accounts))
	}

	seeded := emptyCodexSpeedView()
	seeded.probeCycle = 2 * time.Hour
	old.view.Store(seeded)
	snap = old.ExportSamples()
	if len(snap.Accounts) != 30 || snap.ProbeCycle != 2*time.Hour {
		t.Fatalf("exported %d accounts, cycle %v", len(snap.Accounts), snap.ProbeCycle)
	}

	fresh := NewCodexSpeedTracker(clock.Now)
	fresh.SetConfig(internalconfig.CodexSpeedPriorityConfig{Enable: true}, true, true)
	fresh.ImportSamples(snap)
	fresh.Recompute(infosOf(ids...))
	if scored := fresh.view.Load().scored; scored != 30 {
		t.Fatalf("first recompute after restart scored %d accounts, want 30", scored)
	}
	if last := fresh.LastProbe(ids[0]); !last.IsZero() {
		t.Fatalf("unreserved probes must not invent a last probe time, got %v", last)
	}
}
