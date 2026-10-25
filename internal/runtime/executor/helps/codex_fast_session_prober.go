package helps

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Probe target kinds.
const (
	codexFastSessionProbeBaseline  = "baseline"
	codexFastSessionProbeMember    = "member"
	codexFastSessionProbeCandidate = "candidate"
)

// Probe verdicts of a judged round.
const (
	codexFastSessionVerdictPass   = "pass"
	codexFastSessionVerdictMiss   = "miss"
	codexFastSessionVerdictFailed = "failed"
)

// CodexFastSessionProbeTiming holds the probe timings measured from sending the request.
type CodexFastSessionProbeTiming struct {
	// Created is the time to the first response.created event (0 when none was seen).
	Created time.Duration
	// First is the time to the first response.output_text.delta.
	First time.Duration
}

// Gap returns the server-side wait between response.created and the first output delta,
// which excludes connect/TLS time. It falls back to First when no created event was seen.
func (t CodexFastSessionProbeTiming) Gap() time.Duration {
	if t.Created > 0 && t.First >= t.Created {
		return t.First - t.Created
	}
	return t.First
}

// CodexFastSessionProbeDeps wires the prober to the runtime. Now and NewID are optional.
type CodexFastSessionProbeDeps struct {
	// Config returns the current codex.fast-session-ids configuration.
	Config func() config.CodexFastSessionIDsConfig
	// HomeMode reports whether Home owns scheduling; probing is skipped then.
	HomeMode func() bool
	// AcquireAuth returns a healthy codex credential with one attempt already counted in its
	// rate window, or nil and the reason none is available.
	AcquireAuth func(model string) (*cliproxyauth.Auth, string)
	// Probe sends one tiny streaming request with sessionID as the upstream Session-Id and
	// returns the time to response.created and to the first output text delta.
	Probe func(ctx context.Context, auth *cliproxyauth.Auth, model, sessionID string) (CodexFastSessionProbeTiming, error)
	// NewID returns a fresh random Session-Id candidate.
	NewID func() string
	// Now returns the current time.
	Now func() time.Time
}

// CodexFastSessionProbeResult is one probe of a round. Judging uses GapMs.
type CodexFastSessionProbeResult struct {
	Kind      string `json:"kind"`
	Value     string `json:"value"`
	CreatedMs int64  `json:"created_ms,omitempty"`
	FirstMs   int64  `json:"first_ms,omitempty"`
	GapMs     int64  `json:"gap_ms,omitempty"`
	Error     string `json:"error,omitempty"`
	Verdict   string `json:"verdict,omitempty"`
}

// CodexFastSessionPending is a candidate that passed judged rounds but is not admitted yet.
type CodexFastSessionPending struct {
	Value  string `json:"value"`
	Passes int    `json:"passes"`
}

// CodexFastSessionProbeRound is the outcome of one probe round.
type CodexFastSessionProbeRound struct {
	At            time.Time                     `json:"at,omitzero"`
	DurationMs    int64                         `json:"duration_ms"`
	Judged        bool                          `json:"judged"`
	Reason        string                        `json:"reason,omitempty"`
	BaselineGapMs int64                         `json:"baseline_gap_p50_ms"`
	Results       []CodexFastSessionProbeResult `json:"results"`
}

// CodexFastSessionProbeStatus reports the prober state.
type CodexFastSessionProbeStatus struct {
	Running         bool                       `json:"running"`
	Rounds          int64                      `json:"rounds"`
	SkippedTicks    int64                      `json:"skipped_ticks"`
	ProbeFailures   int64                      `json:"probe_failures"`
	LastSkip        string                     `json:"last_skip,omitempty"`
	LastSkipAt      time.Time                  `json:"last_skip_at,omitzero"`
	LastRound       CodexFastSessionProbeRound `json:"last_round"`
	LastJudgedRound CodexFastSessionProbeRound `json:"last_judged_round"`
	Pending         []CodexFastSessionPending  `json:"pending"`
}

// CodexFastSessionProber discovers fast Session-Id values and removes slow members. Probes
// run sequentially without timeouts; a tick that finds the previous round still running is
// skipped, and only ctx cancellation (service shutdown) stops a round early.
type CodexFastSessionProber struct {
	pool    *CodexFastSessionPool
	deps    CodexFastSessionProbeDeps
	running atomic.Bool
	wg      sync.WaitGroup

	mu             sync.Mutex
	lastRoundStart time.Time
	pending        map[string]int
	misses         map[string]int
	status         CodexFastSessionProbeStatus
}

// NewCodexFastSessionProber returns a prober for pool.
func NewCodexFastSessionProber(pool *CodexFastSessionPool, deps CodexFastSessionProbeDeps) *CodexFastSessionProber {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.NewID == nil {
		deps.NewID = uuid.NewString
	}
	return &CodexFastSessionProber{pool: pool, deps: deps, pending: map[string]int{}, misses: map[string]int{}}
}

// StartCodexFastSessionProber attaches a prober to the process-wide pool and drives it once a
// minute until ctx is done. Each tick also evaluates the rollback from in-memory stats.
func StartCodexFastSessionProber(ctx context.Context, deps CodexFastSessionProbeDeps) *CodexFastSessionProber {
	prober := NewCodexFastSessionProber(CodexFastSessions(), deps)
	CodexFastSessions().prober.Store(prober)
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				prober.Tick(ctx)
			}
		}
	}()
	return prober
}

// Tick syncs the pool with the config, evaluates the rollback, and starts a probe round in
// the background when one is due and the previous round has finished.
func (p *CodexFastSessionProber) Tick(ctx context.Context) {
	if p == nil || p.deps.Config == nil {
		return
	}
	cfg := p.deps.Config()
	cfg.Normalize()
	p.pool.ApplyConfig(cfg)
	p.pool.EvaluateRollback(cfg.Rollback)
	if !cfg.Enable || cfg.ProbeIntervalSeconds <= 0 || p.deps.Probe == nil || p.deps.AcquireAuth == nil {
		return
	}
	if p.deps.HomeMode != nil && p.deps.HomeMode() {
		p.skip("home mode")
		return
	}
	now := p.deps.Now()
	p.mu.Lock()
	due := p.lastRoundStart.IsZero() || now.Sub(p.lastRoundStart) >= time.Duration(cfg.ProbeIntervalSeconds)*time.Second
	p.mu.Unlock()
	if !due {
		return
	}
	if !p.running.CompareAndSwap(false, true) {
		p.skip("previous round still running")
		return
	}
	p.mu.Lock()
	p.lastRoundStart = now
	p.mu.Unlock()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer p.running.Store(false)
		p.RunRound(ctx, cfg)
	}()
}

// Wait blocks until background rounds started by Tick have finished.
func (p *CodexFastSessionProber) Wait() {
	if p != nil {
		p.wg.Wait()
	}
}

func (p *CodexFastSessionProber) skip(reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status.SkippedTicks++
	p.status.LastSkip = reason
	p.status.LastSkipAt = p.deps.Now()
}

type codexFastSessionProbeTarget struct {
	kind  string
	value string
}

// RunRound probes baseline values, members, and candidates once and judges the results.
func (p *CodexFastSessionProber) RunRound(ctx context.Context, cfg config.CodexFastSessionIDsConfig) {
	if p == nil {
		return
	}
	cfg.Normalize()
	start := p.deps.Now()
	members := p.pool.Members()
	p.mu.Lock()
	pending := make([]string, 0, len(p.pending))
	for value := range p.pending {
		if !slices.Contains(members, value) {
			pending = append(pending, value)
		}
	}
	p.mu.Unlock()
	slices.Sort(pending)

	targets := make([]codexFastSessionProbeTarget, 0, cfg.ProbeBaselines+len(members)+len(pending)+cfg.ProbeCandidates)
	for i := 0; i < cfg.ProbeBaselines; i++ {
		targets = append(targets, codexFastSessionProbeTarget{codexFastSessionProbeBaseline, p.deps.NewID()})
	}
	for _, value := range members {
		targets = append(targets, codexFastSessionProbeTarget{codexFastSessionProbeMember, value})
	}
	for _, value := range pending {
		targets = append(targets, codexFastSessionProbeTarget{codexFastSessionProbeCandidate, value})
	}
	if len(members) < cfg.PoolSize {
		for i := 0; i < cfg.ProbeCandidates; i++ {
			targets = append(targets, codexFastSessionProbeTarget{codexFastSessionProbeCandidate, p.deps.NewID()})
		}
	}

	results := make([]CodexFastSessionProbeResult, 0, len(targets))
	gaps := make([]time.Duration, 0, len(targets))
	for i, target := range targets {
		if ctx.Err() != nil {
			return
		}
		result := CodexFastSessionProbeResult{Kind: target.kind, Value: target.value}
		auth, reason := p.deps.AcquireAuth(cfg.ProbeModel)
		if auth == nil {
			if i == 0 {
				p.finishSkippedRound(start, "no codex credential available: "+reason)
				return
			}
			result.Error = "no codex credential available: " + reason
			results = append(results, result)
			gaps = append(gaps, 0)
			continue
		}
		timing, err := p.deps.Probe(ctx, auth, cfg.ProbeModel, target.value)
		if ctx.Err() != nil {
			return
		}
		gap := timing.Gap()
		if err == nil && timing.First <= 0 {
			err = fmt.Errorf("no first token")
		}
		if err != nil {
			result.Error = err.Error()
			gap = 0
		} else {
			result.CreatedMs = timing.Created.Milliseconds()
			result.FirstMs = timing.First.Milliseconds()
			result.GapMs = gap.Milliseconds()
		}
		results = append(results, result)
		gaps = append(gaps, gap)
	}
	p.judgeRound(cfg, start, results, gaps)
}

func (p *CodexFastSessionProber) finishSkippedRound(start time.Time, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status.Rounds++
	p.status.LastRound = CodexFastSessionProbeRound{
		At:         start,
		DurationMs: p.deps.Now().Sub(start).Milliseconds(),
		Reason:     reason,
		Results:    []CodexFastSessionProbeResult{},
	}
	log.WithField("reason", reason).Info("codex fast session probe round skipped")
}

func (p *CodexFastSessionProber) judgeRound(cfg config.CodexFastSessionIDsConfig, start time.Time, results []CodexFastSessionProbeResult, gaps []time.Duration) {
	var baselineIdx []int
	failures := int64(0)
	for i, result := range results {
		if result.Error != "" {
			failures++
			continue
		}
		if result.Kind == codexFastSessionProbeBaseline {
			baselineIdx = append(baselineIdx, i)
		}
	}
	baseline := make([]time.Duration, 0, len(baselineIdx))
	for _, i := range baselineIdx {
		baseline = append(baseline, gaps[i])
	}
	baselineP50 := codexFastSessionMedian(baseline)
	judged := false
	reason := ""
	switch {
	case len(baseline) == 0:
		reason = "no successful baseline probe"
	case baselineP50 < time.Duration(cfg.BaselineThresholdMs)*time.Millisecond:
		reason = fmt.Sprintf("baseline gap p50 %dms below threshold %dms", baselineP50.Milliseconds(), cfg.BaselineThresholdMs)
	default:
		judged = true
		reason = fmt.Sprintf("judged against baseline gap p50 %dms", baselineP50.Milliseconds())
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if judged {
		limit := time.Duration(float64(baselineP50) * cfg.AdmitRatio)
		// A random baseline value that turns out fast is a free candidate.
		for _, i := range baselineIdx {
			others := make([]time.Duration, 0, len(baselineIdx)-1)
			for _, j := range baselineIdx {
				if j != i {
					others = append(others, gaps[j])
				}
			}
			if len(others) >= 2 && gaps[i] < time.Duration(float64(codexFastSessionMedian(others))*cfg.AdmitRatio) {
				results[i].Verdict = codexFastSessionVerdictPass
				p.judgeCandidateLocked(cfg, results[i].Value, true, gaps[i], baselineP50)
			}
		}
		for i := range results {
			result := &results[i]
			if result.Kind == codexFastSessionProbeBaseline {
				continue
			}
			if result.Error != "" {
				result.Verdict = codexFastSessionVerdictFailed
				continue
			}
			pass := gaps[i] < limit
			if pass {
				result.Verdict = codexFastSessionVerdictPass
			} else {
				result.Verdict = codexFastSessionVerdictMiss
			}
			switch result.Kind {
			case codexFastSessionProbeMember:
				p.judgeMemberLocked(cfg, result.Value, pass, gaps[i], baselineP50)
			case codexFastSessionProbeCandidate:
				p.judgeCandidateLocked(cfg, result.Value, pass, gaps[i], baselineP50)
			}
		}
		members := p.pool.Members()
		for value := range p.misses {
			if !slices.Contains(members, value) {
				delete(p.misses, value)
			}
		}
	}

	round := CodexFastSessionProbeRound{
		At:            start,
		DurationMs:    p.deps.Now().Sub(start).Milliseconds(),
		Judged:        judged,
		Reason:        reason,
		BaselineGapMs: baselineP50.Milliseconds(),
		Results:       results,
	}
	p.status.Rounds++
	p.status.ProbeFailures += failures
	p.status.LastRound = round
	if judged {
		p.status.LastJudgedRound = round
	}
	log.WithFields(log.Fields{"judged": judged, "reason": reason, "probes": len(results), "failures": failures}).Info("codex fast session probe round finished")
}

func (p *CodexFastSessionProber) judgeMemberLocked(cfg config.CodexFastSessionIDsConfig, value string, pass bool, gap, baseline time.Duration) {
	if pass {
		delete(p.misses, value)
		return
	}
	p.misses[value]++
	if p.misses[value] < cfg.Consecutive {
		return
	}
	detail := fmt.Sprintf("missed %d judged rounds (last gap %dms vs baseline %dms)", p.misses[value], gap.Milliseconds(), baseline.Milliseconds())
	if p.pool.removeWithDetail(value, detail) {
		log.WithFields(log.Fields{"session": shortCodexFastSessionID(value), "detail": detail}).Info("codex fast session member removed")
	}
	delete(p.misses, value)
}

func (p *CodexFastSessionProber) judgeCandidateLocked(cfg config.CodexFastSessionIDsConfig, value string, pass bool, gap, baseline time.Duration) {
	if !pass {
		delete(p.pending, value)
		return
	}
	p.pending[value]++
	if p.pending[value] < cfg.Consecutive {
		return
	}
	detail := fmt.Sprintf("passed %d judged rounds (last gap %dms vs baseline %dms)", p.pending[value], gap.Milliseconds(), baseline.Milliseconds())
	if p.pool.addWithLimit(value, cfg.PoolSize, detail) {
		log.WithFields(log.Fields{"session": shortCodexFastSessionID(value), "detail": detail}).Info("codex fast session member admitted")
		delete(p.pending, value)
	}
}

// Status returns a copy of the prober state.
func (p *CodexFastSessionProber) Status() CodexFastSessionProbeStatus {
	if p == nil {
		return CodexFastSessionProbeStatus{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.status
	out.Running = p.running.Load()
	out.LastRound.Results = cloneCodexFastSessionResults(p.status.LastRound.Results)
	out.LastJudgedRound.Results = cloneCodexFastSessionResults(p.status.LastJudgedRound.Results)
	out.Pending = make([]CodexFastSessionPending, 0, len(p.pending))
	for value, passes := range p.pending {
		out.Pending = append(out.Pending, CodexFastSessionPending{Value: value, Passes: passes})
	}
	slices.SortFunc(out.Pending, func(a, b CodexFastSessionPending) int {
		if a.Value < b.Value {
			return -1
		}
		if a.Value > b.Value {
			return 1
		}
		return 0
	})
	return out
}

func cloneCodexFastSessionResults(results []CodexFastSessionProbeResult) []CodexFastSessionProbeResult {
	if results == nil {
		return []CodexFastSessionProbeResult{}
	}
	return slices.Clone(results)
}

// codexFastSessionMedian returns the median of samples (mean of the two middle values for an
// even count).
func codexFastSessionMedian(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

func shortCodexFastSessionID(value string) string {
	if len(value) > 8 {
		return value[:8]
	}
	return value
}
