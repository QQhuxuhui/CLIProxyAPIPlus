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
	// PenaltyMs is the sample recorded for a failed request.
	PenaltyMs int `json:"penalty_ms"`
	// RecomputeSeconds is the tier recomputation cadence.
	RecomputeSeconds int `json:"recompute_seconds"`
	FastPriority     int `json:"fast_priority"`
	NormalPriority   int `json:"normal_priority"`
	SlowPriority     int `json:"slow_priority"`
}

// DefaultConfig returns the parameters agreed for the first rollout.
func DefaultConfig() Config {
	return Config{
		Enabled:          false,
		FastEnterPercent: 0.20,
		FastExitPercent:  0.30,
		SlowEnterPercent: 0.40,
		SlowExitPercent:  0.50,
		HalfLifeSeconds:  600,
		MinSamples:       5,
		InactiveMinutes:  30,
		ProbationPercent: 0.05,
		ProbationMin:     3,
		PenaltyMs:        30000,
		RecomputeSeconds: 60,
		FastPriority:     2,
		NormalPriority:   1,
		SlowPriority:     0,
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
	if c.HalfLifeSeconds <= 0 || c.MinSamples <= 0 || c.InactiveMinutes <= 0 || c.PenaltyMs <= 0 || c.RecomputeSeconds <= 0 || c.ProbationMin < 0 {
		return errors.New("half_life_seconds, min_samples, inactive_minutes, penalty_ms, recompute_seconds must be positive and probation_min non-negative")
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
}

type accountStat struct {
	AuthID     string    `json:"auth_id"`
	EWMA       float64   `json:"ewma_ms"`
	Weight     float64   `json:"weight"`
	EWMAAt     time.Time `json:"ewma_at"`
	Samples    int64     `json:"samples"`
	Failures   int64     `json:"failures"`
	LastSample time.Time `json:"last_sample"`
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
	now        func() time.Time
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
	ms, failed, ok := sampleFromRecord(rec, t.cfg.PenaltyMs)
	if !ok {
		return
	}
	st := t.stats[rec.AuthID]
	if st == nil {
		st = &accountStat{AuthID: rec.AuthID}
		t.stats[rec.AuthID] = st
	}
	st.observe(now, ms, failed, time.Duration(t.cfg.HalfLifeSeconds)*time.Second)
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

// sampleFromRecord converts a usage record into (milliseconds, failed, usable).
func sampleFromRecord(rec usage.Record, penaltyMs int) (float64, bool, bool) {
	if rec.Failed {
		return float64(penaltyMs), true, true
	}
	if rec.Stream && rec.Detail.OutputTokens == 0 && rec.Detail.ReasoningTokens == 0 && rec.Detail.TotalTokens == 0 {
		// A streamed response that produced nothing is a stream cut after response.created.
		return float64(penaltyMs), true, true
	}
	if rec.TTFT > 0 {
		return float64(rec.TTFT.Milliseconds()), false, true
	}
	if rec.Latency > 0 {
		return float64(rec.Latency.Milliseconds()), false, true
	}
	return 0, false, false
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
	eligible := make([]ranked, 0, len(t.stats))
	t.eligible = make(map[string]bool, len(t.stats))
	for id, st := range t.stats {
		if st.Samples >= int64(t.cfg.MinSamples) && !st.LastSample.IsZero() && now.Sub(st.LastSample) <= inactive {
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
	for id := range t.stats {
		if _, ok := newTiers[id]; !ok {
			newTiers[id] = TierNormal
		}
	}

	// Probation: temporarily promote the least recently sampled non-fast credentials.
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
		if tier != TierFast {
			candidates = append(candidates, id)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := t.stats[candidates[i]].LastSample, t.stats[candidates[j]].LastSample
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

	if t.cfg.Enabled && t.auths != nil {
		t.applyLocked()
		for _, id := range demoted {
			t.auths.InvalidateSessionAffinity(id)
		}
	}
	t.mu.Unlock()
	t.persist()
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
		if last, ok := t.applied[id]; ok && last == want {
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
	snap := Snapshot{Config: t.cfg, ComputedAt: t.computedAt, Tracked: len(t.stats), Applied: len(t.applied), Persistence: t.persistErr}
	snap.Accounts = make([]AccountView, 0, len(t.stats))
	for id, st := range t.stats {
		view := AccountView{
			AuthID:         id,
			Tier:           t.tiers[id],
			Probation:      t.probation[id],
			Eligible:       t.eligible[id],
			Rank:           t.ranks[id],
			EWMAMs:         math.Round(st.EWMA),
			Samples:        st.Samples,
			Failures:       st.Failures,
			RecentFailures: st.recentFailures(),
			RecentWindow:   len(st.Recent),
			Recent10m:      st.recentCount(now),
			LastSample:     st.LastSample,
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
	var p persisted
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
