package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexspeed"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type speedClock struct{ now time.Time }

func (c *speedClock) Now() time.Time          { return c.now }
func (c *speedClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestSpeedTracker(t *testing.T, mutate func(*internalconfig.CodexSpeedPriorityConfig)) (*CodexSpeedTracker, *speedClock) {
	t.Helper()
	clock := &speedClock{now: time.Date(2026, 11, 3, 6, 10, 0, 0, time.UTC)}
	tr := NewCodexSpeedTracker(clock.Now)
	cfg := internalconfig.CodexSpeedPriorityConfig{Enable: true}
	if mutate != nil {
		mutate(&cfg)
	}
	tr.SetConfig(cfg, true, true)
	return tr, clock
}

func feed(tr *CodexSpeedTracker, id string, n int, gap time.Duration) {
	for i := 0; i < n; i++ {
		tr.ObserveAttempt(CodexSpeedAttempt{AuthID: id, Success: true, Gap: gap, InputTokens: 100})
	}
}

// slowPool adds count accounts with a 9s median so the pool baseline enables classification.
func slowPool(tr *CodexSpeedTracker, count int) {
	for i := 0; i < count; i++ {
		feed(tr, fmt.Sprintf("slow-%d", i), 3, 9*time.Second)
	}
}

func TestCodexSpeedClassifiesFastSlowAndUnknown(t *testing.T) {
	tr, _ := newTestSpeedTracker(t, nil)
	slowPool(tr, 25)
	feed(tr, "fast", 3, time.Second)
	feed(tr, "few", 2, time.Second)
	tr.Recompute(nil)
	if !tr.Classifying() {
		t.Fatal("a 9s baseline must enable classification")
	}
	if got := tr.TierOf("fast"); got != CodexSpeedTierFast {
		t.Fatalf("fast tier = %s", got)
	}
	if got := tr.TierOf("slow-0"); got != CodexSpeedTierSlow {
		t.Fatalf("slow tier = %s", got)
	}
	if got := tr.TierOf("few"); got != CodexSpeedTierUnknown {
		t.Fatalf("too few samples must stay unknown, got %s", got)
	}
}

func TestCodexSpeedSampleFilters(t *testing.T) {
	tr, _ := newTestSpeedTracker(t, nil)
	slowPool(tr, 25)
	for i := 0; i < 3; i++ {
		tr.ObserveAttempt(CodexSpeedAttempt{AuthID: "img", Success: true, Gap: time.Second, InputTokens: 100, HasImage: true})
		tr.ObserveAttempt(CodexSpeedAttempt{AuthID: "think", Success: true, Gap: time.Second, InputTokens: 100, ReasoningTokens: 500})
		tr.ObserveAttempt(CodexSpeedAttempt{AuthID: "big", Success: true, Gap: time.Second, InputTokens: 50000})
		tr.ObserveAttempt(CodexSpeedAttempt{AuthID: "nogap", Success: true, InputTokens: 100})
	}
	tr.Recompute(nil)
	for _, id := range []string{"img", "think", "big", "nogap"} {
		if got := tr.TierOf(id); got != CodexSpeedTierUnknown {
			t.Fatalf("%s must not produce samples, tier %s", id, got)
		}
	}
}

func TestCodexSpeedOverloadMakesSlowAndProbeSuccessRecovers(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, nil)
	slowPool(tr, 25)
	feed(tr, "flaky", 3, time.Second)
	for i := 0; i < 3; i++ {
		tr.ObserveAttempt(CodexSpeedAttempt{AuthID: "flaky", Overload: true})
	}
	tr.Recompute(nil)
	if got := tr.TierOf("flaky"); got != CodexSpeedTierSlow {
		t.Fatalf("50%% overload must be slow, got %s", got)
	}
	// Successful probes later dilute the overload rate below 15%.
	clock.Advance(2 * time.Hour)
	slowPool(tr, 25)
	for i := 0; i < 30; i++ {
		tr.ObserveProbe(CodexSpeedProbeResult{AuthID: "flaky", Success: true, Gap: time.Second})
	}
	tr.Recompute(nil)
	if got := tr.TierOf("flaky"); got != CodexSpeedTierFast {
		t.Fatalf("probe successes must count in the overload denominator, got %s", got)
	}
}

func TestCodexSpeedSamplesExpireWithoutNewData(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, nil)
	slowPool(tr, 25)
	feed(tr, "fast", 3, time.Second)
	tr.Recompute(nil)
	if tr.TierOf("fast") != CodexSpeedTierFast {
		t.Fatal("setup")
	}
	clock.Advance(61 * time.Minute)
	slowPool(tr, 25) // keep the baseline alive
	tr.Recompute(nil)
	if got := tr.TierOf("fast"); got != CodexSpeedTierUnknown {
		t.Fatalf("expired passive samples must give unknown, got %s", got)
	}
}

func TestCodexSpeedProbeSamplesLastLonger(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, nil)
	for i := 0; i < 3; i++ {
		tr.ObserveProbe(CodexSpeedProbeResult{AuthID: "idle", Success: true, Gap: time.Second})
		clock.Advance(time.Hour)
	}
	slowPool(tr, 25)
	tr.Recompute(nil)
	if got := tr.TierOf("idle"); got != CodexSpeedTierFast {
		t.Fatalf("three hourly probes must classify an idle account, got %s", got)
	}
}

func TestCodexSpeedBaselineIgnoresTrafficSkew(t *testing.T) {
	tr, _ := newTestSpeedTracker(t, nil)
	for i := 0; i < 15; i++ {
		feed(tr, fmt.Sprintf("s%d", i), 1, 9*time.Second)
	}
	for i := 0; i < 10; i++ {
		feed(tr, fmt.Sprintf("f%d", i), 30, time.Second) // fast accounts carry most samples
	}
	tr.Recompute(nil)
	if v := tr.view.Load(); !v.hasBase || v.baselineMs != 9000 || !v.classify {
		t.Fatalf("baseline must weight accounts equally: %+v", v)
	}
}

func TestCodexSpeedOffPeakAndThinBaselineStopClassifying(t *testing.T) {
	tr, _ := newTestSpeedTracker(t, nil)
	for i := 0; i < 25; i++ {
		feed(tr, fmt.Sprintf("a%d", i), 3, time.Second)
	}
	tr.Recompute(nil)
	if tr.Classifying() || tr.TierOf("a0") != CodexSpeedTierUnknown {
		t.Fatal("a 1s baseline is off-peak: no tiers")
	}
	tr2, _ := newTestSpeedTracker(t, nil)
	slowPool(tr2, 10)
	tr2.Recompute(nil)
	if tr2.Classifying() {
		t.Fatal("fewer than 20 accounts give no baseline")
	}
}

func TestCodexSpeedGroupsCapAndHalving(t *testing.T) {
	tr, _ := newTestSpeedTracker(t, func(c *internalconfig.CodexSpeedPriorityConfig) { c.Percent = 100 })
	if capPct, eff := tr.ab.shares(); capPct != 99 || eff != 99 {
		t.Fatalf("rollback keeps a control cohort: cap=%d eff=%d", capPct, eff)
	}
	if g, _ := tr.ab.groupFor(99); g != CodexSpeedGroupControl {
		t.Fatalf("bucket 99 = %s", g)
	}
	tr2, _ := newTestSpeedTracker(t, func(c *internalconfig.CodexSpeedPriorityConfig) { c.Percent = 20 })
	tr2.ab.mu.Lock()
	tr2.ab.effective = 10
	tr2.ab.mu.Unlock()
	for bucket, want := range map[int]string{5: CodexSpeedGroupTreatment, 15: CodexSpeedGroupOff, 20: CodexSpeedGroupControl} {
		if g, _ := tr2.ab.groupFor(bucket); g != want {
			t.Fatalf("bucket %d = %s, want %s", bucket, g, want)
		}
	}
}

func TestCodexSpeedRollbackHalvesAndRecovers(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, func(c *internalconfig.CodexSpeedPriorityConfig) { c.Percent = 40 })
	gen, _, _ := tr.ab.generation()
	for i := 0; i < 50; i++ {
		tr.ab.observeRequest(CodexSpeedGroupTreatment, gen, i < 15, 0, clock.now)
		tr.ab.observeRequest(CodexSpeedGroupControl, gen, i < 2, 0, clock.now)
	}
	if ev := tr.Tick(); ev == "" {
		t.Fatal("treatment failing 26 points more must halve the share")
	}
	if _, eff := tr.ab.shares(); eff != 20 {
		t.Fatalf("effective = %d, want 20", eff)
	}
	clock.Advance(11 * time.Minute)
	if ev := tr.Tick(); ev == "" {
		t.Fatal("no new evidence: share climbs one step")
	}
	if _, eff := tr.ab.shares(); eff != 25 {
		t.Fatalf("effective = %d, want 25", eff)
	}
}

func TestCodexSpeedGenerationChangeAndRestore(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, nil)
	gen1, hash1, start1 := tr.ab.generation()
	tr.SetConfig(internalconfig.CodexSpeedPriorityConfig{Enable: true}, true, true)
	if gen, _, _ := tr.ab.generation(); gen != gen1 {
		t.Fatal("unchanged config keeps the generation")
	}
	clock.Advance(time.Minute)
	tr.SetConfig(internalconfig.CodexSpeedPriorityConfig{Enable: true, Weights: internalconfig.CodexSpeedWeights{Fast: 20}}, true, true)
	gen2, hash2, _ := tr.ab.generation()
	if gen2 == gen1 || hash2 == hash1 {
		t.Fatal("a weight change starts a new generation")
	}
	tr.ab.observeRequest(CodexSpeedGroupTreatment, gen1, false, 0, clock.now)
	if treat, _, _ := tr.ab.currentHour(); treat.Requests != 0 {
		t.Fatal("old-generation results must not enter the new window")
	}
	// Restart with the original config restores its start time.
	fresh, _ := newTestSpeedTracker(t, nil)
	fresh.ab.restoreGeneration(hash1, start1.Add(-time.Hour))
	if _, _, start := fresh.ab.generation(); !start.Equal(start1.Add(-time.Hour)) {
		t.Fatalf("restored start = %v", start)
	}
}

func TestCodexSpeedSmoothWeightedPickRespectsTiersAndCohorts(t *testing.T) {
	tr, _ := newTestSpeedTracker(t, nil)
	slowPool(tr, 25)
	var boost, hold string
	for i := 0; boost == "" || hold == ""; i++ {
		key := fmt.Sprintf("acct-%d", i)
		if codexSpeedCohortOf(key) == CodexSpeedCohortBoost && boost == "" {
			boost = key
		} else if codexSpeedCohortOf(key) == CodexSpeedCohortHold && hold == "" {
			hold = key
		}
	}
	feed(tr, boost, 3, time.Second)
	feed(tr, hold, 3, time.Second)
	tr.Recompute(nil)
	auths := []*Auth{{ID: boost}, {ID: hold}, {ID: "slow-0"}}
	counts := map[string]int{}
	for i := 0; i < 140; i++ {
		counts[tr.PickWeighted(1, auths).ID]++
	}
	if counts[boost] != 100 || counts[hold] != 30 || counts["slow-0"] != 10 {
		t.Fatalf("smooth WRR 10/3/1 = %v", counts)
	}
}

func TestCodexSpeedCohortTransitionsAndRestore(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, nil)
	slowPool(tr, 25)
	feed(tr, "acct", 3, time.Second)
	tr.Recompute([]CodexSpeedAuthInfo{{ID: "acct", AccountKey: "key-1"}})
	clock.Advance(61 * time.Minute)
	slowPool(tr, 25)
	feed(tr, "acct", 3, 8*time.Second)
	transitions := tr.Recompute(nil)
	if len(transitions) != 1 || transitions[0].From != CodexSpeedTierFast || transitions[0].To != CodexSpeedTierSlow || transitions[0].AccountKey != "key-1" {
		t.Fatalf("transitions = %+v", transitions)
	}
	boost, hold := tr.cohort.summaries(clock.now)
	total := boost.FastToSlow + hold.FastToSlow
	if total != 1 || boost.FastSet+hold.FastSet != 1 {
		t.Fatalf("cohorts = %+v %+v", boost, hold)
	}
	data, err := tr.MarshalState()
	if err != nil {
		t.Fatal(err)
	}
	restored := NewCodexSpeedTracker(clock.Now)
	if err = restored.RestoreState(data); err != nil {
		t.Fatal(err)
	}
	b2, h2 := restored.cohort.summaries(clock.now)
	if b2.FastToSlow+h2.FastToSlow != 1 {
		t.Fatalf("restored cohorts = %+v %+v", b2, h2)
	}
	sh := restored.shard("acct")
	if a := sh.accounts["acct"]; a == nil || a.lastKnown != CodexSpeedTierSlow {
		t.Fatal("last known tier must survive a restart")
	}
}

func TestCodexSpeedQuotaBurnSkipsResets(t *testing.T) {
	var c codexSpeedCohort
	now := time.Date(2026, 11, 3, 6, 0, 0, 0, time.UTC)
	c.observe(now, "k1", CodexSpeedCohortBoost, CodexSpeedTierFast)
	c.observe(now, "k2", CodexSpeedCohortBoost, CodexSpeedTierFast)
	c.quotaBurn(now, map[string]float64{"k1": 10, "k2": 50})
	boost, _ := c.quotaBurn(now, map[string]float64{"k1": 13, "k2": 2})
	if boost.Accounts != 1 || boost.DeltaSum != 3 {
		t.Fatalf("burn = %+v", boost)
	}
}

func TestCodexSpeedRequestStateAndStreamBootstrapFailure(t *testing.T) {
	tr, _ := newTestSpeedTracker(t, nil)
	ctx, state := tr.withCodexSpeedState(context.Background(), []string{"codex"})
	if state == nil {
		t.Fatal("active tracker must attach request state")
	}
	tr.initRequest(state, "session-x", time.Hour)
	group, _ := CodexSpeedGroupFromContext(ctx)
	if group == "" {
		t.Fatal("group must be set after the first pick")
	}
	tr.finishRequest(state, true, 529)
	if treat, control, _ := tr.ab.currentHour(); treat.Requests+control.Requests != 0 {
		t.Fatal("requests that never executed Codex are not counted")
	}
	MarkCodexSpeedExecuted(ctx)
	result := streamErrorResult(http.Header{}, &Error{Code: "x", HTTPStatus: 529})
	tr.finishStreamRequest(state, result, nil)
	treat, control, _ := tr.ab.currentHour()
	g := treat
	if group == CodexSpeedGroupControl {
		g = control
	}
	if group != CodexSpeedGroupOff && (g.Requests != 1 || g.RequestFailures != 1 || g.Final529 != 1) {
		t.Fatalf("bootstrap failure with nil error must count as failed 529: group=%s %+v", group, g)
	}
	if _, ok := streamErrorResults.Load(result); ok {
		t.Fatal("stream error marker must be removed")
	}
	// Preview-only and inactive paths attach nothing.
	inactive := NewCodexSpeedTracker(nil)
	if _, s := inactive.withCodexSpeedState(context.Background(), []string{"codex"}); s != nil {
		t.Fatal("disabled tracker must not attach state")
	}
	if _, s := tr.withCodexSpeedState(context.Background(), []string{"claude"}); s != nil {
		t.Fatal("non-codex requests get no state")
	}
}

func TestCodexSpeedSessionBucketIsStableAndAdopted(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, nil)
	b1 := tr.ab.sessionBucket("fallback-key", time.Hour, clock.now)
	tr.ab.adoptBucket("lcp-key", b1, time.Hour, clock.now)
	if b2 := tr.ab.sessionBucket("lcp-key", time.Hour, clock.now); b2 != b1 {
		t.Fatalf("adopted bucket %d != %d", b2, b1)
	}
	tr.ab.adoptBucket("lcp-key", (b1+1)%100, time.Hour, clock.now)
	if b3 := tr.ab.sessionBucket("lcp-key", time.Hour, clock.now); b3 != b1 {
		t.Fatal("concurrent first bindings keep the first writer")
	}
	tr.ab.markRebound("lcp-key", time.Hour, clock.now)
	clock.Advance(50 * time.Minute)
	tr.ab.sessionBucket("lcp-key", time.Hour, clock.now) // activity slides the expiry
	clock.Advance(50 * time.Minute)
	if !tr.ab.rebound("lcp-key", clock.now) {
		t.Fatal("an active session keeps its rebound mark beyond one TTL")
	}
	clock.Advance(2 * time.Hour)
	if tr.ab.rebound("lcp-key", clock.now) {
		t.Fatal("an idle session ends and loses its mark")
	}
}

func TestCodexSpeedProbeCandidatesOrderAndReservation(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, nil)
	infos := []CodexSpeedAuthInfo{
		{ID: "high", Schedulable: true, QuotaUsed: 80},
		{ID: "low", Schedulable: true, QuotaUsed: 5},
		{ID: "off", Schedulable: false, QuotaUsed: 1},
	}
	got := tr.ProbeCandidates(infos, 10)
	if len(got) != 2 || got[0] != "low" || got[1] != "high" {
		t.Fatalf("candidates = %v", got)
	}
	if !tr.ReserveProbe("low") || tr.ReserveProbe("low") {
		t.Fatal("a probe in flight must not be reserved twice")
	}
	if got = tr.ProbeCandidates(infos, 10); len(got) != 1 || got[0] != "high" {
		t.Fatalf("in-flight account must be skipped: %v", got)
	}
	tr.ObserveProbe(CodexSpeedProbeResult{AuthID: "low", Success: true, Gap: time.Second})
	clock.Advance(30 * time.Minute)
	if tr.ReserveProbe("low") {
		t.Fatal("one probe per account per interval")
	}
	clock.Advance(31 * time.Minute)
	if !tr.ReserveProbe("low") {
		t.Fatal("interval elapsed")
	}
}

func TestCodexSpeedInactiveReasons(t *testing.T) {
	tr := NewCodexSpeedTracker(nil)
	tr.SetConfig(internalconfig.CodexSpeedPriorityConfig{Enable: true}, false, false)
	if tr.Active() || tr.InactiveReason() != CodexSpeedInactivePoolsOff {
		t.Fatalf("reason = %s", tr.InactiveReason())
	}
	tr.SetConfig(internalconfig.CodexSpeedPriorityConfig{Enable: true}, true, false)
	if tr.InactiveReason() != CodexSpeedInactivePoolsRestart {
		t.Fatalf("reason = %s", tr.InactiveReason())
	}
	tr.SetConfig(internalconfig.CodexSpeedPriorityConfig{Enable: true}, true, true)
	if !tr.Active() {
		t.Fatal("should be active")
	}
}

func TestCodexSpeedHourRollIncludesCohortsAndProbes(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, nil)
	tr.ObserveProbe(CodexSpeedProbeResult{AuthID: "p", Success: true, Gap: time.Second})
	if got := tr.RollHours(nil); len(got) != 0 {
		t.Fatal("unfinished hour must not roll")
	}
	clock.Advance(time.Hour)
	got := tr.RollHours(nil)
	if len(got) != 1 || got[0].Probes != 1 || got[0].Generation == "" || got[0].Boost.Day == "" {
		t.Fatalf("rolled = %+v", got)
	}
	var _ codexspeed.SpeedHour = got[0]
}

func TestCodexSpeedHourSurvivesRequestCrossingTheHour(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, nil)
	gen, _, _ := tr.ab.generation()
	tr.ab.observeRequest(CodexSpeedGroupControl, gen, false, 0, clock.now)
	clock.Advance(time.Hour) // the next request arrives before the ticker rolls the hour
	tr.ab.observeRequest(CodexSpeedGroupControl, gen, false, 0, clock.now)
	got := tr.RollHours(nil)
	if len(got) != 1 || got[0].Control.Requests != 1 {
		t.Fatalf("previous hour must be kept for persistence: %+v", got)
	}
	if _, control, _ := tr.ab.currentHour(); control.Requests != 1 {
		t.Fatalf("current hour = %+v", control)
	}
}

func TestCodexSpeedWeightUsesLearnedAccountCohort(t *testing.T) {
	tr, _ := newTestSpeedTracker(t, nil)
	slowPool(tr, 25)
	var boostKey, holdID string
	for i := 0; boostKey == "" || holdID == ""; i++ {
		v := fmt.Sprintf("v-%d", i)
		if codexSpeedCohortOf(v) == CodexSpeedCohortBoost && boostKey == "" {
			boostKey = v
		} else if codexSpeedCohortOf(v) == CodexSpeedCohortHold && holdID == "" {
			holdID = v
		}
	}
	// The credential ID hashes to hold, but its upstream account is in boost.
	feed(tr, holdID, 3, time.Second)
	tr.Recompute([]CodexSpeedAuthInfo{{ID: holdID, AccountKey: boostKey}})
	cfg := tr.config()
	if w := tr.weightFor(&Auth{ID: holdID}, cfg); w != int64(cfg.Weights.Fast)*authWeight(&Auth{ID: holdID}) {
		t.Fatalf("snapshot without account_id must still use the account cohort, weight %d", w)
	}
}
