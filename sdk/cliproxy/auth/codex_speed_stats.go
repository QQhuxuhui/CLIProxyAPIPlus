package auth

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexspeed"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// codexSpeedHourAcc accumulates one hour. Attempts are attributed to the hour they finished in;
// account counts and utilization are summed per sampled minute and averaged on close.
type codexSpeedHourAcc struct {
	rec                       codexspeed.SpeedHour
	fastAcc, slowAcc, unscAcc float64
	fastUtil, slowUtil        utilAvg
}

func (a *codexSpeedHourAcc) finalize() codexspeed.SpeedHour {
	rec := a.rec
	rec.Schema = codexspeed.SpeedSchemaRanked
	rec.Fast.Hist = slices.Clone(rec.Fast.Hist)
	rec.Slow.Hist = slices.Clone(rec.Slow.Hist)
	rec.Overall.Hist = slices.Clone(rec.Overall.Hist)
	if rec.Minutes > 0 {
		n := float64(rec.Minutes)
		rec.FastAccounts, rec.SlowAccounts, rec.UnscoredAccounts = a.fastAcc/n, a.slowAcc/n, a.unscAcc/n
	}
	rec.FastUtil, rec.SlowUtil = a.fastUtil.value(), a.slowUtil.value()
	rec.FinalizeGroups()
	return rec
}

func (a *codexSpeedHourAcc) hasData() bool {
	r := &a.rec
	return r.Minutes > 0 || r.Overall.Attempts > 0 || r.Probes > 0 || r.InstantDemotions > 0
}

// codexSpeedStats keeps the open hour and the finished hours waiting to be persisted.
type codexSpeedStats struct {
	mu      sync.Mutex
	open    codexSpeedHourAcc
	pending []codexspeed.SpeedHour
}

func (s *codexSpeedStats) init(now time.Time) {
	s.open = codexSpeedHourAcc{rec: codexspeed.SpeedHour{Hour: now.Truncate(time.Hour)}}
}

// rollLocked seals the open hour when now is in a later hour.
func (s *codexSpeedStats) rollLocked(now time.Time) {
	hour := now.Truncate(time.Hour)
	if !hour.After(s.open.rec.Hour) {
		return
	}
	if s.open.hasData() {
		s.pending = append(s.pending, s.open.finalize())
	}
	s.open = codexSpeedHourAcc{rec: codexspeed.SpeedHour{Hour: hour}}
}

func addAttempt(g *codexspeed.SpeedGroupHour, at CodexSpeedAttempt) {
	g.Attempts++
	if at.Overload {
		g.Overloads++
	} else if at.Success && at.TTFT > 0 && at.InputTokens < codexSpeedSmallInputTokens {
		g.Hist.Add(at.TTFT)
	}
}

// observeAttempt counts a finished attempt under the group recorded when it started.
func (s *codexSpeedStats) observeAttempt(at CodexSpeedAttempt, now time.Time) {
	if at.Group == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollLocked(now)
	addAttempt(&s.open.rec.Overall, at)
	switch at.Group {
	case CodexSpeedGroupFast:
		addAttempt(&s.open.rec.Fast, at)
	case CodexSpeedGroupSlow:
		addAttempt(&s.open.rec.Slow, at)
	}
}

func (s *codexSpeedStats) countProbe(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollLocked(now)
	s.open.rec.Probes++
}

func (s *codexSpeedStats) countDemotion(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollLocked(now)
	s.open.rec.InstantDemotions++
}

// sampleMinute adds one minute of group sizes, utilization and membership changes.
func (s *codexSpeedStats) sampleMinute(now time.Time, cfg internalconfig.CodexSpeedPriorityConfig, active bool, view *codexSpeedView, toSlow, toFast int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollLocked(now)
	r := &s.open.rec
	r.Enabled = r.Enabled || active
	r.FastPercent = cfg.FastPercent
	r.Minutes++
	if active && view.ready {
		r.ReadyMinutes++
	}
	r.FastToSlow += toSlow
	r.SlowToFast += toFast
	s.open.fastAcc += float64(view.fastCount)
	s.open.slowAcc += float64(view.slowCount)
	s.open.unscAcc += float64(view.unscored)
	if view.fastUtil != nil {
		s.open.fastUtil.add(*view.fastUtil)
	}
	if view.slowUtil != nil {
		s.open.slowUtil.add(*view.slowUtil)
	}
}

// roll returns every finished hour, oldest first.
func (s *codexSpeedStats) roll(now time.Time) []codexspeed.SpeedHour {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollLocked(now)
	out := s.pending
	s.pending = nil
	return out
}

// current returns the open hour so far.
func (s *codexSpeedStats) current(now time.Time) codexspeed.SpeedHour {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollLocked(now)
	return s.open.finalize()
}

// RollHours returns the finished hour records, oldest first.
func (t *CodexSpeedTracker) RollHours() []codexspeed.SpeedHour {
	if t == nil {
		return nil
	}
	return t.stats.roll(t.now())
}

// Tick runs the minute maintenance that does not depend on account infos.
func (t *CodexSpeedTracker) Tick() {
	if t == nil {
		return
	}
	t.sessions.prune(t.now())
}

// CodexSpeedSnapshot is the live state shown by the management API.
type CodexSpeedSnapshot struct {
	Enabled       bool     `json:"enabled"`
	Active        bool     `json:"active"`
	Inactive      string   `json:"inactive,omitempty"`
	Ready         bool     `json:"ready"`
	Scored        int      `json:"scored"`
	ReadyMin      int      `json:"ready_min"`
	Total         int      `json:"total"`
	Fast          int      `json:"fast"`
	Slow          int      `json:"slow"`
	Unscored      int      `json:"unscored"`
	FastPercent   int      `json:"fast_percent"`
	FastUtil      *float64 `json:"fast_util"`
	SlowUtil      *float64 `json:"slow_util"`
	OverflowShare *float64 `json:"overflow_share"`
	FastP50Ms     *int64   `json:"fast_p50_ms"`
	SlowP50Ms     *int64   `json:"slow_p50_ms"`
	OverallP50Ms  *int64   `json:"overall_p50_ms"`
	Probes        int64    `json:"probes"`
	// ProbePercentPerMinute is the configured share of accounts probed each minute,
	// ProbeRoundMinutes the round it implies and ProbeRatePerSecond the resulting target rate.
	ProbePercentPerMinute float64 `json:"probe_percent_per_minute"`
	ProbeRoundMinutes     float64 `json:"probe_round_minutes"`
	ProbeRatePerSecond    float64 `json:"probe_rate_per_second"`
	// ProbeCycleMinutes is the estimated time of one round at the effective rate.
	ProbeCycleMinutes float64         `json:"probe_cycle_minutes"`
	ProbeBehind       bool            `json:"probe_behind"`
	Bins              []CodexSpeedBin `json:"bins"`
	// FastUsedShare and SlowUsedShare are the shares of fast members and of scored non-fast
	// accounts used in the last minute; nil unless live account use was supplied.
	FastUsedShare *float64             `json:"fast_used_share"`
	SlowUsedShare *float64             `json:"slow_used_share"`
	Current       codexspeed.SpeedHour `json:"current"`
}

// Snapshot returns the live view including the unfinished hour (histograms stripped).
func (t *CodexSpeedTracker) Snapshot() CodexSpeedSnapshot {
	return t.snapshotFrom(t.view.Load())
}

// SnapshotWithUse is Snapshot plus live per-bin use. It loads the view once, so bin counts and
// bin members always come from the same recompute; the shared view is never modified.
func (t *CodexSpeedTracker) SnapshotWithUse(use map[string]CodexAccountUse) CodexSpeedSnapshot {
	view := t.view.Load()
	snap := t.snapshotFrom(view)
	if use == nil || len(view.binIDs) != len(snap.Bins) {
		return snap
	}
	bins := make([]CodexSpeedBin, len(snap.Bins))
	copy(bins, snap.Bins)
	for i, ids := range view.binIDs {
		for _, id := range ids {
			u := use[id]
			bins[i].Requests += u.Requests
			if u.used() {
				bins[i].Used++
			}
		}
		if n := len(ids); n > 0 {
			share := float64(bins[i].Used) / float64(n)
			bins[i].UsedShare = &share
		}
	}
	snap.Bins = bins
	var fast, slow utilAvg
	for id := range view.fast {
		fast.add(use[id].share())
	}
	for id := range view.score {
		if !view.fast[id] {
			slow.add(use[id].share())
		}
	}
	snap.FastUsedShare, snap.SlowUsedShare = fast.value(), slow.value()
	return snap
}

func (t *CodexSpeedTracker) snapshotFrom(view *codexSpeedView) CodexSpeedSnapshot {
	cfg := t.config()
	cur := t.stats.current(t.now())
	bins := view.bins
	if bins == nil {
		bins = make([]CodexSpeedBin, codexSpeedBinCount)
	}
	round := cfg.Probe.Round()
	return CodexSpeedSnapshot{
		Enabled: cfg.Enable, Active: t.Active(), Inactive: t.InactiveReason(), Ready: view.ready,
		Scored: view.scored, ReadyMin: codexSpeedReadyMinScored, Total: view.total,
		Fast: view.fastCount, Slow: view.slowCount, Unscored: view.unscored, FastPercent: cfg.FastPercent,
		FastUtil: view.fastUtil, SlowUtil: view.slowUtil, OverflowShare: cur.OverflowShare,
		FastP50Ms: cur.Fast.P50Ms, SlowP50Ms: cur.Slow.P50Ms, OverallP50Ms: cur.Overall.P50Ms,
		Probes: cur.Probes, ProbePercentPerMinute: cfg.Probe.PercentPerMinute,
		ProbeRoundMinutes: round.Minutes(), ProbeRatePerSecond: view.probeRate,
		// A round estimate more than 5% over the target means probing cannot keep up.
		ProbeCycleMinutes: view.probeCycle.Minutes(), ProbeBehind: t.Active() && view.probeCycle > round+round/20,
		Bins: bins, Current: cur.StripHist(),
	}
}

// SummaryLine is the minute log line; values are in the message because the file formatter
// drops structured fields.
func (t *CodexSpeedTracker) SummaryLine() string {
	s := t.Snapshot()
	ms := func(v *int64) int64 {
		if v == nil {
			return -1
		}
		return *v
	}
	f := func(v *float64) float64 {
		if v == nil {
			return -1
		}
		return *v
	}
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	return fmt.Sprintf("codex speed summary: enabled=%d active=%d ready=%d total=%d fast=%d slow=%d unscored=%d fast_percent=%d fast_util=%.3f overflow_share=%.3f fast_p50=%d slow_p50=%d overall_p50=%d demotions=%d probes=%d probe_cycle_min=%.1f",
		b2i(s.Enabled), b2i(s.Active), b2i(s.Ready), s.Total, s.Fast, s.Slow, s.Unscored, s.FastPercent, f(s.FastUtil), f(s.OverflowShare),
		ms(s.FastP50Ms), ms(s.SlowP50Ms), ms(s.OverallP50Ms), s.Current.InstantDemotions, s.Probes, s.ProbeCycleMinutes)
}
