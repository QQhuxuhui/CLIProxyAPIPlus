package helps

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// fakeCodexFastSessionUpstream answers probes from fixed per-value outcomes. Every probe pays
// connect before response.created; the server-side wait is 500ms for fast values and
// baseline otherwise, unless timing overrides the value.
type fakeCodexFastSessionUpstream struct {
	mu       sync.Mutex
	next     int
	fast     map[string]bool
	fail     map[string]bool
	timing   map[string]CodexFastSessionProbeTiming
	connect  time.Duration
	baseline time.Duration
	probed   []string
	noAuth   string
}

func newFakeCodexFastSessionUpstream() *fakeCodexFastSessionUpstream {
	return &fakeCodexFastSessionUpstream{
		fast:     map[string]bool{},
		fail:     map[string]bool{},
		timing:   map[string]CodexFastSessionProbeTiming{},
		connect:  1500 * time.Millisecond,
		baseline: 5 * time.Second,
	}
}

func (f *fakeCodexFastSessionUpstream) deps(cfg *config.CodexFastSessionIDsConfig) CodexFastSessionProbeDeps {
	now := time.Date(2026, 10, 25, 18, 0, 0, 0, time.UTC)
	return CodexFastSessionProbeDeps{
		Config:   func() config.CodexFastSessionIDsConfig { return *cfg },
		HomeMode: func() bool { return false },
		AcquireAuth: func(string) (*cliproxyauth.Auth, string) {
			if f.noAuth != "" {
				return nil, f.noAuth
			}
			return &cliproxyauth.Auth{ID: "probe-auth"}, ""
		},
		Probe: func(_ context.Context, _ *cliproxyauth.Auth, _ string, sessionID string) (CodexFastSessionProbeTiming, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.probed = append(f.probed, sessionID)
			if timing, ok := f.timing[sessionID]; ok {
				return timing, nil
			}
			gap := f.baseline
			switch {
			case f.fail[sessionID]:
				return CodexFastSessionProbeTiming{}, errors.New("status 429")
			case f.fast[sessionID]:
				gap = 500 * time.Millisecond
			}
			return CodexFastSessionProbeTiming{Sent: f.connect, Created: f.connect, First: f.connect + gap}, nil
		},
		NewID: func() string {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.next++
			return fmt.Sprintf("id-%d", f.next)
		},
		Now: func() time.Time { return now },
	}
}

func codexFastSessionProberConfig(seed ...string) *config.CodexFastSessionIDsConfig {
	cfg := &config.CodexFastSessionIDsConfig{Enable: true, Percent: 10, Seed: seed, ProbeIntervalSeconds: 600, ProbeCandidates: 2}
	cfg.Normalize()
	return cfg
}

func newTestCodexFastSessionProber(cfg *config.CodexFastSessionIDsConfig, upstream *fakeCodexFastSessionUpstream) (*CodexFastSessionPool, *CodexFastSessionProber) {
	pool := NewCodexFastSessionPool()
	pool.ApplyConfig(*cfg)
	return pool, NewCodexFastSessionProber(pool, upstream.deps(cfg))
}

func TestCodexFastSessionProberAdmitsAfterConsecutivePasses(t *testing.T) {
	cfg := codexFastSessionProberConfig()
	upstream := newFakeCodexFastSessionUpstream()
	// Round 1: id-1..5 baseline, id-6 and id-7 candidates. Round 2: id-8..12 baseline,
	// pending id-6, candidates id-13 and id-14.
	upstream.fast["id-6"] = true
	pool, prober := newTestCodexFastSessionProber(cfg, upstream)

	prober.RunRound(context.Background(), *cfg)
	if got := pool.Members(); len(got) != 0 {
		t.Fatalf("admitted after one pass: %v", got)
	}
	status := prober.Status()
	if !status.LastRound.Judged || len(status.Pending) != 1 || status.Pending[0] != (CodexFastSessionPending{Value: "id-6", Passes: 1}) {
		t.Fatalf("status after round 1 = %+v", status)
	}
	if r := status.LastRound.Results[5]; r.Value != "id-6" || r.CreatedMs != 1500 || r.FirstMs != 2000 || r.GapMs != 500 || r.Verdict != codexFastSessionVerdictPass {
		t.Fatalf("candidate result = %+v", r)
	}
	if status.LastRound.BaselineGapMs != 5000 {
		t.Fatalf("baseline gap = %d, want 5000", status.LastRound.BaselineGapMs)
	}

	prober.RunRound(context.Background(), *cfg)
	if got := pool.Members(); !slices.Equal(got, []string{"id-6"}) {
		t.Fatalf("members = %v, want [id-6]", got)
	}
	snap := pool.Snapshot(10)
	if len(snap.Events) != 1 || snap.Events[0].Action != "admit" || snap.Events[0].Value != "id-6" {
		t.Fatalf("events = %+v", snap.Events)
	}
	if len(prober.Status().Pending) != 0 {
		t.Fatalf("pending not cleared: %+v", prober.Status().Pending)
	}
}

func TestCodexFastSessionProberJudgesServerSideGap(t *testing.T) {
	cfg := codexFastSessionProberConfig()
	upstream := newFakeCodexFastSessionUpstream()
	upstream.connect = 100 * time.Millisecond
	// id-6 pays a slow 3s connect but the server answers quickly: by wall time it is
	// 3.5s vs 5.1s (miss), by server-side gap it is 0.5s vs 5s (pass).
	upstream.timing["id-6"] = CodexFastSessionProbeTiming{Sent: 3 * time.Second, Created: 3 * time.Second, First: 3500 * time.Millisecond}
	// id-7 has no response.created event: the gap falls back to the first-delta time.
	upstream.timing["id-7"] = CodexFastSessionProbeTiming{First: 4 * time.Second}
	_, prober := newTestCodexFastSessionProber(cfg, upstream)
	prober.RunRound(context.Background(), *cfg)
	status := prober.Status()
	if len(status.Pending) != 1 || status.Pending[0].Value != "id-6" {
		t.Fatalf("pending = %+v", status.Pending)
	}
	if r := status.LastRound.Results[6]; r.Value != "id-7" || r.GapMs != 4000 || r.Verdict != codexFastSessionVerdictMiss {
		t.Fatalf("fallback result = %+v", r)
	}
}

func TestCodexFastSessionProberCountsQueueingBeforeCreated(t *testing.T) {
	cfg := codexFastSessionProberConfig()
	upstream := newFakeCodexFastSessionUpstream()
	upstream.fast["id-6"] = true
	upstream.fast["id-7"] = true
	// id-7 is sent at once but upstream holds response.created for 6s and then streams the
	// first delta immediately: the created->first gap is ~0, the real wait is ~6s.
	upstream.timing["id-7"] = CodexFastSessionProbeTiming{Sent: 1500 * time.Millisecond, Created: 7500 * time.Millisecond, First: 7500 * time.Millisecond}
	_, prober := newTestCodexFastSessionProber(cfg, upstream)
	prober.RunRound(context.Background(), *cfg)
	status := prober.Status()
	if len(status.Pending) != 1 || status.Pending[0].Value != "id-6" {
		t.Fatalf("pending = %+v", status.Pending)
	}
	if r := status.LastRound.Results[6]; r.Value != "id-7" || r.GapMs != 0 || r.WaitMs != 6000 || r.Verdict != codexFastSessionVerdictMiss {
		t.Fatalf("queued result = %+v", r)
	}
}

func TestCodexFastSessionProberFastBaselineBecomesCandidate(t *testing.T) {
	cfg := codexFastSessionProberConfig()
	upstream := newFakeCodexFastSessionUpstream()
	upstream.fast["id-2"] = true // a random baseline value that happens to be fast
	upstream.timing["id-4"] = CodexFastSessionProbeTiming{Sent: time.Second, Created: time.Second, First: 29 * time.Second}
	_, prober := newTestCodexFastSessionProber(cfg, upstream)
	prober.RunRound(context.Background(), *cfg)
	status := prober.Status()
	// The median of 0.5s, 5s, 5s, 5s, 28s stays at 5s.
	if !status.LastRound.Judged || status.LastRound.BaselineGapMs != 5000 {
		t.Fatalf("round = %+v", status.LastRound)
	}
	if len(status.Pending) != 1 || status.Pending[0] != (CodexFastSessionPending{Value: "id-2", Passes: 1}) {
		t.Fatalf("pending = %+v", status.Pending)
	}
	if r := status.LastRound.Results[1]; r.Kind != codexFastSessionProbeBaseline || r.Verdict != codexFastSessionVerdictPass {
		t.Fatalf("fast baseline result = %+v", r)
	}
	if r := status.LastRound.Results[3]; r.Verdict != "" {
		t.Fatalf("slow baseline must have no verdict: %+v", r)
	}
}

func TestCodexFastSessionProberRemovesAfterConsecutiveMissesIgnoringFailures(t *testing.T) {
	cfg := codexFastSessionProberConfig("slow", "good")
	cfg.PoolSize = 2 // full: no new candidates
	upstream := newFakeCodexFastSessionUpstream()
	upstream.fast["good"] = true
	pool, prober := newTestCodexFastSessionProber(cfg, upstream)

	prober.RunRound(context.Background(), *cfg) // miss 1
	upstream.fail["slow"] = true
	prober.RunRound(context.Background(), *cfg) // failure: neither pass nor miss
	if got := pool.Members(); !slices.Equal(got, []string{"slow", "good"}) {
		t.Fatalf("members after miss+failure = %v", got)
	}
	if status := prober.Status(); status.ProbeFailures != 1 || status.LastRound.Results[5].Verdict != codexFastSessionVerdictFailed {
		t.Fatalf("status = %+v", status)
	}
	delete(upstream.fail, "slow")
	prober.RunRound(context.Background(), *cfg) // miss 2
	if got := pool.Members(); !slices.Equal(got, []string{"good"}) {
		t.Fatalf("members = %v, want [good]", got)
	}
	snap := pool.Snapshot(10)
	if len(snap.Events) != 1 || snap.Events[0].Action != "remove" || snap.Events[0].Value != "slow" {
		t.Fatalf("events = %+v", snap.Events)
	}
	// A full pool generates no new candidates: 5 baseline + 2 members per round.
	if len(upstream.probed) != 21 {
		t.Fatalf("probes = %d (%v), want 21", len(upstream.probed), upstream.probed)
	}
}

func TestCodexFastSessionProberDoesNotJudgeBelowThresholdAndKeepsLastJudgedRound(t *testing.T) {
	cfg := codexFastSessionProberConfig("slow")
	upstream := newFakeCodexFastSessionUpstream()
	pool, prober := newTestCodexFastSessionProber(cfg, upstream)
	prober.RunRound(context.Background(), *cfg) // peak: judged, slow misses once

	upstream.baseline = 2 * time.Second // off-peak: below the 2500ms threshold
	upstream.fast["id-13"] = true
	for i := 0; i < 3; i++ {
		prober.RunRound(context.Background(), *cfg)
	}
	if got := pool.Members(); !slices.Equal(got, []string{"slow"}) {
		t.Fatalf("members = %v, want [slow]", got)
	}
	status := prober.Status()
	if status.LastRound.Judged || !strings.Contains(status.LastRound.Reason, "below threshold") || len(status.Pending) != 0 || status.Rounds != 4 {
		t.Fatalf("status = %+v", status)
	}
	if !status.LastJudgedRound.Judged || status.LastJudgedRound.BaselineGapMs != 5000 || len(status.LastJudgedRound.Results) == 0 {
		t.Fatalf("last judged round = %+v", status.LastJudgedRound)
	}
}

func TestCodexFastSessionProberRespectsPoolSize(t *testing.T) {
	cfg := codexFastSessionProberConfig("m1")
	cfg.PoolSize = 2
	cfg.ProbeCandidates = 3
	upstream := newFakeCodexFastSessionUpstream()
	upstream.fast["m1"] = true
	upstream.fast["id-6"] = true
	upstream.fast["id-7"] = true
	pool, prober := newTestCodexFastSessionProber(cfg, upstream)
	prober.RunRound(context.Background(), *cfg)
	prober.RunRound(context.Background(), *cfg)
	if got := pool.Members(); len(got) != 2 || got[0] != "m1" {
		t.Fatalf("members = %v, want m1 plus one admitted candidate", got)
	}
	// Discard surplus candidates once the pool is full.
	if status := prober.Status(); len(status.Pending) != 0 {
		t.Fatalf("pending = %+v", status.Pending)
	}
}

func TestCodexFastSessionProberSkipsRoundWithoutAuth(t *testing.T) {
	cfg := codexFastSessionProberConfig("m1")
	upstream := newFakeCodexFastSessionUpstream()
	upstream.noAuth = "all codex oauth credentials at rate window limit"
	pool, prober := newTestCodexFastSessionProber(cfg, upstream)
	prober.RunRound(context.Background(), *cfg)
	status := prober.Status()
	if status.Rounds != 1 || status.LastRound.Judged || !strings.Contains(status.LastRound.Reason, upstream.noAuth) || len(upstream.probed) != 0 {
		t.Fatalf("status = %+v probed = %v", status, upstream.probed)
	}
	if got := pool.Members(); !slices.Equal(got, []string{"m1"}) {
		t.Fatalf("members = %v", got)
	}
}

func TestCodexFastSessionProberSkipsTickWhileRoundRunning(t *testing.T) {
	cfg := codexFastSessionProberConfig()
	upstream := newFakeCodexFastSessionUpstream()
	deps := upstream.deps(cfg)
	now := time.Date(2026, 10, 25, 18, 0, 0, 0, time.UTC)
	deps.Now = func() time.Time { return now }
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	probe := deps.Probe
	deps.Probe = func(ctx context.Context, auth *cliproxyauth.Auth, model, sessionID string) (CodexFastSessionProbeTiming, error) {
		once.Do(func() {
			close(entered)
			<-release
		})
		return probe(ctx, auth, model, sessionID)
	}
	pool := NewCodexFastSessionPool()
	prober := NewCodexFastSessionProber(pool, deps)

	prober.Tick(context.Background())
	<-entered
	now = now.Add(time.Hour)
	prober.Tick(context.Background())
	if status := prober.Status(); !status.Running || status.SkippedTicks != 1 || status.LastSkip != "previous round still running" {
		close(release)
		prober.Wait()
		t.Fatalf("status = %+v", status)
	}
	close(release)
	prober.Wait()
	if status := prober.Status(); status.Running || status.Rounds != 1 {
		t.Fatalf("status after release = %+v", status)
	}

	// The skipped tick did not consume the interval: the next tick starts a round...
	prober.Tick(context.Background())
	prober.Wait()
	// ...and a tick before the interval elapses again does nothing.
	now = now.Add(time.Minute)
	prober.Tick(context.Background())
	prober.Wait()
	if status := prober.Status(); status.Rounds != 2 || status.SkippedTicks != 1 {
		t.Fatalf("status after early tick = %+v", status)
	}
}

func TestCodexFastSessionProberIdleWhenIntervalZero(t *testing.T) {
	cfg := codexFastSessionProberConfig()
	cfg.ProbeIntervalSeconds = 0
	upstream := newFakeCodexFastSessionUpstream()
	_, prober := newTestCodexFastSessionProber(cfg, upstream)
	prober.Tick(context.Background())
	prober.Wait()
	if status := prober.Status(); status.Rounds != 0 || len(upstream.probed) != 0 {
		t.Fatalf("status = %+v", status)
	}
}

func codexFastSessionRollbackPool(t *testing.T) (*CodexFastSessionPool, config.CodexFastSessionIDsConfig) {
	t.Helper()
	pool := NewCodexFastSessionPool()
	now := time.Date(2026, 10, 25, 18, 0, 30, 0, time.UTC)
	pool.nowFunc = func() time.Time { return now }
	cfg := config.CodexFastSessionIDsConfig{Enable: true, Percent: 100, Seed: []string{"fast-a"}, Rollback: config.CodexFastSessionRollbackConfig{Enable: true}}
	cfg.Normalize()
	pool.ApplyConfig(cfg)
	return pool, cfg
}

func observeN(pool *CodexFastSessionPool, group string, n int, failures int, ttft time.Duration) {
	for i := 0; i < n; i++ {
		pool.Observe(group, i < failures, ttft, 100)
	}
}

func TestCodexFastSessionRollbackTriggersOnSlowFastGroupAndReenablesOnConfigChange(t *testing.T) {
	pool, cfg := codexFastSessionRollbackPool(t)
	observeN(pool, CodexFastSessionGroupFast, 30, 0, 2*time.Second)
	observeN(pool, CodexFastSessionGroupControl, 30, 0, time.Second)
	pool.EvaluateRollback(cfg.Rollback)

	snap := pool.Snapshot(10)
	if !snap.Rollback.Disabled || !strings.Contains(snap.Rollback.Reason, "ttft") {
		t.Fatalf("rollback = %+v", snap.Rollback)
	}
	if _, group := pool.Select("conv"); group != CodexFastSessionGroupOff {
		t.Fatalf("group after rollback = %q, want off", group)
	}
	// Same config keeps it disabled.
	pool.ApplyConfig(cfg)
	if _, group := pool.Select("conv"); group != CodexFastSessionGroupOff {
		t.Fatalf("group after same config = %q, want off", group)
	}
	// Changing percent re-enables.
	cfg.Percent = 99
	pool.ApplyConfig(cfg)
	snap = pool.Snapshot(10)
	if snap.Rollback.Disabled || snap.Rollback.ReenabledAt.IsZero() {
		t.Fatalf("rollback after config change = %+v", snap.Rollback)
	}
	if got := snap.Events; len(got) != 2 || got[0].Action != "disable" || got[1].Action != "reenable" {
		t.Fatalf("events = %+v", got)
	}
	if _, group := pool.Select("conv"); group == CodexFastSessionGroupOff {
		t.Fatal("mapping still off after config change")
	}
}

func TestCodexFastSessionRollbackTriggersOnFailureRate(t *testing.T) {
	pool, cfg := codexFastSessionRollbackPool(t)
	observeN(pool, CodexFastSessionGroupFast, 30, 3, time.Second) // 10% failures
	observeN(pool, CodexFastSessionGroupControl, 30, 0, time.Second)
	pool.EvaluateRollback(cfg.Rollback)
	if snap := pool.Snapshot(10); !snap.Rollback.Disabled || !strings.Contains(snap.Rollback.Reason, "failure rate") {
		t.Fatalf("rollback = %+v", snap.Rollback)
	}
}

func TestCodexFastSessionRollbackNeedsMinSamplesAndMargin(t *testing.T) {
	pool, cfg := codexFastSessionRollbackPool(t)
	observeN(pool, CodexFastSessionGroupFast, 29, 10, 5*time.Second)
	observeN(pool, CodexFastSessionGroupControl, 30, 0, time.Second)
	pool.EvaluateRollback(cfg.Rollback)
	if snap := pool.Snapshot(10); snap.Rollback.Disabled || snap.RollbackCheck.Outcome != "insufficient samples" {
		t.Fatalf("rollback with 29 samples = %+v / %+v", snap.Rollback, snap.RollbackCheck)
	}

	pool, cfg = codexFastSessionRollbackPool(t)
	observeN(pool, CodexFastSessionGroupFast, 31, 1, 1100*time.Millisecond) // within both margins
	observeN(pool, CodexFastSessionGroupControl, 30, 0, time.Second)
	pool.EvaluateRollback(cfg.Rollback)
	if snap := pool.Snapshot(10); snap.Rollback.Disabled || snap.RollbackCheck.Outcome != "ok" {
		t.Fatalf("rollback within margin = %+v / %+v", snap.Rollback, snap.RollbackCheck)
	}

	cfg.Rollback.Enable = false
	observeN(pool, CodexFastSessionGroupFast, 30, 30, time.Second)
	pool.EvaluateRollback(cfg.Rollback)
	if snap := pool.Snapshot(10); snap.Rollback.Disabled {
		t.Fatal("rollback triggered while disabled in config")
	}
}

func TestCodexFastSessionProberBoundsPendingWhenFull(t *testing.T) {
	cfg := codexFastSessionProberConfig("member")
	cfg.PoolSize = 1
	upstream := newFakeCodexFastSessionUpstream()
	upstream.fast["member"] = true
	for i := 1; i <= 125; i += 5 {
		upstream.fast[fmt.Sprintf("id-%d", i)] = true
	}
	_, prober := newTestCodexFastSessionProber(cfg, upstream)
	for i := 0; i < 25; i++ {
		prober.RunRound(context.Background(), *cfg)
	}
	status := prober.Status()
	if len(status.Pending) > cfg.PoolSize || len(status.LastRound.Results) > cfg.ProbeBaselines+cfg.PoolSize*2+cfg.ProbeCandidates {
		t.Fatalf("full pool pending=%d probes=%d", len(status.Pending), len(status.LastRound.Results))
	}
}

func TestCodexFastSessionProberReplacesFailedPendingCandidate(t *testing.T) {
	cfg := codexFastSessionProberConfig()
	cfg.PoolSize = 1
	cfg.ProbeCandidates = 1
	upstream := newFakeCodexFastSessionUpstream()
	upstream.fast["id-6"] = true
	upstream.fast["id-12"] = true
	pool, prober := newTestCodexFastSessionProber(cfg, upstream)
	prober.RunRound(context.Background(), *cfg)
	upstream.fail["id-6"] = true
	prober.RunRound(context.Background(), *cfg)
	prober.RunRound(context.Background(), *cfg)
	if got := pool.Members(); !slices.Equal(got, []string{"id-12"}) {
		t.Fatalf("failed pending candidate blocked healthy replacement: members=%v pending=%v", got, prober.Status().Pending)
	}
	if prober.Status().ProbeFailures != 1 {
		t.Fatal("failed candidate must still be recorded as a probe failure")
	}
}

func TestCodexFastSessionPoolIgnoresStaleConfigOnceProberAttached(t *testing.T) {
	current := codexFastSessionProberConfig("a", "b", "c")
	stale := codexFastSessionProberConfig("a", "b")
	upstream := newFakeCodexFastSessionUpstream()
	pool, prober := newTestCodexFastSessionProber(current, upstream)
	pool.Add("d")

	// Without a prober, request-path configs are applied.
	pool.ApplyUnmanagedConfig(*stale)
	if got := pool.Members(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("unmanaged members = %v", got)
	}

	pool.ApplyConfig(*current)
	pool.Add("d")
	pool.prober.Store(prober)
	// An executor still holding the previous config must not reset the pool.
	pool.ApplyUnmanagedConfig(*stale)
	if got := pool.Members(); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("managed members = %v, want runtime member kept", got)
	}
}

func TestCodexFastSessionProberDiscardsRoundWhenSeedChanges(t *testing.T) {
	cfg := codexFastSessionProberConfig("member")
	cfg.Consecutive = 1
	upstream := newFakeCodexFastSessionUpstream()
	upstream.fast["id-6"] = true // a passing candidate; "member" is slow and would be missed
	pool, prober := newTestCodexFastSessionProber(cfg, upstream)
	next := codexFastSessionProberConfig("member", "new-seed")
	probe := prober.deps.Probe
	swapped := false
	prober.deps.Probe = func(ctx context.Context, auth *cliproxyauth.Auth, model, sessionID string) (CodexFastSessionProbeTiming, error) {
		if !swapped {
			swapped = true
			pool.ApplyConfig(*next) // hot reload while the round is running
		}
		return probe(ctx, auth, model, sessionID)
	}
	prober.RunRound(context.Background(), *cfg)
	if got := pool.Members(); !slices.Equal(got, []string{"member", "new-seed"}) {
		t.Fatalf("members = %v, want the new seed untouched", got)
	}
	status := prober.Status()
	if status.LastRound.Judged || status.LastRound.Reason != "pool seed changed during the round" || len(status.Pending) != 0 {
		t.Fatalf("round = %+v pending = %+v", status.LastRound, status.Pending)
	}
}

func TestCodexFastSessionProberResetsMissesOnSeedChange(t *testing.T) {
	cfg := codexFastSessionProberConfig("slow")
	cfg.PoolSize = 1
	upstream := newFakeCodexFastSessionUpstream()
	pool, prober := newTestCodexFastSessionProber(cfg, upstream)
	prober.RunRound(context.Background(), *cfg) // first miss for "slow"

	next := codexFastSessionProberConfig("slow", "other")
	next.PoolSize = 2
	pool.ApplyConfig(*next)
	upstream.fast["other"] = true
	prober.RunRound(context.Background(), *next) // a miss under the new seed is the first one
	if got := pool.Members(); !slices.Equal(got, []string{"slow", "other"}) {
		t.Fatalf("members = %v, old-seed miss must not count", got)
	}
	prober.RunRound(context.Background(), *next)
	if got := pool.Members(); !slices.Equal(got, []string{"other"}) {
		t.Fatalf("members = %v, want slow removed after two misses under the new seed", got)
	}
}
