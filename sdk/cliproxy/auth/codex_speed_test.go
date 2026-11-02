package auth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type speedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *speedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *speedClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

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

func infosOf(ids ...string) []CodexSpeedAuthInfo {
	out := make([]CodexSpeedAuthInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, CodexSpeedAuthInfo{ID: id, Schedulable: true, QuotaUsed: -1})
	}
	return out
}

// rankPool feeds n accounts named prefix-i with gaps (i+1) seconds and recomputes; it returns
// the IDs fastest first.
func rankPool(tr *CodexSpeedTracker, prefix string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%03d", prefix, i)
		feed(tr, ids[i], 3, time.Duration(i+1)*time.Second)
	}
	tr.Recompute(infosOf(ids...))
	return ids
}

func useSpeedTracker(t *testing.T, tr *CodexSpeedTracker) {
	t.Helper()
	previous := defaultCodexSpeedTracker
	defaultCodexSpeedTracker = tr
	t.Cleanup(func() { defaultCodexSpeedTracker = previous })
}

func TestCodexSpeedFastMembersHysteresis(t *testing.T) {
	ranked := make([]string, 100)
	for i := range ranked {
		ranked[i] = fmt.Sprintf("a%03d", i)
	}
	first := codexSpeedFastMembers(ranked, nil, 100, 30)
	if len(first) != 30 || !first["a000"] || !first["a029"] || first["a030"] {
		t.Fatalf("first build must fill the target by rank: %d", len(first))
	}
	// a029 drops to rank 33 (inside keep 35) and stays; the newcomer now at rank 29 is inside
	// the band but outside enter 25, so it does not push in while the group is full.
	moved := append([]string{}, ranked[:29]...)
	moved = append(moved, ranked[30:34]...)
	moved = append(moved, "a029")
	moved = append(moved, ranked[34:]...)
	second := codexSpeedFastMembers(moved, first, 100, 30)
	if !second["a029"] || len(second) != 30 {
		t.Fatalf("a member inside keep must stay: %d %v", len(second), second["a029"])
	}
	// Beyond keep the member leaves and the group refills by rank.
	far := append(append([]string{}, ranked[:29]...), ranked[30:40]...)
	far = append(far, "a029")
	far = append(far, ranked[40:]...)
	third := codexSpeedFastMembers(far, first, 100, 30)
	if third["a029"] || len(third) != 30 {
		t.Fatalf("a member beyond keep must leave and the group refill: %d", len(third))
	}
	if up := codexSpeedFastMembers(ranked, first, 100, 40); len(up) != 40 {
		t.Fatalf("raising X must grow the group at once: %d", len(up))
	}
	if down := codexSpeedFastMembers(ranked, first, 100, 10); len(down) != 15 {
		t.Fatalf("lowering X keeps members inside keep (15): %d", len(down))
	}
	if shrink := codexSpeedFastMembers(ranked[:10], first, 100, 30); len(shrink) != 10 {
		t.Fatalf("a smaller ranked set shrinks the group: %d", len(shrink))
	}
	if tiny := codexSpeedFastMembers(ranked[:10], nil, 10, 1); len(tiny) != 0 {
		t.Fatalf("1%% of 10 accounts is no fast account: %d", len(tiny))
	}
}

func TestCodexSpeedRecomputeRanksAndWaitsForScores(t *testing.T) {
	tr, _ := newTestSpeedTracker(t, nil)
	rankPool(tr, "few", codexSpeedReadyMinScored-1)
	if tr.Ready() || tr.selecting() {
		t.Fatal("fewer than 20 scored accounts must not be ready")
	}
	ids := rankPool(tr, "acct", 40)
	if !tr.Ready() {
		t.Fatal("40 scored accounts must be ready")
	}
	view := tr.view.Load()
	if view.total != 40 || view.fastCount != 12 || !view.fast[ids[0]] || view.fast[ids[39]] {
		t.Fatalf("fast group: total=%d fast=%d", view.total, view.fastCount)
	}
	tr.ObserveAttempt(CodexSpeedAttempt{AuthID: "no-score", Success: true, Gap: time.Second, InputTokens: 100})
	tr.Recompute(infosOf(append(ids, "no-score")...))
	if tr.view.Load().unscored != 1 {
		t.Fatalf("an account with one sample has no score: %d", tr.view.Load().unscored)
	}
}

func TestCodexSpeedOverloadThresholdIsStrict(t *testing.T) {
	tr, _ := newTestSpeedTracker(t, nil)
	ids := rankPool(tr, "acct", 30)
	slow := ids[29]
	// 7 successes and 3 overloads at one instant (no decay): exactly 30% stays ranked.
	feed(tr, slow, 4, 30*time.Second)
	for i := 0; i < 3; i++ {
		tr.ObserveAttempt(CodexSpeedAttempt{AuthID: slow, Overload: true})
	}
	tr.Recompute(infosOf(ids...))
	if tr.view.Load().penalized[slow] {
		t.Fatal("an overload rate of exactly 30% must stay ranked")
	}
	tr.ObserveAttempt(CodexSpeedAttempt{AuthID: slow, Overload: true})
	tr.Recompute(infosOf(ids...))
	if !tr.view.Load().penalized[slow] {
		t.Fatal("above 30% the account must leave the ranking")
	}
}

func TestCodexSpeedInstantDemotionAndCooldown(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, nil)
	ids := rankPool(tr, "acct", 40)
	fast := ids[0]
	if !tr.inFast(fast, clock.Now()) {
		t.Fatal("setup: fastest account must be fast")
	}
	tr.ObserveAttempt(CodexSpeedAttempt{AuthID: fast, Overload: true})
	if !tr.inFast(fast, clock.Now()) {
		t.Fatal("one overload must not demote")
	}
	tr.ObserveAttempt(CodexSpeedAttempt{AuthID: fast, Overload: true})
	if tr.inFast(fast, clock.Now()) {
		t.Fatal("two overloads in a row must demote at once")
	}
	tr.ObserveAttempt(CodexSpeedAttempt{AuthID: fast, Overload: true})
	tr.ObserveAttempt(CodexSpeedAttempt{AuthID: fast, Overload: true})
	if got := tr.stats.current(clock.Now()).InstantDemotions; got != 1 {
		t.Fatalf("a demotion counts once during its cooldown: %d", got)
	}
	if class, _ := tr.overflowKey(tr.view.Load(), fast, clock.Now()); class != 1 {
		t.Fatalf("a just-demoted account must sort after healthy accounts: class %d", class)
	}
	tr.Recompute(infosOf(ids...))
	if tr.view.Load().fast[fast] {
		t.Fatal("cooldown keeps the account out of the next ranking")
	}
	// After the cooldown (10 min) and the overloads' decay, fast samples bring it back.
	clock.Advance(40 * time.Minute)
	for _, id := range ids {
		feed(tr, id, 3, time.Duration(10+len(id))*time.Second)
	}
	feed(tr, fast, 12, 500*time.Millisecond)
	tr.Recompute(infosOf(ids...))
	if !tr.inFast(fast, clock.Now()) {
		t.Fatal("after the cooldown a fast score brings the account back")
	}
}

func TestCodexSpeedSlowGapsDemote(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, nil)
	ids := rankPool(tr, "acct", 40)
	fast := ids[0]
	feed(tr, fast, 2, 60*time.Second)
	if !tr.inFast(fast, clock.Now()) {
		t.Fatal("two slow gaps must not demote")
	}
	feed(tr, fast, 1, 60*time.Second)
	if tr.inFast(fast, clock.Now()) {
		t.Fatal("three gaps slower than the slow median must demote")
	}
}

func TestCodexSpeedProbeWindowCoversSlowCycles(t *testing.T) {
	cfg := internalconfig.CodexSpeedPriorityConfig{}
	cfg.Normalize()
	if got := probeWindow(cfg, 0); got != 40*time.Minute {
		t.Fatalf("10 min interval: %v", got)
	}
	cfg.Probe.IntervalMinutes = 60
	if got := probeWindow(cfg, 0); got != 240*time.Minute {
		t.Fatalf("60 min interval: %v", got)
	}
	cfg.Probe.IntervalMinutes = 10
	if got := probeWindow(cfg, 32*time.Minute); got != 128*time.Minute {
		t.Fatalf("a slow cycle must widen the window: %v", got)
	}
	tr, _ := newTestSpeedTracker(t, func(c *internalconfig.CodexSpeedPriorityConfig) { c.Probe.RatePerSecond = 1 })
	if got := tr.probeCycle(tr.config(), 1900); got < 31*time.Minute {
		t.Fatalf("1900 accounts at 1/s take over 31 minutes: %v", got)
	}
}

// Without user traffic, probes alone give a score at a 10 and a 60 minute interval, and with
// 1900 accounts at 1/s the widened window still keeps three probe samples.
func TestCodexSpeedProbesAloneScoreAnAccount(t *testing.T) {
	for _, tc := range []struct {
		interval, rate, accounts int
	}{{10, 3, 1}, {60, 3, 1}, {10, 1, 1900}} {
		tr, clock := newTestSpeedTracker(t, func(c *internalconfig.CodexSpeedPriorityConfig) {
			c.Probe.IntervalMinutes, c.Probe.RatePerSecond = tc.interval, tc.rate
		})
		ids := make([]string, tc.accounts)
		for i := range ids {
			ids[i] = fmt.Sprintf("idle-%04d", i)
		}
		cycle := tr.probeCycle(tr.config(), tc.accounts)
		step := max(time.Duration(tc.interval)*time.Minute, cycle) + time.Minute
		for round := 0; round < 3; round++ {
			if !tr.ReserveProbe(ids[0]) {
				t.Fatalf("%+v: probe %d must be due", tc, round)
			}
			tr.ObserveProbe(CodexSpeedProbeResult{AuthID: ids[0], Success: true, Gap: 800 * time.Millisecond})
			if round < 2 {
				clock.Advance(step)
			}
			tr.Recompute(infosOf(ids...))
		}
		if _, ok := tr.view.Load().score[ids[0]]; !ok {
			t.Fatalf("%+v: three probes must give a score", tc)
		}
	}
}

func TestCodexSpeedProbeCandidatesSkipFastAndPreferUnscored(t *testing.T) {
	tr, clock := newTestSpeedTracker(t, nil)
	ids := rankPool(tr, "acct", 30)
	infos := append(infosOf(ids...), CodexSpeedAuthInfo{ID: "new", Schedulable: true, QuotaUsed: 5})
	got := tr.ProbeCandidates(infos, 100)
	if len(got) == 0 || got[0] != "new" {
		t.Fatalf("unscored accounts go first: %v", got)
	}
	for _, id := range got {
		if tr.inFast(id, clock.Now()) {
			t.Fatalf("fast account %s must not be probed", id)
		}
	}
	if !tr.ReserveProbe("new") || tr.ReserveProbe("new") {
		t.Fatal("a reserved probe blocks a second one")
	}
}

func TestCodexSpeedBinsCoverEveryAccount(t *testing.T) {
	for _, n := range []int{7, 123} {
		tr, _ := newTestSpeedTracker(t, nil)
		ids := rankPool(tr, fmt.Sprintf("n%d", n), n)
		infos := infosOf(ids...)
		infos[0].RateLimit, infos[0].RateCount = 10, 5
		tr.Recompute(infos)
		view := tr.view.Load()
		if len(view.bins) != codexSpeedBinCount {
			t.Fatalf("n=%d: %d bins", n, len(view.bins))
		}
		total, fast := 0, 0
		for _, b := range view.bins {
			total += b.Accounts
			fast += b.Fast
			if b.Accounts == 0 && (b.ScoreMs != nil || b.Util != nil) {
				t.Fatalf("n=%d: an empty bin must carry no values", n)
			}
		}
		if total != n || fast != view.fastCount {
			t.Fatalf("n=%d: bins cover %d accounts, %d fast (want %d)", n, total, fast, view.fastCount)
		}
		for _, b := range view.bins {
			if b.Accounts > 0 {
				if b.ScoreMs == nil || *b.ScoreMs > 2000 || b.Util == nil || *b.Util != 0.5 {
					t.Fatalf("n=%d: first bin must hold the fastest account and its load", n)
				}
				break
			}
		}
	}
}

func TestCodexSpeedStatsAttributeByStartGroupAndFinishHour(t *testing.T) {
	clock := &speedClock{now: time.Date(2026, 11, 3, 6, 59, 0, 0, time.UTC)}
	tr := NewCodexSpeedTracker(clock.Now)
	tr.SetConfig(internalconfig.CodexSpeedPriorityConfig{Enable: true}, true, true)
	clock.Advance(3 * time.Minute)
	tr.ObserveAttempt(CodexSpeedAttempt{AuthID: "a", Success: true, TTFT: time.Second, InputTokens: 10, Group: CodexSpeedGroupFast})
	tr.ObserveAttempt(CodexSpeedAttempt{AuthID: "b", Overload: true, Group: CodexSpeedGroupSlow})
	tr.ObserveAttempt(CodexSpeedAttempt{AuthID: "c", Success: true, Group: CodexSpeedGroupPlugin})
	tr.ObserveAttempt(CodexSpeedAttempt{AuthID: "d", Success: true, Group: ""})
	cur := tr.stats.current(clock.Now())
	if !cur.Hour.Equal(time.Date(2026, 11, 3, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("attempts count in the hour they finished: %v", cur.Hour)
	}
	if cur.Fast.Attempts != 1 || cur.Slow.Attempts != 1 || cur.Slow.Overloads != 1 || cur.Overall.Attempts != 3 {
		t.Fatalf("groups: fast=%d slow=%d overall=%d", cur.Fast.Attempts, cur.Slow.Attempts, cur.Overall.Attempts)
	}
	if cur.OverflowShare == nil || *cur.OverflowShare != 0.5 || cur.Fast.P50Ms == nil {
		t.Fatalf("share %v p50 %v", cur.OverflowShare, cur.Fast.P50Ms)
	}
	clock.Advance(time.Hour)
	rolled := tr.RollHours()
	if len(rolled) != 1 || rolled[0].Overall.Attempts != 3 || rolled[0].Schema != "ranked" {
		t.Fatalf("rolled: %+v", rolled)
	}
}

func TestCodexSpeedGroupAtFollowsPickSource(t *testing.T) {
	tr, _ := newTestSpeedTracker(t, nil)
	ids := rankPool(tr, "acct", 30)
	ctx := withCodexSpeedState(context.Background(), []string{"codex"})
	fast := &Auth{ID: ids[0], Provider: "codex"}
	slow := &Auth{ID: ids[29], Provider: "codex"}
	if tr.GroupAt(ctx, fast) != CodexSpeedGroupFast || tr.GroupAt(ctx, slow) != CodexSpeedGroupSlow {
		t.Fatal("ranked groups")
	}
	notePick(ctx, slow.ID)
	if tr.GroupAt(ctx, slow) != CodexSpeedGroupPlugin || tr.GroupAt(ctx, fast) != CodexSpeedGroupFast {
		t.Fatal("only the plugin-picked account is attributed to the plugin")
	}
	notePick(ctx, "")
	if tr.GroupAt(ctx, slow) != CodexSpeedGroupSlow {
		t.Fatal("a later built-in pick clears the plugin mark")
	}
	if tr.GroupAt(ctx, &Auth{ID: "key", Provider: "codex", Attributes: map[string]string{AttributeAPIKey: "sk"}}) != "" {
		t.Fatal("API key accounts are excluded")
	}
	tr.SetConfig(internalconfig.CodexSpeedPriorityConfig{}, true, true)
	if tr.GroupAt(ctx, fast) != CodexSpeedGroupOff {
		t.Fatal("disabled ranking is off")
	}
}

func TestCodexSpeedInactiveReasons(t *testing.T) {
	tr, _ := newTestSpeedTracker(t, nil)
	if !tr.Active() || tr.InactiveReason() != "" {
		t.Fatal("enabled with pools running must be active")
	}
	tr.SetConfig(internalconfig.CodexSpeedPriorityConfig{Enable: true}, false, false)
	if tr.InactiveReason() != CodexSpeedInactivePoolsOff {
		t.Fatal(tr.InactiveReason())
	}
	tr.SetConfig(internalconfig.CodexSpeedPriorityConfig{Enable: true}, true, false)
	if tr.InactiveReason() != CodexSpeedInactivePoolsRestart {
		t.Fatal(tr.InactiveReason())
	}
}

// --- selection ---

func speedSelectionManager(t *testing.T, n int) (*Manager, *CodexSpeedTracker, *speedClock, []string) {
	t.Helper()
	m := newPoolStorageManager(t, 4)
	m.RegisterExecutor(&retryRoundCallExecutor{identifier: "codex"})
	m.SetSelector(NewSessionAffinitySelector(&FillFirstSelector{}))
	tr, clock := newTestSpeedTracker(t, nil)
	useSpeedTracker(t, tr)
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("sel-%02d", i)
		registerPoolModel(t, m, ids[i])
		feed(tr, ids[i], 3, time.Duration(i+1)*time.Second)
	}
	tr.Recompute(infosOf(ids...))
	return m, tr, clock, ids
}

func sessionOpts(id string) exec.Options {
	opts := exec.Options{Headers: http.Header{"Session-Id": {id}}}
	opts.EnsureMetadata()
	return opts
}

func fastSet(tr *CodexSpeedTracker, ids []string, now time.Time) map[string]struct{} {
	out := map[string]struct{}{}
	for _, id := range ids {
		if tr.inFast(id, now) {
			out[id] = struct{}{}
		}
	}
	return out
}

func TestCodexSpeedPicksFastAccountsEvenly(t *testing.T) {
	m, tr, clock, ids := speedSelectionManager(t, 24)
	fast := fastSet(tr, ids, clock.Now())
	counts := map[string]int{}
	for i := 0; i < 1400; i++ {
		a, _, err := m.pickCodexPool(context.Background(), "pool-model", exec.Options{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := fast[a.ID]; !ok {
			t.Fatalf("pick %d went to non-fast %s while fast accounts are free", i, a.ID)
		}
		counts[a.ID]++
	}
	// 24 accounts at 30% = 7 fast; fill-first would pin one account, random spreads ~200 each.
	if len(counts) != len(fast) || len(fast) != 7 {
		t.Fatalf("fast accounts used: %v", counts)
	}
	for id, c := range counts {
		if c < 120 {
			t.Fatalf("%s got only %d of 1400 picks: %v", id, c, counts)
		}
	}
}

func TestCodexSpeedOverflowPicksBestRankedAcrossPools(t *testing.T) {
	m, tr, clock, ids := speedSelectionManager(t, 24)
	tried := fastSet(tr, ids, clock.Now())
	n := len(tried)
	a, _, err := m.pickCodexPool(context.Background(), "pool-model", exec.Options{}, tried)
	if err != nil || a.ID != ids[n] {
		t.Fatalf("overflow must take rank %d (%s): %v %v", n, ids[n], a, err)
	}
	tr.demoted.Store(ids[n], clock.Now().Add(time.Hour))
	a, _, err = m.pickCodexPool(context.Background(), "pool-model", exec.Options{}, tried)
	if err != nil || a.ID != ids[n+1] {
		t.Fatalf("a demoted account must lose to the next healthy one: %v %v", a, err)
	}
}

func TestCodexSpeedReplacesSlowBindingOnceWithFastOnly(t *testing.T) {
	m, tr, clock, ids := speedSelectionManager(t, 24)
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	opts := sessionOpts("speed-session")
	key, affinity := m.poolSessionKey("pool-model", opts)
	slow, _ := m.GetByID(ids[23])
	if _, _, err := m.bindPoolSelection(context.Background(), key, "pool-model", opts, affinity, slow, nil, nil); err != nil {
		t.Fatal(err)
	}
	a, _, err := m.pickCodexPool(context.Background(), "pool-model", sessionOpts("speed-session"), nil)
	if err != nil || !tr.inFast(a.ID, clock.Now()) {
		t.Fatalf("slow binding must be replaced by a fast account: %v %v", a, err)
	}
	if !tr.sessions.rebound(key, clock.Now()) {
		t.Fatal("the replacement must be recorded")
	}
	tr.demoted.Store(a.ID, clock.Now().Add(time.Hour))
	again, _, err := m.pickCodexPool(context.Background(), "pool-model", sessionOpts("speed-session"), nil)
	if err != nil || again.ID != a.ID {
		t.Fatalf("a session is sped up only once: %v %v", again, err)
	}
}

func TestCodexSpeedKeepsBindingWithoutFastAndFailsOverWithoutSpending(t *testing.T) {
	m, tr, clock, ids := speedSelectionManager(t, 24)
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	busy := fastSet(tr, ids, clock.Now())
	opts := sessionOpts("keep")
	key, affinity := m.poolSessionKey("pool-model", opts)
	slow, _ := m.GetByID(ids[20])
	if _, _, err := m.bindPoolSelection(context.Background(), key, "pool-model", opts, affinity, slow, nil, nil); err != nil {
		t.Fatal(err)
	}
	a, _, err := m.pickCodexPool(context.Background(), "pool-model", sessionOpts("keep"), busy)
	if err != nil || a.ID != ids[20] || tr.sessions.rebound(key, clock.Now()) {
		t.Fatalf("without a free fast account the binding stays and the chance is kept: %v %v", a, err)
	}
	busy[ids[20]] = struct{}{}
	a, _, err = m.pickCodexPool(context.Background(), "pool-model", sessionOpts("keep"), busy)
	if err != nil || a.ID == ids[20] || tr.sessions.rebound(key, clock.Now()) {
		t.Fatalf("failover must pick another account without spending the replacement: %v %v", a, err)
	}
}

func TestCodexSpeedMixedPreviewDefersReplacement(t *testing.T) {
	m, tr, clock, ids := speedSelectionManager(t, 24)
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	opts := sessionOpts("mixed")
	key, affinity := m.poolSessionKey("pool-model", opts)
	slow, _ := m.GetByID(ids[22])
	if _, _, err := m.bindPoolSelection(context.Background(), key, "pool-model", opts, affinity, slow, nil, nil); err != nil {
		t.Fatal(err)
	}
	preview := context.WithValue(context.Background(), poolPreviewKey{}, true)
	pooled := sessionOpts("mixed")
	a, _, err := m.pickCodexPool(preview, "pool-model", pooled, nil)
	if err != nil || !tr.inFast(a.ID, clock.Now()) {
		t.Fatalf("preview must offer a fast replacement: %v %v", a, err)
	}
	replace, ok := pooled.Metadata[poolSpeedReplaceMetadataKey].(codexPoolBinding)
	if !ok || replace.authID != ids[22] || tr.sessions.rebound(key, clock.Now()) {
		t.Fatal("preview must not write the binding")
	}
	if _, _, err = m.bindPoolSelection(context.Background(), key, "pool-model", pooled, affinity, a, nil, &replace); err != nil {
		t.Fatal(err)
	}
	if !tr.sessions.rebound(key, clock.Now()) {
		t.Fatal("binding after settling on Codex must replace and record")
	}
	got, _, err := m.pickCodexPool(context.Background(), "pool-model", sessionOpts("mixed"), nil)
	if err != nil || got.ID != a.ID {
		t.Fatalf("the session must now be bound to the fast account: %v %v", got, err)
	}
}

func TestCodexSpeedNotReadyUsesOriginalSelection(t *testing.T) {
	m, tr, _, _ := speedSelectionManager(t, 5)
	if tr.selecting() {
		t.Fatal("five scored accounts are not enough")
	}
	if _, _, err := m.pickCodexPool(context.Background(), "pool-model", exec.Options{}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCodexSpeedConcurrentRecomputeAndObserve(t *testing.T) {
	m, tr, _, ids := speedSelectionManager(t, 24)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				id := ids[(g*7+i)%len(ids)]
				if a, ok := m.GetByID(id); ok {
					if lease, okLease := m.accountConcurrency.tryAcquire(a); okLease && lease != nil {
						lease.release()
					}
				}
				tr.ObserveAttempt(CodexSpeedAttempt{AuthID: id, Success: true, Gap: time.Second, InputTokens: 10, Group: CodexSpeedGroupFast})
				if _, _, err := m.pickCodexPool(context.Background(), "pool-model", exec.Options{}, nil); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	for i := 0; i < 50; i++ {
		infos := m.CodexSpeedAuthInfos("pool-model")
		for j := range infos {
			infos[j].Schedulable = true
		}
		tr.Recompute(infos)
		_ = tr.Snapshot()
	}
	wg.Wait()
}

// An active session keeps its replacement mark as long as its binding: after more than one TTL
// of steady use, the account turning slow does not trigger a second replacement.
func TestCodexSpeedReplacementMarkFollowsActiveSession(t *testing.T) {
	m, tr, clock, ids := speedSelectionManager(t, 24)
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	opts := sessionOpts("long")
	key, affinity := m.poolSessionKey("pool-model", opts)
	slow, _ := m.GetByID(ids[23])
	if _, _, err := m.bindPoolSelection(context.Background(), key, "pool-model", opts, affinity, slow, nil, nil); err != nil {
		t.Fatal(err)
	}
	first, _, err := m.pickCodexPool(context.Background(), "pool-model", sessionOpts("long"), nil)
	if err != nil || !tr.sessions.rebound(key, clock.Now()) {
		t.Fatalf("setup: first replacement: %v %v", first, err)
	}
	ttl := codexSpeedMarkTTL(affinity)
	for elapsed := time.Duration(0); elapsed < ttl+30*time.Minute; elapsed += 10 * time.Minute {
		clock.Advance(10 * time.Minute)
		if _, _, err = m.pickCodexPool(context.Background(), "pool-model", sessionOpts("long"), nil); err != nil {
			t.Fatal(err)
		}
	}
	tr.demoted.Store(first.ID, clock.Now().Add(time.Hour))
	again, _, err := m.pickCodexPool(context.Background(), "pool-model", sessionOpts("long"), nil)
	if err != nil || again.ID != first.ID {
		t.Fatalf("an active session must not be replaced twice: %v %v", again, err)
	}
	if !tr.sessions.rebound(key, clock.Now()) {
		t.Fatal("the mark must still be alive")
	}
}

// A replacement never leaves a still-usable account of a higher priority layer.
func TestCodexSpeedReplacementKeepsHigherPriorityBinding(t *testing.T) {
	m, tr, clock, ids := speedSelectionManager(t, 24)
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	if _, err := m.Register(context.Background(), &Auth{ID: "prio-slow", Provider: "codex", Attributes: map[string]string{"priority": "1"}}); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient("prio-slow", "codex", []*registry.ModelInfo{{ID: "pool-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("prio-slow") })
	m.ReconcileRegistryModelStates(context.Background(), "prio-slow")
	feed(tr, "prio-slow", 3, 60*time.Second)
	tr.Recompute(infosOf(append(ids, "prio-slow")...))
	if tr.inFast("prio-slow", clock.Now()) {
		t.Fatal("setup: the priority-1 account must be slow")
	}
	opts := sessionOpts("prio")
	key, affinity := m.poolSessionKey("pool-model", opts)
	bound, _ := m.GetByID("prio-slow")
	if _, _, err := m.bindPoolSelection(context.Background(), key, "pool-model", opts, affinity, bound, nil, nil); err != nil {
		t.Fatal(err)
	}
	a, _, err := m.pickCodexPool(context.Background(), "pool-model", sessionOpts("prio"), nil)
	if err != nil || a.ID != "prio-slow" || tr.sessions.rebound(key, clock.Now()) {
		t.Fatalf("the only priority-1 account must keep the session: %v %v", a, err)
	}
}

// A mixed-provider session reuses its binding through preview plus bindPoolSelection; that
// reuse keeps the replacement mark alive even when no request ends in success.
func TestCodexSpeedMixedReuseKeepsReplacementMark(t *testing.T) {
	m, tr, clock, ids := speedSelectionManager(t, 24)
	m.SetSelector(NewSessionAffinitySelector(&RoundRobinSelector{}))
	opts := sessionOpts("mixed-long")
	key, affinity := m.poolSessionKey("pool-model", opts)
	slow, _ := m.GetByID(ids[23])
	if _, _, err := m.bindPoolSelection(context.Background(), key, "pool-model", opts, affinity, slow, nil, nil); err != nil {
		t.Fatal(err)
	}
	preview := context.WithValue(context.Background(), poolPreviewKey{}, true)
	settle := func() *Auth {
		t.Helper()
		pooled := sessionOpts("mixed-long")
		a, _, err := m.pickCodexPool(preview, "pool-model", pooled, nil)
		if err != nil {
			t.Fatal(err)
		}
		var replace *codexPoolBinding
		if b, ok := pooled.Metadata[poolSpeedReplaceMetadataKey].(codexPoolBinding); ok {
			replace = &b
		}
		got, _, err := m.bindPoolSelection(context.Background(), key, "pool-model", pooled, affinity, a, nil, replace)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := settle()
	if !tr.sessions.rebound(key, clock.Now()) || !tr.inFast(first.ID, clock.Now()) {
		t.Fatal("setup: the first settle must replace the slow binding")
	}
	for elapsed := time.Duration(0); elapsed < codexSpeedMarkTTL(affinity)+30*time.Minute; elapsed += 10 * time.Minute {
		clock.Advance(10 * time.Minute)
		settle()
	}
	tr.demoted.Store(first.ID, clock.Now().Add(time.Hour))
	if again := settle(); again.ID != first.ID {
		t.Fatalf("an active mixed session must not be replaced twice: %s", again.ID)
	}
}
