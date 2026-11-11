package cliproxy

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexSpeedProbeRoundSkipsRejectedHeadOfQueue(t *testing.T) {
	tracker := coreauth.NewCodexSpeedTracker(nil)
	var infos []coreauth.CodexSpeedAuthInfo
	for i := 0; i < 10; i++ {
		infos = append(infos, coreauth.CodexSpeedAuthInfo{ID: fmt.Sprintf("a%02d", i), Schedulable: true, QuotaUsed: float64(i)})
	}
	var launched []string
	svc := &codexSpeedService{tracker: tracker}
	svc.acquire = func(id, model string) *coreauth.Auth {
		if id < "a05" { // the five accounts at the head of the queue are always rejected
			return nil
		}
		return &coreauth.Auth{ID: id}
	}
	svc.launch = func(ctx context.Context, auth *coreauth.Auth, model string) {
		launched = append(launched, auth.ID)
		svc.inflight.Add(-1)
	}
	if n := svc.probeRound(context.Background(), infos, 2, 32, "m"); n != 2 || len(launched) != 2 || launched[0] != "a05" || launched[1] != "a06" {
		t.Fatalf("rejected accounts must not block later candidates: started=%d launched=%v", n, launched)
	}
	// Rejected accounts keep their probe interval: they are still candidates next second.
	if got := tracker.ProbeCandidates(infos, 10); len(got) == 0 || got[0] != "a00" {
		t.Fatalf("rejection must not spend the interval: %v", got)
	}
}

type speedSimClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *speedSimClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *speedSimClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// speedSim drives the real budget, candidate, reservation and in-flight path of the prober on
// a virtual clock; a launched probe finishes after probeTime with the outcome fail decides.
type speedSim struct {
	clock     *speedSimClock
	tracker   *coreauth.CodexSpeedTracker
	svc       *codexSpeedService
	cfg       internalconfig.CodexSpeedPriorityConfig
	infos     []coreauth.CodexSpeedAuthInfo
	probeTime time.Duration
	fail      func(id string) bool
	pending   map[int64][]string
	started   map[string]int
	second    int64
}

func newSpeedSim(accounts int, probeTime time.Duration, mutate func(*internalconfig.CodexSpeedPriorityConfig)) *speedSim {
	sim := &speedSim{clock: &speedSimClock{now: time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)}, probeTime: probeTime,
		pending: map[int64][]string{}, started: map[string]int{}, fail: func(string) bool { return false }}
	sim.cfg = internalconfig.CodexSpeedPriorityConfig{Enable: true}
	if mutate != nil {
		mutate(&sim.cfg)
	}
	sim.cfg.Normalize()
	sim.tracker = coreauth.NewCodexSpeedTracker(sim.clock.Now)
	sim.tracker.SetConfig(sim.cfg, true, true)
	for i := 0; i < accounts; i++ {
		sim.infos = append(sim.infos, coreauth.CodexSpeedAuthInfo{ID: fmt.Sprintf("acct-%05d", i), Schedulable: true, QuotaUsed: -1})
	}
	sim.svc = &codexSpeedService{tracker: sim.tracker, infos: sim.infos}
	sim.svc.acquire = func(id, model string) *coreauth.Auth { return &coreauth.Auth{ID: id} }
	sim.svc.launch = func(ctx context.Context, auth *coreauth.Auth, model string) {
		due := sim.second + int64(sim.probeTime/time.Second)
		sim.pending[due] = append(sim.pending[due], auth.ID)
		sim.started[auth.ID]++
	}
	return sim
}

// run advances the simulation one second at a time, recomputing the ranking every minute.
func (sim *speedSim) run(d time.Duration) {
	for end := sim.second + int64(d/time.Second); sim.second < end; {
		sim.second++
		sim.clock.Advance(time.Second)
		for _, id := range sim.pending[sim.second] {
			res := coreauth.CodexSpeedProbeResult{AuthID: id, Duration: sim.probeTime}
			if sim.fail(id) {
				res.Overload = true
			} else {
				res.Success, res.Gap = true, 2*time.Second
			}
			sim.tracker.ObserveProbe(res)
			sim.svc.inflight.Add(-1)
		}
		delete(sim.pending, sim.second)
		sim.svc.probeStep(context.Background(), time.Second, sim.cfg)
		if sim.second%60 == 0 {
			sim.tracker.Recompute(sim.infos)
		}
	}
}

// Acceptance: 23,000 accounts after a cold start, default settings, 10 second probes, every
// probe failing independently 30% of the time, no real traffic. Two hours later at least 80%
// of the accounts have a score.
func TestCodexSpeedColdStartCoversLargePool(t *testing.T) {
	sim := newSpeedSim(23000, 10*time.Second, nil)
	rng := rand.New(rand.NewSource(1))
	sim.fail = func(string) bool { return rng.Intn(10) < 3 }
	sim.run(2 * time.Hour)
	snap := sim.tracker.Snapshot()
	if snap.Total != 23000 || snap.Scored < 23000*80/100 {
		t.Fatalf("scored %d of %d after two hours, want at least 80%%", snap.Scored, snap.Total)
	}
	if snap.ProbeBehind {
		t.Fatalf("10 second probes must keep up: round estimate %.1f min", snap.ProbeCycleMinutes)
	}
	probes := 0
	for _, n := range sim.started {
		probes += n
	}
	// The budget is 1.7% of 23,000 a minute; fast-group accounts are not probed, so the second
	// round uses less than that, but the first round must reach every account.
	if budget := 23000 * 1.7 / 100 * 120; float64(probes) > budget+10 || len(sim.started) != 23000 {
		t.Fatalf("started %d probes on %d accounts, budget %.0f", probes, len(sim.started), budget)
	}
	t.Logf("scored %d of %d after two hours with %d probes", snap.Scored, snap.Total, probes)
}

// Accounts that always fail are retried once a round, so they do not take the budget of the
// accounts that can be measured.
func TestCodexSpeedFailingAccountsDoNotStarveTheRest(t *testing.T) {
	sim := newSpeedSim(100, 10*time.Second, func(c *internalconfig.CodexSpeedPriorityConfig) { c.Probe.PercentPerMinute = 10 })
	sim.fail = func(id string) bool { return id < "acct-00030" }
	sim.run(25 * time.Minute)
	snap := sim.tracker.Snapshot()
	if snap.Scored != 70 {
		t.Fatalf("scored %d accounts, want the 70 that answer", snap.Scored)
	}
	for id, n := range sim.started {
		if n > 3 {
			t.Fatalf("%s was probed %d times in 25 minutes at a 10 minute round", id, n)
		}
	}
}

// A small pool earns less than one probe a second: the budget accumulates instead of rounding
// to zero or to one a second, and a stall never turns into a burst.
func TestCodexSpeedProbeBudgetIsFractional(t *testing.T) {
	sim := newSpeedSim(600, time.Second, nil) // 600 x 1.7% = 10.2 probes a minute
	sim.run(10 * time.Minute)
	probes := 0
	for _, n := range sim.started {
		probes += n
	}
	if probes < 100 || probes > 103 {
		t.Fatalf("started %d probes in ten minutes, want about 102", probes)
	}
	if got := sim.svc.probeStep(context.Background(), time.Hour, sim.cfg); got > 1 {
		t.Fatalf("an hour-long stall released %d probes at once", got)
	}
	empty := newSpeedSim(0, time.Second, nil)
	empty.run(time.Minute)
	if len(empty.started) != 0 || empty.svc.probeTokens != 0 {
		t.Fatalf("an empty pool must not probe or save budget: %v %v", empty.started, empty.svc.probeTokens)
	}
}

// The in-flight limit follows the rate: slow probes stop new starts at the limit.
func TestCodexSpeedProbeInFlightLimitHolds(t *testing.T) {
	sim := newSpeedSim(23000, 10*time.Minute, nil)
	sim.run(5 * time.Minute)
	limit := coreauth.CodexSpeedProbeInFlightLimit(coreauth.CodexSpeedProbeRate(23000, sim.cfg))
	if got := int(sim.svc.inflight.Load()); got != limit {
		t.Fatalf("in flight %d, want the limit %d", got, limit)
	}
}
