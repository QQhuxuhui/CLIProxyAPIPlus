package helps

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type adaptiveFixture struct {
	pool *CodexFastSessionPool
	cfg  config.CodexFastSessionIDsConfig
	now  time.Time
}

func newAdaptiveFixture(t *testing.T, percent int) *adaptiveFixture {
	t.Helper()
	f := &adaptiveFixture{pool: NewCodexFastSessionPool(), now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	f.pool.nowFunc = func() time.Time { return f.now }
	f.cfg = config.CodexFastSessionIDsConfig{Enable: true, Percent: percent, Seed: []string{"fast"}}
	f.cfg.Rollback.Enable = true
	f.cfg.Rollback.WindowMinutes = 30
	f.cfg.Rollback.MinSamples = 40
	f.cfg.Rollback.FailureWorsePoints = 10
	f.cfg.Normalize()
	f.pool.ApplyConfig(f.cfg)
	return f
}

func (f *adaptiveFixture) worse() {
	observeN(f.pool, CodexFastSessionGroupFast, 40, 20, time.Second)
	observeN(f.pool, CodexFastSessionGroupControl, 40, 0, time.Second)
}

func (f *adaptiveFixture) evaluate() int {
	f.pool.EvaluateRollback(f.cfg.Rollback)
	return f.pool.Snapshot(10).EffectivePercent
}

func (f *adaptiveFixture) advance(d time.Duration) { f.now = f.now.Add(d) }

func TestAdaptiveShareHalvesToFloorAndNeverDisables(t *testing.T) {
	f := newAdaptiveFixture(t, 97)
	for _, want := range []int{48, 24, 12, 6, 5, 5} {
		f.worse()
		if got := f.evaluate(); got != want {
			t.Fatalf("share = %d, want %d", got, want)
		}
		if snap := f.pool.Snapshot(10); snap.Rollback.Disabled {
			t.Fatalf("pool reported disabled: %+v", snap.Rollback)
		}
		f.advance(5 * time.Minute)
	}
}

func TestAdaptiveShareIgnoresSlowFirstToken(t *testing.T) {
	f := newAdaptiveFixture(t, 97)
	observeN(f.pool, CodexFastSessionGroupFast, 40, 0, 20*time.Second)
	observeN(f.pool, CodexFastSessionGroupControl, 40, 0, time.Second)
	if got := f.evaluate(); got != 97 {
		t.Fatalf("share = %d after slow first tokens", got)
	}
}

func TestAdaptiveShareNeedsSamplesToDecrease(t *testing.T) {
	f := newAdaptiveFixture(t, 97)
	observeN(f.pool, CodexFastSessionGroupFast, 39, 39, time.Second)
	observeN(f.pool, CodexFastSessionGroupControl, 40, 0, time.Second)
	if got := f.evaluate(); got != 97 {
		t.Fatalf("share = %d with too few fast samples", got)
	}
	if outcome := f.pool.Snapshot(10).RollbackCheck.Outcome; outcome != "insufficient samples" {
		t.Fatalf("outcome = %q", outcome)
	}
}

func TestAdaptiveShareMarginIsExclusive(t *testing.T) {
	f := newAdaptiveFixture(t, 97)
	observeN(f.pool, CodexFastSessionGroupFast, 40, 4, time.Second) // exactly 10 points
	observeN(f.pool, CodexFastSessionGroupControl, 40, 0, time.Second)
	if got := f.evaluate(); got != 97 {
		t.Fatalf("share = %d at the margin", got)
	}
}

func TestAdaptiveShareRecoversOnlyAfterInterval(t *testing.T) {
	f := newAdaptiveFixture(t, 97)
	f.worse()
	if got := f.evaluate(); got != 48 {
		t.Fatalf("share = %d", got)
	}
	f.advance(9*time.Minute + 59*time.Second)
	if got := f.evaluate(); got != 48 {
		t.Fatalf("share = %d before the interval", got)
	}
	f.advance(time.Second)
	if got := f.evaluate(); got != 53 {
		t.Fatalf("share = %d after the interval", got)
	}
	if got := f.evaluate(); got != 53 {
		t.Fatalf("share = %d, want one step per interval", got)
	}
}

func TestAdaptiveShareRecoversWithoutAnySamples(t *testing.T) {
	f := newAdaptiveFixture(t, 97)
	f.worse()
	f.evaluate()
	for i := 0; i < 20; i++ {
		f.advance(10 * time.Minute)
		f.evaluate()
	}
	if got := f.pool.Snapshot(10).EffectivePercent; got != 97 {
		t.Fatalf("share = %d, want the configured cap", got)
	}
	f.advance(10 * time.Minute)
	if got := f.evaluate(); got != 97 {
		t.Fatalf("share = %d went past the cap", got)
	}
}

func TestAdaptiveShareHalvesAgainAfterPartialRecovery(t *testing.T) {
	f := newAdaptiveFixture(t, 97)
	f.worse()
	f.evaluate()
	f.advance(10 * time.Minute)
	f.evaluate()
	f.advance(5 * time.Minute)
	f.worse()
	if got := f.evaluate(); got != 26 {
		t.Fatalf("share = %d, want half of 53", got)
	}
}

func TestAdaptiveShareFloorRestartsRecoveryClock(t *testing.T) {
	f := newAdaptiveFixture(t, 10)
	f.worse()
	f.evaluate() // 5
	f.advance(9 * time.Minute)
	f.worse()
	if got := f.evaluate(); got != 5 {
		t.Fatalf("share = %d", got)
	}
	f.advance(9 * time.Minute)
	if got := f.evaluate(); got != 5 {
		t.Fatalf("share = %d, failure evidence must restart the clock", got)
	}
	f.advance(time.Minute)
	if got := f.evaluate(); got != 10 {
		t.Fatalf("share = %d after the restarted interval", got)
	}
}

func TestAdaptiveShareCooldownBlocksAdjustment(t *testing.T) {
	f := newAdaptiveFixture(t, 97)
	f.worse()
	f.evaluate()
	f.advance(4 * time.Minute)
	f.worse()
	if got := f.evaluate(); got != 48 {
		t.Fatalf("share = %d inside the cooldown", got)
	}
	if outcome := f.pool.Snapshot(10).RollbackCheck.Outcome; outcome != "cooling" {
		t.Fatalf("outcome = %q", outcome)
	}
}

func TestAdaptiveShareRaisingMinPercentNeverIncreasesOnDecrease(t *testing.T) {
	f := newAdaptiveFixture(t, 97)
	for i := 0; i < 5; i++ {
		f.worse()
		f.evaluate()
		f.advance(5 * time.Minute)
	}
	f.cfg.Rollback.MinPercent = 20
	f.worse()
	if got := f.evaluate(); got != 5 {
		t.Fatalf("share = %d, a decrease must not raise the share", got)
	}
}

func TestAdaptiveShareBelowMinPercentStays(t *testing.T) {
	f := newAdaptiveFixture(t, 3)
	f.worse()
	if got := f.evaluate(); got != 3 {
		t.Fatalf("share = %d", got)
	}
}

func TestAdaptiveShareInflightResultsCountForTheirGroup(t *testing.T) {
	f := newAdaptiveFixture(t, 97)
	_, group, epoch := f.pool.selectTracked(adaptiveFastID(t, f.pool))
	if group != CodexFastSessionGroupFast {
		t.Fatalf("group = %q", group)
	}
	f.worse()
	f.evaluate()
	f.pool.observeGeneration(group, true, 0, 100, epoch)
	stats := f.pool.Snapshot(10).Groups[CodexFastSessionGroupFast]
	if stats.WindowRequests != 41 || stats.WindowFailures != 21 {
		t.Fatalf("in-flight result lost from the reported window: %+v", stats)
	}
	requests, failures := f.pool.decisionWindowLocked(CodexFastSessionGroupFast, 30, f.now)
	if requests != 1 || failures != 1 {
		t.Fatalf("decision window = %d/%d", requests, failures)
	}
}

func TestAdaptiveShareClearingDecisionKeepsReportedWindow(t *testing.T) {
	f := newAdaptiveFixture(t, 97)
	f.worse()
	f.evaluate()
	snap := f.pool.Snapshot(10)
	if snap.Groups[CodexFastSessionGroupFast].WindowRequests != 40 || snap.Groups[CodexFastSessionGroupControl].WindowRequests != 40 {
		t.Fatalf("reported window changed: %+v", snap.Groups)
	}
	if requests, _ := f.pool.decisionWindowLocked(CodexFastSessionGroupFast, 30, f.now); requests != 0 {
		t.Fatalf("decision window = %d", requests)
	}
}

func TestAdaptiveShareConcurrentObserveIsNeverLost(t *testing.T) {
	f := newAdaptiveFixture(t, 97)
	var wg sync.WaitGroup
	const total = 20000
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < total; i++ {
			f.pool.Observe(CodexFastSessionGroupFast, false, time.Second, 100)
		}
	}()
	for i := 0; i < 200; i++ {
		f.pool.EvaluateRollback(f.cfg.Rollback)
	}
	wg.Wait()
	if got := f.pool.Snapshot(10).Groups[CodexFastSessionGroupFast].TotalRequests; got != total {
		t.Fatalf("total = %d", got)
	}
}

func TestAdaptiveShareOnlyMovesBoundaryIdentities(t *testing.T) {
	f := newAdaptiveFixture(t, 97)
	ids := codexFastSessionTestIDs(5000)
	type state struct{ group string }
	before := make(map[string]state, len(ids))
	for _, id := range ids {
		_, group := f.pool.Select(id)
		before[id] = state{group}
	}
	f.worse()
	f.evaluate()
	fast := 0
	for _, id := range ids {
		_, group := f.pool.Select(id)
		switch before[id].group {
		case CodexFastSessionGroupControl:
			if group != CodexFastSessionGroupControl {
				t.Fatalf("control identity %s moved to %s", id, group)
			}
		case CodexFastSessionGroupFast:
			if group == CodexFastSessionGroupControl {
				t.Fatalf("fast identity %s became control", id)
			}
			if group == CodexFastSessionGroupFast {
				fast++
			}
		}
		if before[id].group == CodexFastSessionGroupOff {
			t.Fatalf("identity %s was off at the cap", id)
		}
	}
	if fast == 0 || fast >= len(ids) {
		t.Fatalf("fast identities after halving = %d", fast)
	}
	// Recovering keeps previously fast identities fast.
	for _, id := range ids {
		if _, group := f.pool.Select(id); group == CodexFastSessionGroupOff {
			if got := before[id].group; got != CodexFastSessionGroupFast {
				t.Fatalf("off identity %s was %s", id, got)
			}
		}
	}
}

func TestAdaptiveShareSameConfigKeepsShareAndChangeResets(t *testing.T) {
	f := newAdaptiveFixture(t, 97)
	f.worse()
	f.evaluate()
	f.pool.ApplyConfig(f.cfg)
	if got := f.pool.Snapshot(10).EffectivePercent; got != 48 {
		t.Fatalf("same config reset the share: %d", got)
	}
	f.cfg.Percent = 90
	f.pool.ApplyConfig(f.cfg)
	if got := f.pool.Snapshot(10).EffectivePercent; got != 90 {
		t.Fatalf("share = %d after a config change", got)
	}
}

func TestAdaptiveShareDisabledRollbackUsesConfiguredPercent(t *testing.T) {
	f := newAdaptiveFixture(t, 100)
	f.cfg.Rollback.Enable = false
	f.pool.ApplyConfig(f.cfg)
	f.worse()
	if got := f.evaluate(); got != 100 {
		t.Fatalf("share = %d", got)
	}
	if outcome := f.pool.Snapshot(10).RollbackCheck.Outcome; outcome != "rollback disabled" {
		t.Fatalf("outcome = %q", outcome)
	}
}

func TestAdaptiveShareManualControlsWin(t *testing.T) {
	f := newAdaptiveFixture(t, 0)
	if _, group := f.pool.Select("conv"); group != CodexFastSessionGroupControl {
		t.Fatalf("percent 0 group = %q", group)
	}
	f = newAdaptiveFixture(t, 97)
	f.cfg.Enable = false
	f.pool.ApplyConfig(f.cfg)
	if _, group := f.pool.Select("conv"); group != CodexFastSessionGroupOff {
		t.Fatalf("disabled group = %q", group)
	}
}

func TestAdaptiveShareEmptyPoolIsNotEvaluated(t *testing.T) {
	f := newAdaptiveFixture(t, 97)
	f.pool.Remove("fast")
	f.worse()
	if got := f.evaluate(); got != 97 {
		t.Fatalf("share = %d", got)
	}
	if outcome := f.pool.Snapshot(10).RollbackCheck.Outcome; outcome != "mapping not active" {
		t.Fatalf("outcome = %q", outcome)
	}
}

func TestAdaptiveShareRecordsEvents(t *testing.T) {
	f := newAdaptiveFixture(t, 97)
	f.worse()
	f.evaluate()
	f.advance(10 * time.Minute)
	f.evaluate()
	events := f.pool.Snapshot(10).Events
	if len(events) < 2 || events[len(events)-2].Action != "decrease" || events[len(events)-1].Action != "increase" {
		t.Fatalf("events = %+v", events)
	}
}

func adaptiveFastID(t *testing.T, pool *CodexFastSessionPool) string {
	t.Helper()
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("conversation-%d", i)
		if _, group := pool.Select(id); group == CodexFastSessionGroupFast {
			return id
		}
	}
	t.Fatal("no fast identity")
	return ""
}
