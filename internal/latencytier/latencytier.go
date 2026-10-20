// Package latencytier ranks codex OAuth credentials by observed time-to-first-token
// and steers CPA's priority-tiered selection toward the fast ones.
//
// The package never touches the selector itself. It subscribes to usage records,
// keeps a decayed first-token average per credential, recomputes relative tiers
// on a fixed cadence, and only rewrites the in-memory "priority" attribute
// (fast=2, normal=1, slow=0). Switching the feature off restores the priorities
// that were in place before it was enabled.
package latencytier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// Tier is the relative speed class of a credential.
type Tier string

const (
	TierFast   Tier = "fast"
	TierNormal Tier = "normal"
	TierSlow   Tier = "slow"
)

const (
	recentOutcomeWindow = 10
	recentMinutesWindow = 10
)

// Config holds the runtime-tunable parameters. All percentages are fractions in (0,1).
type Config struct {
	Enabled bool `json:"enabled"`
	// FastEnterPercent: a credential enters the fast tier when its rank (0 = fastest)
	// is within this fraction of eligible credentials.
	FastEnterPercent float64 `json:"fast_enter_percent"`
	// FastExitPercent: a fast credential stays fast until its rank falls beyond this fraction.
	FastExitPercent float64 `json:"fast_exit_percent"`
	// SlowEnterPercent: a credential enters the slow tier when it is within the slowest fraction.
	SlowEnterPercent float64 `json:"slow_enter_percent"`
	// SlowExitPercent: a slow credential leaves the slow tier once it is outside the slowest fraction.
	SlowExitPercent float64 `json:"slow_exit_percent"`
	// HalfLifeSeconds controls the time decay of the first-token average.
	HalfLifeSeconds int `json:"half_life_seconds"`
	// MinSamples is the number of samples needed before a credential is ranked.
	MinSamples int `json:"min_samples"`
	// InactiveMinutes: a credential without samples for this long drops back to normal.
	InactiveMinutes int `json:"inactive_minutes"`
	// ProbationPercent of the fast tier size is temporarily promoted each cycle for exploration.
	ProbationPercent float64 `json:"probation_percent"`
	// ProbationMin is the minimum number of probation slots per cycle.
	ProbationMin int `json:"probation_min"`
	// MinEligiblePercent gates priority rewrites: they only happen while at least this fraction of
	// the tracked pool is rankable, so a handful of measured credentials never absorbs all traffic.
	MinEligiblePercent float64 `json:"min_eligible_percent"`
	// PenaltyMs is the sample recorded for a failed request.
	PenaltyMs int `json:"penalty_ms"`
	// OverloadPenaltyMs is the latency sample recorded for an upstream "server overloaded" rejection.
	// Overload hits every credential alike, so by default (0) it is counted as a failure but does not
	// touch the first-token average; the ranking then reflects how fast the credential answers when
	// it does answer.
	OverloadPenaltyMs int `json:"overload_penalty_ms"`
	// TrustPlus treats never-measured plus-plan credentials as fast until they are ranked.
	TrustPlus bool `json:"trust_plus"`
	// RecomputeSeconds is the tier recomputation cadence.
	RecomputeSeconds int `json:"recompute_seconds"`
	FastPriority     int `json:"fast_priority"`
	NormalPriority   int `json:"normal_priority"`
	SlowPriority     int `json:"slow_priority"`
}

// DefaultConfig returns the parameters agreed for the first rollout.
func DefaultConfig() Config {
	return Config{
		Enabled:            false,
		FastEnterPercent:   0.20,
		FastExitPercent:    0.30,
		SlowEnterPercent:   0.40,
		SlowExitPercent:    0.50,
		HalfLifeSeconds:    180,
		MinSamples:         5,
		InactiveMinutes:    30,
		ProbationPercent:   0.05,
		ProbationMin:       10,
		MinEligiblePercent: 0.10,
		PenaltyMs:          30000,
		OverloadPenaltyMs:  0,
		TrustPlus:          true,
		RecomputeSeconds:   60,
		FastPriority:       2,
		NormalPriority:     1,
		SlowPriority:       0,
	}
}

// Validate rejects parameter combinations that cannot produce a stable ranking.
func (c Config) Validate() error {
	frac := func(name string, v float64) error {
		if v <= 0 || v >= 1 {
			return errors.New(name + " must be between 0 and 1")
		}
		return nil
	}
	if err := frac("fast_enter_percent", c.FastEnterPercent); err != nil {
		return err
	}
	if err := frac("fast_exit_percent", c.FastExitPercent); err != nil {
		return err
	}
	if err := frac("slow_enter_percent", c.SlowEnterPercent); err != nil {
		return err
	}
	if err := frac("slow_exit_percent", c.SlowExitPercent); err != nil {
		return err
	}
	if c.FastExitPercent < c.FastEnterPercent {
		return errors.New("fast_exit_percent must be >= fast_enter_percent")
	}
	if c.SlowExitPercent < c.SlowEnterPercent {
		return errors.New("slow_exit_percent must be >= slow_enter_percent")
	}
	if c.FastExitPercent+c.SlowExitPercent > 1 {
		return errors.New("fast_exit_percent + slow_exit_percent must not exceed 1")
	}
	if c.ProbationPercent < 0 || c.ProbationPercent >= 1 {
		return errors.New("probation_percent must be between 0 and 1")
	}
	if c.MinEligiblePercent < 0 || c.MinEligiblePercent >= 1 {
		return errors.New("min_eligible_percent must be between 0 and 1")
	}
	if c.HalfLifeSeconds <= 0 || c.MinSamples <= 0 || c.InactiveMinutes <= 0 || c.PenaltyMs <= 0 || c.RecomputeSeconds <= 0 || c.ProbationMin < 0 || c.OverloadPenaltyMs < 0 {
		return errors.New("half_life_seconds, min_samples, inactive_minutes, penalty_ms, recompute_seconds must be positive and probation_min/overload_penalty_ms non-negative")
	}
	if !(c.FastPriority > c.NormalPriority && c.NormalPriority > c.SlowPriority) {
		return errors.New("priorities must satisfy fast > normal > slow")
	}
	return nil
}

// AuthAccess is the slice of the auth manager the tracker depends on.
type AuthAccess interface {
	List() []*coreauth.Auth
	SetAuthPriorityInMemory(id string, priority int) bool
	AuthPriorityInMemory(id string) (int, bool)
	InvalidateSessionAffinity(authID string)
	InFlight(id string) int
}

type accountStat struct {
	AuthID     string    `json:"auth_id"`
	EWMA       float64   `json:"ewma_ms"`
	Weight     float64   `json:"weight"`
	EWMAAt     time.Time `json:"ewma_at"`
	Samples    int64     `json:"samples"`
	Failures   int64     `json:"failures"`
	LastSample time.Time `json:"last_sample"`
	// LastProbation records when the credential was last promoted for exploration.
	LastProbation time.Time `json:"last_probation"`
	// Recent holds the last recentOutcomeWindow outcomes, true = failure, newest last.
	Recent []bool `json:"recent"`
	// Minutes maps unix-minute to request count for the rolling 10 minute window.
	Minutes map[int64]int `json:"minutes"`
}

func (s *accountStat) recentFailures() int {
	n := 0
	for _, f := range s.Recent {
		if f {
			n++
		}
	}
	return n
}

func (s *accountStat) recentCount(now time.Time) int {
	cut := now.Unix()/60 - recentMinutesWindow
	n := 0
	for m, c := range s.Minutes {
		if m > cut {
			n += c
		}
	}
	return n
}

func (s *accountStat) observe(now time.Time, ms float64, failed bool, halfLife time.Duration) {
	s.observeWith(now, ms, failed, true, halfLife)
}

// observeWith records an outcome; when hasLatency is false only the failure/activity counters move.
func (s *accountStat) observeWith(now time.Time, ms float64, failed, hasLatency bool, halfLife time.Duration) {
	if !hasLatency {
		if failed {
			s.Failures++
		}
		s.LastSample = now
		s.Recent = append(s.Recent, failed)
		if len(s.Recent) > recentOutcomeWindow {
			s.Recent = s.Recent[len(s.Recent)-recentOutcomeWindow:]
		}
		s.touchMinute(now)
		return
	}
	if s.Weight > 0 && !s.EWMAAt.IsZero() {
		dt := now.Sub(s.EWMAAt)
		if dt < 0 {
			dt = 0
		}
		decay := math.Pow(0.5, dt.Seconds()/halfLife.Seconds())
		w := s.Weight * decay
		s.EWMA = (s.EWMA*w + ms) / (w + 1)
		s.Weight = w + 1
	} else {
		s.EWMA = ms
		s.Weight = 1
	}
	s.EWMAAt = now
	s.Samples++
	if failed {
		s.Failures++
	}
	s.LastSample = now
	s.Recent = append(s.Recent, failed)
	if len(s.Recent) > recentOutcomeWindow {
		s.Recent = s.Recent[len(s.Recent)-recentOutcomeWindow:]
	}
	s.touchMinute(now)
}

func (s *accountStat) touchMinute(now time.Time) {
	if s.Minutes == nil {
		s.Minutes = make(map[int64]int)
	}
	minute := now.Unix() / 60
	s.Minutes[minute]++
	cut := minute - recentMinutesWindow
	for m := range s.Minutes {
		if m <= cut {
			delete(s.Minutes, m)
		}
	}
}

// AccountView is the per-credential row exposed to the management API.
type AccountView struct {
	AuthID         string    `json:"auth_id"`
	Label          string    `json:"label"`
	Plan           string    `json:"plan"`
	Tier           Tier      `json:"tier"`
	Probation      bool      `json:"probation"`
	Eligible       bool      `json:"eligible"`
	Rank           int       `json:"rank"`
	Priority       int       `json:"priority"`
	EWMAMs         float64   `json:"ewma_ms"`
	Samples        int64     `json:"samples"`
	Failures       int64     `json:"failures"`
	RecentFailures int       `json:"recent_failures"`
	RecentWindow   int       `json:"recent_window"`
	Recent10m      int       `json:"recent_10m"`
	LastSample     time.Time `json:"last_sample"`
	Status         string    `json:"status"`
	// Active reports whether the credential produced a sample within InactiveMinutes.
	Active bool `json:"active"`
	// InFlight is the number of requests the credential is serving right now.
	InFlight int `json:"in_flight"`
	// Trusted marks a plus credential promoted on trust before it has enough samples.
	Trusted bool `json:"trusted"`
}

// Snapshot is the management API payload.
type Snapshot struct {
	Config      Config        `json:"config"`
	ComputedAt  time.Time     `json:"computed_at"`
	Tracked     int           `json:"tracked"`
	Eligible    int           `json:"eligible"`
	Fast        int           `json:"fast"`
	Normal      int           `json:"normal"`
	Slow        int           `json:"slow"`
	Probation   int           `json:"probation"`
	Applied     int           `json:"applied"`
	Accounts    []AccountView `json:"accounts"`
	Persistence string        `json:"persistence,omitempty"`
	// GateOpen reports whether enough of the pool is rankable for priorities to be rewritten.
	GateOpen        bool    `json:"gate_open"`
	GateReason      string  `json:"gate_reason,omitempty"`
	EligiblePercent float64 `json:"eligible_percent"`
	MinEligible     int     `json:"min_eligible"`
	InFlightTotal   int     `json:"in_flight_total"`
	// Hits10m counts usage records in the last 10 minutes by the tier the credential held at that
	// moment (fast, trusted, probation, normal, slow): the ground truth for where traffic lands.
	Hits10m map[string]int `json:"hits_10m"`
}

type persisted struct {
	Config   Config                  `json:"config"`
	Baseline map[string]int          `json:"baseline"`
	Stats    map[string]*accountStat `json:"stats"`
	SavedAt  time.Time               `json:"saved_at"`
}

// Tracker owns the statistics and the tier assignment.
type Tracker struct {
	mu        sync.Mutex
	cfg       Config
	auths     AuthAccess
	stats     map[string]*accountStat
	tiers     map[string]Tier
	probation map[string]bool
	ranks     map[string]int
	eligible  map[string]bool
	// baseline remembers the priority a credential had before we touched it.
	baseline map[string]int
	// applied remembers the priority we last wrote for a credential.
	applied    map[string]int
	computedAt time.Time
	path       string
	persistErr string
	trusted    map[string]bool
	// hits records, per minute, how many usage records landed on each tier.
	hits map[int64]map[string]int
	// gateOpen is the last recompute's verdict on whether priorities may be rewritten.
	gateOpen    bool
	gateReason  string
	minEligible int
	now         func() time.Time
}

// New builds a tracker. path may be empty to disable persistence.
func New(auths AuthAccess, cfg Config, path string) *Tracker {
	if err := cfg.Validate(); err != nil {
		cfg = DefaultConfig()
	}
	return &Tracker{
		cfg:       cfg,
		auths:     auths,
		stats:     make(map[string]*accountStat),
		tiers:     make(map[string]Tier),
		probation: make(map[string]bool),
		trusted:   make(map[string]bool),
		hits:      make(map[int64]map[string]int),
		ranks:     make(map[string]int),
		eligible:  make(map[string]bool),
		baseline:  make(map[string]int),
		applied:   make(map[string]int),
		path:      path,
		now:       time.Now,
	}
}

// Config returns a copy of the active configuration.
func (t *Tracker) Config() Config {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cfg
}

// SetConfig replaces the configuration. Disabling restores baseline priorities
// immediately; enabling takes effect on the next recompute (or Recompute call).
func (t *Tracker) SetConfig(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	t.mu.Lock()
	wasEnabled := t.cfg.Enabled
	t.cfg = cfg
	if wasEnabled && !cfg.Enabled {
		t.restoreBaselineLocked()
	}
	t.mu.Unlock()
	if cfg.Enabled && !wasEnabled {
		t.Recompute()
	} else {
		t.persist()
	}
	return nil
}

// HandleUsage implements usage.Plugin.
func (t *Tracker) HandleUsage(_ context.Context, rec usage.Record) {
	if !isCodexOAuth(rec.Provider, rec.AuthType) || rec.AuthID == "" || !isTrackedModel(rec.Model, rec.Alias) {
		return
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	ms, failed, hasLatency, ok := sampleFromRecord(rec, t.cfg.PenaltyMs, t.cfg.OverloadPenaltyMs)
	if !ok {
		return
	}
	st := t.stats[rec.AuthID]
	if st == nil {
		st = &accountStat{AuthID: rec.AuthID}
		t.stats[rec.AuthID] = st
	}
	st.observeWith(now, ms, failed, hasLatency, time.Duration(t.cfg.HalfLifeSeconds)*time.Second)
	t.countHitLocked(now, rec.AuthID)
}

func isCodexOAuth(provider, authType string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "codex") && strings.EqualFold(strings.TrimSpace(authType), coreauth.AuthKindOAuth)
}

func isTrackedModel(model, alias string) bool {
	for _, name := range []string{model, alias} {
		name = strings.ToLower(strings.TrimSpace(name))
		if strings.HasPrefix(name, "gpt-6") || strings.HasPrefix(name, "gpt-5.6") {
			return true
		}
	}
	return false
}

// sampleFromRecord converts a usage record into (milliseconds, failed, hasLatency, usable).
// hasLatency is false for pool-wide overload rejections when no overload penalty is configured:
// they count as failures but must not move the first-token average.
func sampleFromRecord(rec usage.Record, penaltyMs, overloadPenaltyMs int) (float64, bool, bool, bool) {
	if rec.Failed {
		if isOverloadFailure(rec.Fail) {
			if overloadPenaltyMs <= 0 {
				return 0, true, false, true
			}
			return float64(overloadPenaltyMs), true, true, true
		}
		return float64(penaltyMs), true, true, true
	}
	if rec.Stream && rec.Detail.OutputTokens == 0 && rec.Detail.ReasoningTokens == 0 && rec.Detail.TotalTokens == 0 {
		// A streamed response that produced nothing is a stream cut after response.created.
		return float64(penaltyMs), true, true, true
	}
	if rec.TTFT > 0 {
		return float64(rec.TTFT.Milliseconds()), false, true, true
	}
	if rec.Latency > 0 {
		return float64(rec.Latency.Milliseconds()), false, true, true
	}
	return 0, false, false, false
}

// isOverloadFailure recognises OpenAI's pool-wide "servers are currently overloaded" rejection,
// which says nothing about the individual credential.
func isOverloadFailure(f usage.Failure) bool {
	if f.StatusCode != 502 && f.StatusCode != 503 && f.StatusCode != 529 {
		return false
	}
	body := strings.ToLower(f.Body)
	return strings.Contains(body, "server_is_overloaded") || strings.Contains(body, "currently overloaded")
}

type ranked struct {
	id   string
	ewma float64
}

// Recompute re-ranks every tracked credential and applies priorities when enabled.
func (t *Tracker) Recompute() {
	now := t.now()
	var auths []*coreauth.Auth
	if t.auths != nil {
		auths = t.auths.List()
	}
	t.mu.Lock()
	known := make(map[string]*coreauth.Auth, len(auths))
	for _, a := range auths {
		if a == nil || !isCodexOAuth(a.Provider, a.AuthKind()) {
			continue
		}
		known[a.ID] = a
		if _, ok := t.stats[a.ID]; !ok {
			t.stats[a.ID] = &accountStat{AuthID: a.ID}
		}
	}
	// Forget credentials that disappeared from the manager (after restoring their priority).
	for id := range t.stats {
		if _, ok := known[id]; !ok && t.auths != nil {
			if base, touched := t.baseline[id]; touched && t.auths != nil {
				t.auths.SetAuthPriorityInMemory(id, base)
			}
			delete(t.stats, id)
			delete(t.tiers, id)
			delete(t.probation, id)
			delete(t.baseline, id)
			delete(t.applied, id)
		}
	}

	inactive := time.Duration(t.cfg.InactiveMinutes) * time.Minute
	active := func(st *accountStat) bool {
		return !st.LastSample.IsZero() && now.Sub(st.LastSample) <= inactive
	}
	// Only recently measured credentials are ranked. A credential measured fast an hour ago
	// would otherwise hold a top rank forever while being inactive (and therefore not fast),
	// squeezing the live fast tier down to a handful of accounts. Stale credentials are
	// re-measured through probation, which prefers the ones that used to be fast.
	eligible := make([]ranked, 0, len(t.stats))
	t.eligible = make(map[string]bool, len(t.stats))
	for id, st := range t.stats {
		if st.Samples >= int64(t.cfg.MinSamples) && active(st) {
			eligible = append(eligible, ranked{id: id, ewma: st.EWMA})
			t.eligible[id] = true
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].ewma == eligible[j].ewma {
			return eligible[i].id < eligible[j].id
		}
		return eligible[i].ewma < eligible[j].ewma
	})

	newTiers := make(map[string]Tier, len(t.stats))
	t.ranks = make(map[string]int, len(eligible))
	n := float64(len(eligible))
	for idx, r := range eligible {
		t.ranks[r.id] = idx + 1
		frac := float64(idx) / n // 0 = fastest
		prev := t.tiers[r.id]
		tier := TierNormal
		switch {
		case prev == TierFast && frac < t.cfg.FastExitPercent:
			tier = TierFast
		case frac < t.cfg.FastEnterPercent:
			tier = TierFast
		case prev == TierSlow && frac >= 1-t.cfg.SlowExitPercent:
			tier = TierSlow
		case frac >= 1-t.cfg.SlowEnterPercent:
			tier = TierSlow
		}
		newTiers[r.id] = tier
	}
	t.trusted = make(map[string]bool)
	for id := range t.stats {
		if _, ok := newTiers[id]; ok {
			continue
		}
		newTiers[id] = TierNormal
		if t.cfg.TrustPlus && t.auths != nil && isPlusPlan(known[id]) && selectable(known[id], now) {
			// Plus credentials are consistently fast: trust them until measured.
			newTiers[id] = TierFast
			t.trusted[id] = true
		}
	}

	// Gate: with only a few rankable credentials "top 20%" degenerates to one or two accounts
	// that would then receive the whole pool's traffic. Priorities stay untouched until the
	// rankable share of the pool reaches MinEligiblePercent.
	t.minEligible = int(math.Ceil(t.cfg.MinEligiblePercent * float64(len(t.stats))))
	if t.minEligible < 1 {
		t.minEligible = 1
	}
	t.gateOpen = len(eligible) >= t.minEligible
	if t.gateOpen {
		t.gateReason = ""
	} else {
		t.gateReason = fmt.Sprintf("可排名 %d/%d 未达门槛 %d（%.0f%%），优先级未改写", len(eligible), len(t.stats), t.minEligible, t.cfg.MinEligiblePercent*100)
	}

	// Probation: temporarily promote the least recently touched non-fast credentials that the
	// selector could actually pick, rotating through the pool so dead accounts are tried once
	// and then pushed to the back of the queue.
	fastCount := 0
	for _, tier := range newTiers {
		if tier == TierFast {
			fastCount++
		}
	}
	k := int(math.Ceil(float64(fastCount) * t.cfg.ProbationPercent))
	if k < t.cfg.ProbationMin {
		k = t.cfg.ProbationMin
	}
	candidates := make([]string, 0, len(newTiers))
	for id, tier := range newTiers {
		if tier == TierFast {
			continue
		}
		if t.auths != nil && !selectable(known[id], now) {
			continue
		}
		candidates = append(candidates, id)
	}
	// Probation order: plus credentials, then stale credentials that measured fast (most likely
	// to be fast again, fastest first), then everything else by how long ago it was last touched.
	fastCutoff := math.Inf(1)
	if len(eligible) > 0 {
		fastCutoff = eligible[int(math.Ceil(float64(len(eligible))*t.cfg.FastEnterPercent))-1].ewma * 1.5
	}
	probeClass := func(id string) int {
		st := t.stats[id]
		if st.Samples >= int64(t.cfg.MinSamples) && !active(st) && st.EWMA <= fastCutoff {
			return 0
		}
		return 1
	}
	sort.Slice(candidates, func(i, j int) bool {
		pi, pj := isPlusPlan(known[candidates[i]]), isPlusPlan(known[candidates[j]])
		if pi != pj {
			return pi
		}
		ci, cj := probeClass(candidates[i]), probeClass(candidates[j])
		if ci != cj {
			return ci < cj
		}
		if ci == 0 && t.stats[candidates[i]].EWMA != t.stats[candidates[j]].EWMA {
			return t.stats[candidates[i]].EWMA < t.stats[candidates[j]].EWMA
		}
		a, b := t.stats[candidates[i]].lastTouch(), t.stats[candidates[j]].lastTouch()
		if a.Equal(b) {
			return candidates[i] < candidates[j]
		}
		return a.Before(b)
	})
	if k > len(candidates) {
		k = len(candidates)
	}
	newProbation := make(map[string]bool, k)
	for _, id := range candidates[:k] {
		newProbation[id] = true
	}

	demoted := make([]string, 0)
	for id, tier := range newTiers {
		if tier == TierSlow && t.tiers[id] != TierSlow {
			demoted = append(demoted, id)
		}
	}
	t.tiers = newTiers
	t.probation = newProbation
	t.computedAt = now
	t.pruneHitsLocked(now)
	lastMin := t.hitsWindowLocked(now, 1)
	p10, p50 := 0.0, 0.0
	if len(eligible) > 0 {
		p10 = eligible[int(float64(len(eligible))*0.1)].ewma / 1000
		p50 = eligible[len(eligible)/2].ewma / 1000
	}
	log.Infof("latency-tier: recompute tracked=%d eligible=%d gate=%v fast=%d trusted=%d probation=%d slow=%d ewma_p10=%.1fs p50=%.1fs hits_last_min fast=%d trusted=%d probation=%d normal=%d slow=%d",
		len(t.stats), len(eligible), t.gateOpen, fastCount-len(t.trusted), len(t.trusted), len(newProbation), countTier(newTiers, TierSlow), p10, p50,
		lastMin["fast"], lastMin["trusted"], lastMin["probation"], lastMin["normal"], lastMin["slow"])

	if t.cfg.Enabled && t.auths != nil {
		if t.gateOpen {
			for id := range newProbation {
				t.stats[id].LastProbation = now
			}
			t.applyLocked()
			for _, id := range demoted {
				t.auths.InvalidateSessionAffinity(id)
			}
		} else if len(t.baseline) > 0 {
			// The pool shrank below the gate: hand the original priorities back.
			t.restoreBaselineLocked()
		}
	}
	t.mu.Unlock()
	t.persist()
}

func countTier(tiers map[string]Tier, want Tier) int {
	n := 0
	for _, tier := range tiers {
		if tier == want {
			n++
		}
	}
	return n
}

// tierLabelLocked names the bucket a credential currently belongs to for hit accounting.
func (t *Tracker) tierLabelLocked(id string) string {
	switch {
	case t.probation[id]:
		return "probation"
	case t.trusted[id]:
		return "trusted"
	case t.tiers[id] == TierFast:
		return "fast"
	case t.tiers[id] == TierSlow:
		return "slow"
	default:
		return "normal"
	}
}

func (t *Tracker) countHitLocked(now time.Time, id string) {
	minute := now.Unix() / 60
	bucket := t.hits[minute]
	if bucket == nil {
		bucket = make(map[string]int)
		t.hits[minute] = bucket
	}
	bucket[t.tierLabelLocked(id)]++
}

func (t *Tracker) pruneHitsLocked(now time.Time) {
	cut := now.Unix()/60 - recentMinutesWindow - 1
	for minute := range t.hits {
		if minute < cut {
			delete(t.hits, minute)
		}
	}
}

// hitsWindowLocked sums the per-tier hits over the last n whole minutes (excluding the current one).
func (t *Tracker) hitsWindowLocked(now time.Time, n int64) map[string]int {
	out := make(map[string]int)
	current := now.Unix() / 60
	for minute, bucket := range t.hits {
		if minute < current && minute >= current-n {
			for label, c := range bucket {
				out[label] += c
			}
		}
	}
	return out
}

func isPlusPlan(a *coreauth.Auth) bool {
	if a == nil {
		return false
	}
	plan := strings.ToLower(strings.TrimSpace(a.Attributes["plan_type"]))
	return plan == "plus" || plan == "pro" || plan == "team"
}

// lastTouch is the later of the last sample and the last probation, used for rotation.
func (st *accountStat) lastTouch() time.Time {
	if st.LastProbation.After(st.LastSample) {
		return st.LastProbation
	}
	return st.LastSample
}

// selectable mirrors the conditions under which the selector would hand a request to the
// credential, so probation slots are not wasted on accounts that cannot serve anyway.
func selectable(a *coreauth.Auth, now time.Time) bool {
	if a == nil || a.Disabled || a.Unavailable {
		return false
	}
	switch a.Status {
	case coreauth.StatusDisabled, coreauth.StatusError:
		return false
	}
	if !a.NextRetryAfter.IsZero() && a.NextRetryAfter.After(now) {
		return false
	}
	if exp := metadataTime(a.Metadata, "expired"); !exp.IsZero() && exp.Before(now) {
		// The access token expired and was never refreshed: the upstream will reject it.
		return false
	}
	for model, state := range a.ModelStates {
		if state == nil || !isTrackedModel(model, "") {
			continue
		}
		if state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now)) {
			return false
		}
	}
	return true
}

func metadataTime(meta map[string]any, key string) time.Time {
	if meta == nil {
		return time.Time{}
	}
	switch v := meta[key].(type) {
	case time.Time:
		return v
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if ts, err := time.Parse(layout, strings.TrimSpace(v)); err == nil {
				return ts
			}
		}
	}
	return time.Time{}
}

func (t *Tracker) desiredPriorityLocked(id string) int {
	if t.probation[id] {
		return t.cfg.FastPriority
	}
	switch t.tiers[id] {
	case TierFast:
		return t.cfg.FastPriority
	case TierSlow:
		return t.cfg.SlowPriority
	default:
		return t.cfg.NormalPriority
	}
}

func (t *Tracker) applyLocked() {
	for id := range t.stats {
		want := t.desiredPriorityLocked(id)
		if _, touched := t.baseline[id]; !touched {
			base, ok := t.auths.AuthPriorityInMemory(id)
			if !ok {
				continue
			}
			t.baseline[id] = base
		}
		// Always compare against the live value: an auth file rewrite (token refresh) replaces the
		// runtime attributes and would silently undo an earlier promotion.
		if current, ok := t.auths.AuthPriorityInMemory(id); ok && current == want {
			t.applied[id] = want
			continue
		}
		t.auths.SetAuthPriorityInMemory(id, want)
		t.applied[id] = want
	}
}

func (t *Tracker) restoreBaselineLocked() {
	if t.auths == nil {
		return
	}
	for id, base := range t.baseline {
		t.auths.SetAuthPriorityInMemory(id, base)
	}
	t.baseline = make(map[string]int)
	t.applied = make(map[string]int)
}

// Snapshot renders the current state for the management API.
func (t *Tracker) Snapshot() Snapshot {
	now := t.now()
	var auths []*coreauth.Auth
	if t.auths != nil {
		auths = t.auths.List()
	}
	meta := make(map[string]*coreauth.Auth, len(auths))
	for _, a := range auths {
		if a != nil {
			meta[a.ID] = a
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	snap := Snapshot{Config: t.cfg, ComputedAt: t.computedAt, Tracked: len(t.stats), Applied: len(t.applied), Persistence: t.persistErr,
		GateOpen: t.gateOpen, GateReason: t.gateReason, MinEligible: t.minEligible, Hits10m: t.hitsWindowLocked(now, recentMinutesWindow)}
	inactive := time.Duration(t.cfg.InactiveMinutes) * time.Minute
	snap.Accounts = make([]AccountView, 0, len(t.stats))
	for id, st := range t.stats {
		view := AccountView{
			AuthID:         id,
			Tier:           t.tiers[id],
			Probation:      t.probation[id],
			Trusted:        t.trusted[id],
			Eligible:       t.eligible[id],
			Rank:           t.ranks[id],
			EWMAMs:         math.Round(st.EWMA),
			Samples:        st.Samples,
			Failures:       st.Failures,
			RecentFailures: st.recentFailures(),
			RecentWindow:   len(st.Recent),
			Recent10m:      st.recentCount(now),
			LastSample:     st.LastSample,
			Active:         !st.LastSample.IsZero() && now.Sub(st.LastSample) <= inactive,
		}
		if view.Tier == "" {
			view.Tier = TierNormal
		}
		if a := meta[id]; a != nil {
			view.Label = a.Label
			view.Plan = strings.TrimSpace(a.Attributes["plan_type"])
			view.Status = string(a.Status)
			if p, ok := t.auths.AuthPriorityInMemory(id); ok {
				view.Priority = p
			}
			view.InFlight = t.auths.InFlight(id)
			snap.InFlightTotal += view.InFlight
		}
		switch view.Tier {
		case TierFast:
			snap.Fast++
		case TierSlow:
			snap.Slow++
		default:
			snap.Normal++
		}
		if view.Probation {
			snap.Probation++
		}
		if view.Eligible {
			snap.Eligible++
		}
		snap.Accounts = append(snap.Accounts, view)
	}
	if snap.Tracked > 0 {
		snap.EligiblePercent = math.Round(float64(snap.Eligible)/float64(snap.Tracked)*1000) / 10
	}
	tierOrder := map[Tier]int{TierFast: 0, TierNormal: 1, TierSlow: 2}
	sort.Slice(snap.Accounts, func(i, j int) bool {
		a, b := snap.Accounts[i], snap.Accounts[j]
		if a.Probation != b.Probation {
			return a.Probation
		}
		if tierOrder[a.Tier] != tierOrder[b.Tier] {
			return tierOrder[a.Tier] < tierOrder[b.Tier]
		}
		if a.Eligible != b.Eligible {
			return a.Eligible
		}
		if a.EWMAMs != b.EWMAMs {
			return a.EWMAMs < b.EWMAMs
		}
		return a.AuthID < b.AuthID
	})
	return snap
}

// Run recomputes on the configured cadence until ctx is done.
func (t *Tracker) Run(ctx context.Context) {
	for {
		t.mu.Lock()
		interval := time.Duration(t.cfg.RecomputeSeconds) * time.Second
		t.mu.Unlock()
		if interval <= 0 {
			interval = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			t.Recompute()
		}
	}
}

// Load restores stats, baseline and config from the persistence file.
func (t *Tracker) Load() error {
	if t.path == "" {
		return nil
	}
	raw, err := os.ReadFile(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// Start from the defaults so fields added after the snapshot was written keep their defaults.
	p := persisted{Config: DefaultConfig()}
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if p.Config.Validate() == nil {
		// The persisted switch wins over config.yaml so a restart keeps the operator's last choice.
		t.cfg = p.Config
	}
	for id, st := range p.Stats {
		if st != nil && id != "" {
			st.AuthID = id
			t.stats[id] = st
		}
	}
	for id, base := range p.Baseline {
		t.baseline[id] = base
	}
	return nil
}

func (t *Tracker) persist() {
	if t.path == "" {
		return
	}
	t.mu.Lock()
	p := persisted{Config: t.cfg, Baseline: make(map[string]int, len(t.baseline)), Stats: make(map[string]*accountStat, len(t.stats)), SavedAt: t.now()}
	for id, b := range t.baseline {
		p.Baseline[id] = b
	}
	for id, st := range t.stats {
		cp := *st
		cp.Recent = append([]bool(nil), st.Recent...)
		cp.Minutes = make(map[int64]int, len(st.Minutes))
		for k, v := range st.Minutes {
			cp.Minutes[k] = v
		}
		p.Stats[id] = &cp
	}
	t.mu.Unlock()
	raw, err := json.Marshal(p)
	if err == nil {
		if errDir := os.MkdirAll(filepath.Dir(t.path), 0o755); errDir == nil {
			tmp := t.path + ".tmp"
			if err = os.WriteFile(tmp, raw, 0o600); err == nil {
				err = os.Rename(tmp, t.path)
			}
		} else {
			err = errDir
		}
	}
	t.mu.Lock()
	if err != nil {
		t.persistErr = err.Error()
	} else {
		t.persistErr = ""
	}
	t.mu.Unlock()
}

var (
	defaultMu      sync.RWMutex
	defaultTracker *Tracker
)

// Default returns the process-wide tracker installed by Install, or nil.
func Default() *Tracker {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultTracker
}

// SetDefault replaces the process-wide tracker (nil uninstalls it).
func SetDefault(t *Tracker) {
	defaultMu.Lock()
	defaultTracker = t
	defaultMu.Unlock()
}

// Install wires a tracker into the usage pipeline and starts its recompute loop.
func Install(ctx context.Context, auths AuthAccess, enabledDefault bool, path string) *Tracker {
	cfg := DefaultConfig()
	cfg.Enabled = enabledDefault
	tracker := New(auths, cfg, path)
	if err := tracker.Load(); err != nil {
		log.Warnf("latency-tier: failed to load snapshot %s: %v", path, err)
	}
	SetDefault(tracker)
	usage.RegisterNamedPlugin("latency-tier", tracker)
	go tracker.Run(ctx)
	log.Infof("latency-tier: installed (enabled=%v, snapshot=%s)", tracker.Config().Enabled, path)
	return tracker
}
