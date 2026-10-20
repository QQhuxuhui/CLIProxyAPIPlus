package latencytier

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type fakeAuths struct {
	mu          sync.Mutex
	auths       []*coreauth.Auth
	prio        map[string]int
	invalidated []string
	sets        int
}

func newFakeAuths(n int) *fakeAuths {
	f := &fakeAuths{prio: make(map[string]int)}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("acct-%02d", i)
		f.auths = append(f.auths, &coreauth.Auth{
			ID:         id,
			Provider:   "codex",
			Label:      id + "@example.com",
			Attributes: map[string]string{"priority": "1", "plan_type": "free"},
			Metadata:   map[string]any{"access_token": "tok", "refresh_token": "rt"},
		})
		f.prio[id] = 1
	}
	return f
}

func (f *fakeAuths) List() []*coreauth.Auth {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*coreauth.Auth, 0, len(f.auths))
	for _, a := range f.auths {
		out = append(out, a.Clone())
	}
	return out
}

func (f *fakeAuths) SetAuthPriorityInMemory(id string, priority int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.prio[id]; !ok {
		return false
	}
	f.sets++
	if f.prio[id] == priority {
		return false
	}
	f.prio[id] = priority
	return true
}

func (f *fakeAuths) AuthPriorityInMemory(id string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.prio[id]
	return p, ok
}

func (f *fakeAuths) InFlight(string) int { return 0 }

func (f *fakeAuths) InvalidateSessionAffinity(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated = append(f.invalidated, id)
}

func overloadRecord(id string) usage.Record {
	rec := record(id, 0, true)
	rec.Fail = usage.Failure{StatusCode: 502, Body: `{"error":{"message":"Our servers are currently overloaded. Please try again later.","type":"service_unavailable_error","code":"server_is_overloaded"}}`}
	return rec
}

func record(id string, ttftMs int, failed bool) usage.Record {
	rec := usage.Record{Provider: "codex", AuthType: "oauth", AuthID: id, Model: "gpt-6-luna", Alias: "gpt-6.1-sol", Stream: true, TTFT: time.Duration(ttftMs) * time.Millisecond, Latency: time.Duration(ttftMs+500) * time.Millisecond, Failed: failed}
	if !failed {
		rec.Detail.OutputTokens = 3
		rec.Detail.TotalTokens = 10
	}
	return rec
}

func feed(t *Tracker, id string, n int, ttftMs int) {
	for i := 0; i < n; i++ {
		t.HandleUsage(context.Background(), record(id, ttftMs, false))
	}
}

func newTestTracker(f *fakeAuths, enabled bool) (*Tracker, *time.Time) {
	cfg := DefaultConfig()
	cfg.Enabled = enabled
	tr := New(f, cfg, "")
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	tr.now = func() time.Time { return now }
	return tr, &now
}

func TestRankingAssignsRelativeTiersAndPriorities(t *testing.T) {
	f := newFakeAuths(20)
	tr, _ := newTestTracker(f, true)
	// acct-00..04 fast (1.5s), 05..11 medium (5s), 12..19 slow (12s).
	for i := 0; i < 20; i++ {
		ms := 5000
		if i < 5 {
			ms = 1500
		} else if i >= 12 {
			ms = 12000
		}
		feed(tr, fmt.Sprintf("acct-%02d", i), 5, ms)
	}
	tr.Recompute()
	snap := tr.Snapshot()
	if snap.Eligible != 20 {
		t.Fatalf("eligible = %d, want 20", snap.Eligible)
	}
	// 20% of 20 = 4 fast, bottom 40% = 8 slow.
	if snap.Fast != 4 || snap.Slow != 8 {
		t.Fatalf("fast=%d slow=%d, want 4/8", snap.Fast, snap.Slow)
	}
	for _, v := range snap.Accounts {
		p, _ := f.AuthPriorityInMemory(v.AuthID)
		want := 1
		switch {
		case v.Probation || v.Tier == TierFast:
			want = 2
		case v.Tier == TierSlow:
			want = 0
		}
		if p != want {
			t.Fatalf("%s tier=%s probation=%v priority=%d want %d", v.AuthID, v.Tier, v.Probation, p, want)
		}
	}
	if snap.Probation != 3 {
		t.Fatalf("probation = %d, want 3 (ProbationMin)", snap.Probation)
	}
	if len(f.invalidated) != 8 {
		t.Fatalf("demoted accounts should have affinity invalidated once, got %d", len(f.invalidated))
	}
}

func TestHysteresisKeepsFastUntilExitBand(t *testing.T) {
	f := newFakeAuths(10)
	tr, now := newTestTracker(f, false)
	for i := 0; i < 10; i++ {
		feed(tr, fmt.Sprintf("acct-%02d", i), 5, 1000+i*1000) // acct-00 fastest
	}
	tr.Recompute()
	if tr.tiers["acct-01"] != TierFast {
		t.Fatalf("acct-01 should be fast (rank 2/10 = 10%%)")
	}
	// Make acct-01 slightly slower so it sits at rank 3 (20% <= frac < 30%): must stay fast.
	*now = now.Add(time.Minute)
	feed(tr, "acct-01", 20, 3500)
	tr.Recompute()
	if tr.tiers["acct-01"] != TierFast {
		t.Fatalf("acct-01 at rank 3/10 should stay fast via hysteresis, got %s", tr.tiers["acct-01"])
	}
	// Now push it to rank 5 (40%): leaves fast.
	*now = now.Add(time.Minute)
	feed(tr, "acct-01", 40, 5500)
	tr.Recompute()
	if tr.tiers["acct-01"] != TierNormal {
		t.Fatalf("acct-01 at rank 5/10 should be normal, got %s", tr.tiers["acct-01"])
	}
}

func TestFailuresCountAsPenaltyAndInactiveDropsToNormal(t *testing.T) {
	f := newFakeAuths(6)
	tr, now := newTestTracker(f, false)
	for i := 0; i < 5; i++ {
		feed(tr, fmt.Sprintf("acct-%02d", i), 5, 2000)
	}
	for i := 0; i < 5; i++ {
		tr.HandleUsage(context.Background(), record("acct-05", 0, true))
	}
	tr.Recompute()
	if tr.tiers["acct-05"] != TierSlow {
		t.Fatalf("all-failed account should be slow, got %s", tr.tiers["acct-05"])
	}
	view := tr.Snapshot()
	for _, v := range view.Accounts {
		if v.AuthID == "acct-05" && (v.RecentFailures != 5 || v.EWMAMs != 30000) {
			t.Fatalf("acct-05 recent_failures=%d ewma=%v", v.RecentFailures, v.EWMAMs)
		}
	}
	if tr.tiers["acct-00"] != TierFast {
		t.Fatalf("acct-00 should start in the fast tier, got %s", tr.tiers["acct-00"])
	}
	*now = now.Add(31 * time.Minute)
	// Everybody but acct-00 (fast) and acct-05 (slow) is measured again.
	for i := 1; i < 5; i++ {
		feed(tr, fmt.Sprintf("acct-%02d", i), 1, 2000)
	}
	tr.Recompute()
	if tr.tiers["acct-00"] == TierFast || !tr.eligible["acct-00"] {
		t.Fatalf("stale account must not hold the fast tier (tier=%s eligible=%v)", tr.tiers["acct-00"], tr.eligible["acct-00"])
	}
	if tr.tiers["acct-01"] != TierFast {
		t.Fatalf("recently measured fast account should stay fast, got %s", tr.tiers["acct-01"])
	}
	if tr.tiers["acct-05"] != TierSlow || !tr.eligible["acct-05"] {
		t.Fatalf("stale slow account keeps its rank until re-measured (tier=%s eligible=%v)", tr.tiers["acct-05"], tr.eligible["acct-05"])
	}
}

func TestDisableRestoresBaselinePriorities(t *testing.T) {
	f := newFakeAuths(8)
	f.prio["acct-07"] = 5
	f.auths[7].Attributes["priority"] = "5"
	tr, _ := newTestTracker(f, true)
	for i := 0; i < 8; i++ {
		feed(tr, fmt.Sprintf("acct-%02d", i), 5, 1000+i*1000)
	}
	tr.Recompute()
	if p, _ := f.AuthPriorityInMemory("acct-07"); p != 0 {
		t.Fatalf("slowest account should be priority 0 while enabled, got %d", p)
	}
	cfg := tr.Config()
	cfg.Enabled = false
	if err := tr.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.AuthPriorityInMemory("acct-07"); p != 5 {
		t.Fatalf("baseline priority not restored, got %d", p)
	}
	if p, _ := f.AuthPriorityInMemory("acct-00"); p != 1 {
		t.Fatalf("baseline priority not restored for acct-00, got %d", p)
	}
	// Recompute while disabled must not touch priorities.
	before := f.sets
	tr.Recompute()
	if f.sets != before {
		t.Fatalf("recompute while disabled wrote priorities")
	}
}

func probationPicks(tr *Tracker) []string {
	picked := make([]string, 0)
	for id, ok := range tr.probation {
		if ok {
			picked = append(picked, id)
		}
	}
	sort.Strings(picked)
	return picked
}

func TestProbationPrefersLeastRecentlySampledAndRotates(t *testing.T) {
	f := newFakeAuths(13)
	// acct-11 is disabled and acct-12 carries an expired token: never worth a probation slot.
	f.auths[11].Disabled = true
	f.auths[12].Metadata["expired"] = "2026-09-28T05:57:16.000Z"
	tr, now := newTestTracker(f, true)
	for i := 0; i < 8; i++ {
		feed(tr, fmt.Sprintf("acct-%02d", i), 5, 1000+i*500)
	}
	// acct-08..10 never sampled and selectable: they must be the first probation picks.
	*now = now.Add(time.Minute)
	tr.Recompute()
	first := probationPicks(tr)
	if fmt.Sprint(first) != "[acct-08 acct-09 acct-10]" {
		t.Fatalf("first probation picks = %v", first)
	}
	for _, id := range first {
		if p, _ := f.AuthPriorityInMemory(id); p != 2 {
			t.Fatalf("probation account %s should get fast priority, got %d", id, p)
		}
	}
	// Next cycle: the probed accounts produced nothing, yet they rotate out and the
	// least recently sampled non-fast accounts take the slots.
	*now = now.Add(time.Minute)
	tr.Recompute()
	second := probationPicks(tr)
	if len(second) != 3 {
		t.Fatalf("second probation picks = %v", second)
	}
	for _, id := range second {
		for _, prev := range first {
			if id == prev {
				t.Fatalf("probation did not rotate: %v then %v", first, second)
			}
		}
		if id == "acct-11" || id == "acct-12" {
			t.Fatalf("unselectable account picked for probation: %v", second)
		}
		if tr.tiers[id] == TierFast {
			t.Fatalf("fast account %s must not take a probation slot", id)
		}
	}
	for _, id := range first {
		if p, _ := f.AuthPriorityInMemory(id); p != 1 {
			t.Fatalf("rotated-out probation account %s should return to normal priority, got %d", id, p)
		}
	}
}

func TestOverloadCountsAsSmallPenaltyNotFullFailure(t *testing.T) {
	f := newFakeAuths(2)
	tr, _ := newTestTracker(f, false)
	tr.HandleUsage(context.Background(), overloadRecord("acct-00"))
	tr.HandleUsage(context.Background(), record("acct-01", 0, true))
	st0 := tr.stats["acct-00"]
	if st0.Samples != 0 || st0.Weight != 0 || st0.Failures != 1 || len(st0.Recent) != 1 || !st0.Recent[0] || st0.recentCount(tr.now()) != 1 {
		t.Fatalf("overload must count as a failure and activity but not as a latency sample: %+v", st0)
	}
	cfg := tr.Config()
	cfg.OverloadPenaltyMs = 3000
	if err := tr.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	tr.HandleUsage(context.Background(), overloadRecord("acct-00"))
	if tr.stats["acct-00"].EWMA != 3000 || tr.stats["acct-00"].Samples != 1 {
		t.Fatalf("with a configured overload penalty the sample is recorded: ewma=%v samples=%d", tr.stats["acct-00"].EWMA, tr.stats["acct-00"].Samples)
	}
	cfg.OverloadPenaltyMs = 0
	if err := tr.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if tr.stats["acct-01"].EWMA != 30000 {
		t.Fatalf("other failures keep the full penalty, got %v", tr.stats["acct-01"].EWMA)
	}
	// A fast account that eats a few pool-wide overloads must stay ahead of a genuinely slow one.
	feed(tr, "acct-00", 4, 2000)
	tr.HandleUsage(context.Background(), overloadRecord("acct-00"))
	feed(tr, "acct-01", 5, 12000)
	if tr.stats["acct-00"].EWMA >= tr.stats["acct-01"].EWMA {
		t.Fatalf("overloaded-but-fast account ranks behind slow one: %v vs %v", tr.stats["acct-00"].EWMA, tr.stats["acct-01"].EWMA)
	}
}

func TestPriorityIsReappliedWhenRuntimeValueDrifts(t *testing.T) {
	f := newFakeAuths(20)
	tr, _ := newTestTracker(f, true)
	for i := 0; i < 20; i++ {
		feed(tr, fmt.Sprintf("acct-%02d", i), 5, 1000+i*500)
	}
	tr.Recompute()
	if p, _ := f.AuthPriorityInMemory("acct-00"); p != 2 {
		t.Fatalf("acct-00 should be promoted, got %d", p)
	}
	// Simulate an auth file reload that resets the runtime attribute.
	f.mu.Lock()
	f.prio["acct-00"] = 1
	f.mu.Unlock()
	tr.Recompute()
	if p, _ := f.AuthPriorityInMemory("acct-00"); p != 2 {
		t.Fatalf("drifted priority must be re-applied on the next cycle, got %d", p)
	}
}

func TestPlusAccountsAreTrustedUntilMeasuredAndProbedFirst(t *testing.T) {
	f := newFakeAuths(30)
	for i := 25; i < 30; i++ {
		f.auths[i].Attributes["plan_type"] = "plus"
	}
	f.auths[29].Disabled = true
	tr, now := newTestTracker(f, true)
	for i := 0; i < 20; i++ {
		feed(tr, fmt.Sprintf("acct-%02d", i), 5, 1000+i*500)
	}
	// acct-25 is a plus account that measured slow: trust must not override data.
	feed(tr, "acct-25", 5, 40000)
	*now = now.Add(time.Minute)
	tr.Recompute()
	snap := tr.Snapshot()
	trusted := map[string]bool{}
	for _, v := range snap.Accounts {
		if v.Trusted {
			trusted[v.AuthID] = true
			if v.Tier != TierFast {
				t.Fatalf("trusted %s should be fast, got %s", v.AuthID, v.Tier)
			}
		}
	}
	if !trusted["acct-26"] || !trusted["acct-27"] || !trusted["acct-28"] || trusted["acct-29"] || trusted["acct-25"] {
		t.Fatalf("trusted set = %v", trusted)
	}
	if tr.tiers["acct-25"] != TierSlow {
		t.Fatalf("measured slow plus account should be slow, got %s", tr.tiers["acct-25"])
	}
	for _, id := range []string{"acct-26", "acct-27", "acct-28"} {
		if p, _ := f.AuthPriorityInMemory(id); p != 2 {
			t.Fatalf("trusted plus %s should get fast priority, got %d", id, p)
		}
	}
	if p, _ := f.AuthPriorityInMemory("acct-29"); p != 1 {
		t.Fatalf("disabled plus account must not be promoted, got %d", p)
	}
	// Probation prefers plus accounts over never-sampled free ones (acct-20..24).
	if !tr.probation["acct-25"] {
		t.Fatalf("slow plus account should be first in line for probation, picks=%v", probationPicks(tr))
	}
}

func TestGateHoldsPrioritiesUntilEnoughOfPoolIsRankable(t *testing.T) {
	f := newFakeAuths(100)
	tr, _ := newTestTracker(f, true)
	// Only 5 of 100 measured: "top 20%" would be a single account absorbing all traffic.
	for i := 0; i < 5; i++ {
		feed(tr, fmt.Sprintf("acct-%02d", i), 5, 1000+i*1000)
	}
	tr.Recompute()
	snap := tr.Snapshot()
	if snap.GateOpen || snap.Applied != 0 || snap.GateReason == "" || snap.MinEligible != 10 {
		t.Fatalf("gate should be closed: open=%v applied=%d reason=%q min=%d", snap.GateOpen, snap.Applied, snap.GateReason, snap.MinEligible)
	}
	if f.sets != 0 {
		t.Fatalf("no priority may be rewritten while the gate is closed, got %d writes", f.sets)
	}
	if snap.Fast != 1 || snap.Eligible != 5 || snap.EligiblePercent != 5 {
		t.Fatalf("ranking should still be computed for display: fast=%d eligible=%d pct=%v", snap.Fast, snap.Eligible, snap.EligiblePercent)
	}
	for i := 5; i < 10; i++ {
		feed(tr, fmt.Sprintf("acct-%02d", i), 5, 1000+i*1000)
	}
	tr.Recompute()
	snap = tr.Snapshot()
	if !snap.GateOpen || snap.Applied == 0 {
		t.Fatalf("gate should open at 10%%: open=%v applied=%d", snap.GateOpen, snap.Applied)
	}
	if p, _ := f.AuthPriorityInMemory("acct-00"); p != 2 {
		t.Fatalf("fastest account should be promoted once the gate opens, got %d", p)
	}
	// The pool grows (new credentials appear): the gate closes again and priorities go back.
	for i := 100; i < 200; i++ {
		id := fmt.Sprintf("acct-%03d", i)
		f.auths = append(f.auths, &coreauth.Auth{ID: id, Provider: "codex", Attributes: map[string]string{"priority": "1"}, Metadata: map[string]any{"access_token": "tok"}})
		f.prio[id] = 1
	}
	tr.Recompute()
	snap = tr.Snapshot()
	if snap.GateOpen || snap.Applied != 0 {
		t.Fatalf("gate should close when the rankable share drops: open=%v applied=%d", snap.GateOpen, snap.Applied)
	}
	if p, _ := f.AuthPriorityInMemory("acct-00"); p != 1 {
		t.Fatalf("baseline priority should be restored when the gate closes, got %d", p)
	}
}

func TestIgnoresNonCodexAndOtherModels(t *testing.T) {
	f := newFakeAuths(2)
	tr, _ := newTestTracker(f, false)
	rec := record("acct-00", 1000, false)
	rec.Provider = "antigravity"
	tr.HandleUsage(context.Background(), rec)
	rec = record("acct-00", 1000, false)
	rec.AuthType = "apikey"
	tr.HandleUsage(context.Background(), rec)
	rec = record("acct-00", 1000, false)
	rec.Model, rec.Alias = "gpt-image-2", "gpt-image-2"
	tr.HandleUsage(context.Background(), rec)
	if len(tr.stats) != 0 {
		t.Fatalf("unexpected samples recorded: %d", len(tr.stats))
	}
	cut := record("acct-00", 0, false)
	cut.Detail = usage.Detail{}
	tr.HandleUsage(context.Background(), cut)
	if st := tr.stats["acct-00"]; st == nil || st.Failures != 1 {
		t.Fatalf("stream cut after created should count as failure")
	}
}

func TestPersistAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "latency-tier.json")
	f := newFakeAuths(3)
	cfg := DefaultConfig()
	cfg.Enabled = true
	tr := New(f, cfg, path)
	feed(tr, "acct-00", 6, 1200)
	tr.Recompute()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	again := New(f, DefaultConfig(), path)
	if err := again.Load(); err != nil {
		t.Fatal(err)
	}
	if !again.Config().Enabled {
		t.Fatalf("persisted enabled flag should win over startup default")
	}
	if st := again.stats["acct-00"]; st == nil || st.Samples != 6 {
		t.Fatalf("stats not restored: %+v", st)
	}
	if again.baseline["acct-00"] != 1 {
		t.Fatalf("baseline not restored: %v", again.baseline)
	}
}

func TestConfigValidation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FastExitPercent = 0.1
	if err := cfg.Validate(); err == nil {
		t.Fatalf("exit < enter must be rejected")
	}
	cfg = DefaultConfig()
	cfg.FastExitPercent, cfg.SlowExitPercent = 0.6, 0.6
	if err := cfg.Validate(); err == nil {
		t.Fatalf("overlapping bands must be rejected")
	}
	cfg = DefaultConfig()
	cfg.SlowPriority = 1
	if err := cfg.Validate(); err == nil {
		t.Fatalf("non-monotonic priorities must be rejected")
	}
}
