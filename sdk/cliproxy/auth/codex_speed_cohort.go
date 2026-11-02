package auth

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexspeed"
)

// codexSpeedDayZone is the business day used for daily cohort ratios (Beijing time).
var codexSpeedDayZone = time.FixedZone("CST", 8*3600)

func codexSpeedDay(t time.Time) string { return t.In(codexSpeedDayZone).Format("2006-01-02") }

type codexSpeedCohortSets struct {
	FastSet       map[string]bool `json:"fast_set"`
	FastConverted map[string]bool `json:"fast_converted"`
	SlowSet       map[string]bool `json:"slow_set"`
	SlowConverted map[string]bool `json:"slow_converted"`
}

func newCodexSpeedCohortSets() *codexSpeedCohortSets {
	return &codexSpeedCohortSets{FastSet: map[string]bool{}, FastConverted: map[string]bool{}, SlowSet: map[string]bool{}, SlowConverted: map[string]bool{}}
}

func (s *codexSpeedCohortSets) summary(day string) codexspeed.SpeedCohortDay {
	return codexspeed.SpeedCohortDay{Day: day, FastSet: len(s.FastSet), FastToSlow: len(s.FastConverted), SlowSet: len(s.SlowSet), SlowToFast: len(s.SlowConverted)}
}

// codexSpeedCohort keeps the per-day observation sets of each cohort, keyed by account key, and
// the last used-percent reading used for hourly quota burn.
type codexSpeedCohort struct {
	mu       sync.Mutex
	day      string
	sets     map[string]*codexSpeedCohortSets
	lastUsed map[string]float64
}

func (c *codexSpeedCohort) rollDayLocked(now time.Time) {
	day := codexSpeedDay(now)
	if c.day == day && c.sets != nil {
		return
	}
	c.day = day
	c.sets = map[string]*codexSpeedCohortSets{CodexSpeedCohortBoost: newCodexSpeedCohortSets(), CodexSpeedCohortHold: newCodexSpeedCohortSets()}
}

// observe adds an account whose last known tier is tier to today's set.
func (c *codexSpeedCohort) observe(now time.Time, key, cohort, tier string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDayLocked(now)
	s := c.sets[cohort]
	if s == nil {
		return
	}
	switch tier {
	case CodexSpeedTierFast:
		s.FastSet[key] = true
	case CodexSpeedTierSlow:
		s.SlowSet[key] = true
	}
}

// transition marks an observed account that changed tier today.
func (c *codexSpeedCohort) transition(now time.Time, tr codexspeed.Transition) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDayLocked(now)
	s := c.sets[tr.Cohort]
	if s == nil {
		return
	}
	switch {
	case tr.From == CodexSpeedTierFast && tr.To == CodexSpeedTierSlow && s.FastSet[tr.AccountKey]:
		s.FastConverted[tr.AccountKey] = true
	case tr.From == CodexSpeedTierSlow && tr.To == CodexSpeedTierFast && s.SlowSet[tr.AccountKey]:
		s.SlowConverted[tr.AccountKey] = true
	}
}

func (c *codexSpeedCohort) summaries(now time.Time) (codexspeed.SpeedCohortDay, codexspeed.SpeedCohortDay) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDayLocked(now)
	return c.sets[CodexSpeedCohortBoost].summary(c.day), c.sets[CodexSpeedCohortHold].summary(c.day)
}

// quotaBurn sums the used-percent increase since the previous call for today's fast set of each
// cohort. Decreases (window resets) are skipped; accounts keep counting after turning slow.
func (c *codexSpeedCohort) quotaBurn(now time.Time, used map[string]float64) (codexspeed.SpeedQuotaBurn, codexspeed.SpeedQuotaBurn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDayLocked(now)
	if c.lastUsed == nil {
		c.lastUsed = map[string]float64{}
	}
	burn := func(cohort string) codexspeed.SpeedQuotaBurn {
		var out codexspeed.SpeedQuotaBurn
		for key := range c.sets[cohort].FastSet {
			cur, ok := used[key]
			if !ok {
				continue
			}
			if prev, seen := c.lastUsed[key]; seen && cur >= prev {
				out.DeltaSum += cur - prev
				out.Accounts++
			}
			c.lastUsed[key] = cur
		}
		return out
	}
	return burn(CodexSpeedCohortBoost), burn(CodexSpeedCohortHold)
}

// codexSpeedPersisted is the accounts.dat content.
type codexSpeedPersisted struct {
	Day       string                           `json:"day"`
	Sets      map[string]*codexSpeedCohortSets `json:"sets"`
	LastKnown map[string]codexSpeedKnown       `json:"last_known"`
	LastUsed  map[string]float64               `json:"last_used"`
	GenHash   string                           `json:"generation_hash"`
	GenStart  time.Time                        `json:"generation_start"`
}

type codexSpeedKnown struct {
	Tier       string `json:"tier"`
	AccountKey string `json:"account_key"`
}

// MarshalState serializes the cohort sets, last known tiers and generation for accounts.dat.
func (t *CodexSpeedTracker) MarshalState() ([]byte, error) {
	if t == nil {
		return nil, nil
	}
	state := codexSpeedPersisted{LastKnown: map[string]codexSpeedKnown{}}
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		for id, a := range sh.accounts {
			if a.lastKnown != "" {
				state.LastKnown[id] = codexSpeedKnown{Tier: a.lastKnown, AccountKey: a.accountKey}
			}
		}
		sh.mu.Unlock()
	}
	_, state.GenHash, state.GenStart = t.ab.generation()
	t.cohort.mu.Lock()
	t.cohort.rollDayLocked(t.now())
	state.Day, state.Sets, state.LastUsed = t.cohort.day, t.cohort.sets, t.cohort.lastUsed
	data, err := json.Marshal(state)
	t.cohort.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("codex speed: encode state: %w", err)
	}
	return data, nil
}

// RestoreState loads accounts.dat written by MarshalState. Today's sets are kept only when the
// saved day is today; the generation start survives only when the config hash is unchanged.
func (t *CodexSpeedTracker) RestoreState(data []byte) error {
	if t == nil || len(data) == 0 {
		return nil
	}
	var state codexSpeedPersisted
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("codex speed: decode state: %w", err)
	}
	now := t.now()
	for id, known := range state.LastKnown {
		sh := t.shard(id)
		sh.mu.Lock()
		a := t.account(sh, id, now)
		a.lastKnown = known.Tier
		if known.AccountKey != "" {
			a.accountKey = known.AccountKey
		}
		sh.mu.Unlock()
	}
	t.cohort.mu.Lock()
	t.cohort.rollDayLocked(now)
	if state.Day == t.cohort.day {
		for cohort, sets := range state.Sets {
			if sets == nil || t.cohort.sets[cohort] == nil {
				continue
			}
			restored := newCodexSpeedCohortSets()
			for k := range sets.FastSet {
				restored.FastSet[k] = true
			}
			for k := range sets.FastConverted {
				restored.FastConverted[k] = true
			}
			for k := range sets.SlowSet {
				restored.SlowSet[k] = true
			}
			for k := range sets.SlowConverted {
				restored.SlowConverted[k] = true
			}
			t.cohort.sets[cohort] = restored
		}
	}
	if state.LastUsed != nil {
		t.cohort.lastUsed = state.LastUsed
	}
	t.cohort.mu.Unlock()
	t.ab.restoreGeneration(state.GenHash, state.GenStart)
	return nil
}

// CodexSpeedSnapshot is the live state shown by the management API.
type CodexSpeedSnapshot struct {
	Enabled          bool                  `json:"enabled"`
	Active           bool                  `json:"active"`
	Inactive         string                `json:"inactive,omitempty"`
	Classify         bool                  `json:"classify"`
	BaselineMs       *int64                `json:"baseline_ms"`
	CapPercent       int                   `json:"cap_percent"`
	EffectivePercent int                   `json:"effective_percent"`
	Generation       string                `json:"generation"`
	GenerationStart  time.Time             `json:"generation_start"`
	Tiers            codexspeed.SpeedTiers `json:"tiers"`
	Current          codexspeed.SpeedHour  `json:"current"`
	Weights          map[string]int        `json:"weights"`
}

func (t *CodexSpeedTracker) generationLabel() (string, time.Time) {
	_, hash, start := t.ab.generation()
	return hash + "@" + start.UTC().Format("20060102T150405Z"), start
}

func (t *CodexSpeedTracker) hourRecord(hour time.Time, treat, control codexspeed.SpeedGroupHour, probes int64, now time.Time) codexspeed.SpeedHour {
	cfg := t.config()
	view := t.view.Load()
	treat.Finalize()
	control.Finalize()
	gen, genStart := t.generationLabel()
	capPct, eff := t.ab.shares()
	rec := codexspeed.SpeedHour{
		Hour: hour, Generation: gen, GenerationStart: genStart, Enabled: cfg.Enable, Inactive: t.InactiveReason(),
		Classify: view.classify, CapPercent: capPct, EffectivePercent: eff,
		Treatment: treat, Control: control, Tiers: view.counts, Probes: probes,
	}
	if view.hasBase {
		b := view.baselineMs
		rec.BaselineMs = &b
	}
	rec.Boost, rec.Hold = t.cohort.summaries(now)
	rec.PreCreatedFastMs, rec.PreCreatedSlowMs = t.preCreatedByTier(hour)
	return rec
}

// RollHours returns the finished hour records, oldest first. used maps account keys to their used
// percent; the quota burn and probe count go to the latest finished hour.
func (t *CodexSpeedTracker) RollHours(used map[string]float64) []codexspeed.SpeedHour {
	if t == nil {
		return nil
	}
	now := t.now()
	finished, probes := t.ab.rollHours(now)
	out := make([]codexspeed.SpeedHour, 0, len(finished))
	for i, h := range finished {
		p := int64(0)
		if i == len(finished)-1 {
			p = probes
		}
		rec := t.hourRecord(h.start, h.treat, h.control, p, now)
		if i == len(finished)-1 {
			rec.QuotaBoost, rec.QuotaHold = t.cohort.quotaBurn(now, used)
		}
		out = append(out, rec)
	}
	return out
}

// Snapshot returns the live view including the unfinished hour.
func (t *CodexSpeedTracker) Snapshot() CodexSpeedSnapshot {
	cfg := t.config()
	now := t.now()
	treat, control, probes := t.ab.currentHour()
	cur := t.hourRecord(now.Truncate(time.Hour), treat, control, probes, now)
	return CodexSpeedSnapshot{
		Enabled: cfg.Enable, Active: t.Active(), Inactive: t.InactiveReason(), Classify: cur.Classify,
		BaselineMs: cur.BaselineMs, CapPercent: cur.CapPercent, EffectivePercent: cur.EffectivePercent,
		Generation: cur.Generation, GenerationStart: cur.GenerationStart, Tiers: cur.Tiers, Current: cur,
		Weights: map[string]int{CodexSpeedTierFast: cfg.Weights.Fast, CodexSpeedTierUnknown: cfg.Weights.Unknown, CodexSpeedTierSlow: cfg.Weights.Slow},
	}
}

// Tick runs the minute maintenance: rollback evaluation and pruning. It returns a rollback
// event description when the share changed.
func (t *CodexSpeedTracker) Tick() string {
	if t == nil {
		return ""
	}
	now := t.now()
	t.ab.prune(now)
	if !t.Active() {
		return ""
	}
	return t.ab.evaluateRollback(t.config(), now)
}

// SummaryLine is the minute log line; values are in the message because the file formatter
// drops structured fields.
func (t *CodexSpeedTracker) SummaryLine() string {
	s := t.Snapshot()
	base := int64(-1)
	if s.BaselineMs != nil {
		base = *s.BaselineMs
	}
	p50 := func(v *int64) int64 {
		if v == nil {
			return -1
		}
		return *v
	}
	rate := func(g codexspeed.SpeedGroupHour) float64 {
		if g.Attempts == 0 {
			return 0
		}
		return float64(g.Overloads) / float64(g.Attempts)
	}
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	return fmt.Sprintf("codex speed summary: enabled=%d active=%d inactive=%s classify=%d baseline_ms=%d fast=%d slow=%d unknown=%d boost_fast=%d hold_fast=%d effective_percent=%d treat_req=%d ctrl_req=%d treat_ttft_p50=%d ctrl_ttft_p50=%d treat_overload=%.3f ctrl_overload=%.3f probes=%d",
		b2i(s.Enabled), b2i(s.Active), s.Inactive, b2i(s.Classify), base, s.Tiers.Fast, s.Tiers.Slow, s.Tiers.Unknown, s.Tiers.BoostFast, s.Tiers.HoldFast,
		s.EffectivePercent, s.Current.Treatment.Requests, s.Current.Control.Requests, p50(s.Current.Treatment.P50Ms), p50(s.Current.Control.P50Ms),
		rate(s.Current.Treatment), rate(s.Current.Control), s.Current.Probes)
}
