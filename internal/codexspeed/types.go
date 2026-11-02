package codexspeed

import "time"

// Fast Session-Id effect labels (see FastSessionLabel).
const (
	LabelInsufficient = "insufficient"
	LabelEffective    = "effective"
	LabelSlower       = "slower"
	LabelNoDifference = "no_difference"
)

// labelMinControlSamples is the control-group sample floor for a fast session verdict.
const labelMinControlSamples = 50

// GroupHour is one group's stats for one hour. TTFT covers successful small requests only.
type GroupHour struct {
	Requests int64     `json:"requests"`
	Failures int64     `json:"failures"`
	Hist     Histogram `json:"ttft_hist,omitempty"`
	Samples  uint64    `json:"ttft_samples"`
	P50Ms    *int64    `json:"p50_ms"`
	P90Ms    *int64    `json:"p90_ms"`
}

// Finalize fills Samples, P50Ms and P90Ms from Hist; empty percentiles stay nil.
func (g *GroupHour) Finalize() {
	if g == nil {
		return
	}
	g.Samples = g.Hist.Count()
	g.P50Ms, g.P90Ms = nil, nil
	if v, ok := g.Hist.Quantile(0.5); ok {
		g.P50Ms = &v
	}
	if v, ok := g.Hist.Quantile(0.9); ok {
		g.P90Ms = &v
	}
}

// Merge adds other into g and refreshes the derived fields.
func (g *GroupHour) Merge(other GroupHour) {
	g.Requests += other.Requests
	g.Failures += other.Failures
	g.Hist.Merge(other.Hist)
	g.Finalize()
}

// FastSessionHour is one hour of fast Session-Id pool stats.
type FastSessionHour struct {
	Hour             time.Time `json:"hour"`
	Epoch            uint64    `json:"epoch"`
	Fast             GroupHour `json:"fast"`
	Control          GroupHour `json:"control"`
	Members          int       `json:"members"`
	EffectivePercent int       `json:"effective_percent"`
	Enabled          bool      `json:"enabled"`
	Label            string    `json:"label"`
}

// SpeedGroupHour is one speed-priority A/B group for one hour. Attempt fields count upstream
// Codex attempts; request fields count client requests that executed at least one attempt.
type SpeedGroupHour struct {
	Attempts        int64     `json:"attempts"`
	Overloads       int64     `json:"overloads"`
	AttemptFailures int64     `json:"attempt_failures"`
	Hist            Histogram `json:"ttft_hist,omitempty"`
	Samples         uint64    `json:"ttft_samples"`
	P50Ms           *int64    `json:"p50_ms"`
	P90Ms           *int64    `json:"p90_ms"`
	Requests        int64     `json:"requests"`
	RequestFailures int64     `json:"request_failures"`
	Final529        int64     `json:"final_529"`
}

// Finalize fills the derived TTFT fields from Hist.
func (g *SpeedGroupHour) Finalize() {
	if g == nil {
		return
	}
	g.Samples = g.Hist.Count()
	g.P50Ms, g.P90Ms = nil, nil
	if v, ok := g.Hist.Quantile(0.5); ok {
		g.P50Ms = &v
	}
	if v, ok := g.Hist.Quantile(0.9); ok {
		g.P90Ms = &v
	}
}

// Merge adds other into g and refreshes the derived fields.
func (g *SpeedGroupHour) Merge(other SpeedGroupHour) {
	g.Attempts += other.Attempts
	g.Overloads += other.Overloads
	g.AttemptFailures += other.AttemptFailures
	g.Requests += other.Requests
	g.RequestFailures += other.RequestFailures
	g.Final529 += other.Final529
	g.Hist.Merge(other.Hist)
	g.Finalize()
}

// SpeedCohortDay is a cohort's daily observation totals, cumulative for Day at the hour's end.
type SpeedCohortDay struct {
	Day        string `json:"day"`
	FastSet    int    `json:"fast_set"`
	FastToSlow int    `json:"fast_to_slow"`
	SlowSet    int    `json:"slow_set"`
	SlowToFast int    `json:"slow_to_fast"`
}

// SpeedQuotaBurn is the mean used-percent increase over the hour of one cohort's fast set.
type SpeedQuotaBurn struct {
	DeltaSum float64 `json:"delta_sum"`
	Accounts int     `json:"accounts"`
}

// SpeedTiers counts accounts per computed tier.
type SpeedTiers struct {
	Fast      int `json:"fast"`
	Slow      int `json:"slow"`
	Unknown   int `json:"unknown"`
	BoostFast int `json:"boost_fast"`
	HoldFast  int `json:"hold_fast"`
}

// SpeedHour is one hour of speed-priority stats.
type SpeedHour struct {
	Hour             time.Time      `json:"hour"`
	Generation       string         `json:"generation"`
	GenerationStart  time.Time      `json:"generation_start"`
	Enabled          bool           `json:"enabled"`
	Inactive         string         `json:"inactive,omitempty"`
	Classify         bool           `json:"classify"`
	BaselineMs       *int64         `json:"baseline_ms"`
	CapPercent       int            `json:"cap_percent"`
	EffectivePercent int            `json:"effective_percent"`
	Treatment        SpeedGroupHour `json:"treatment"`
	Control          SpeedGroupHour `json:"control"`
	Tiers            SpeedTiers     `json:"tiers"`
	Boost            SpeedCohortDay `json:"boost"`
	Hold             SpeedCohortDay `json:"hold"`
	QuotaBoost       SpeedQuotaBurn `json:"quota_boost"`
	QuotaHold        SpeedQuotaBurn `json:"quota_hold"`
	PreCreatedFastMs *int64         `json:"pre_created_fast_ms"`
	PreCreatedSlowMs *int64         `json:"pre_created_slow_ms"`
	Probes           int64          `json:"probes"`
}

// Transition is one change of an account's last known tier.
type Transition struct {
	Time       time.Time `json:"time"`
	AccountKey string    `json:"account_key"`
	AuthID     string    `json:"auth_id"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	Cohort     string    `json:"cohort"`
}

// HourRecord is one persisted line of history.jsonl.
type HourRecord struct {
	Hour        time.Time        `json:"hour"`
	FastSession *FastSessionHour `json:"fast_session,omitempty"`
	Speed       *SpeedHour       `json:"speed,omitempty"`
	// SpeedParts holds further generation segments of the same hour (a config change inside it).
	SpeedParts []SpeedHour `json:"speed_parts,omitempty"`
}

// SpeedSegments returns every speed segment of the record, oldest first.
func (r HourRecord) SpeedSegments() []SpeedHour {
	var out []SpeedHour
	if r.Speed != nil {
		out = append(out, *r.Speed)
	}
	return append(out, r.SpeedParts...)
}

// FastSessionLabel compares the two groups' small-request TTFT p50: effective when fast is at
// least 20% faster, slower when it is at least 10% slower, insufficient when control has fewer
// than 50 samples or fast has none.
func FastSessionLabel(fast, control GroupHour) string {
	if control.Hist.Count() < labelMinControlSamples || fast.Hist.Count() == 0 {
		return LabelInsufficient
	}
	f, okF := fast.Hist.Quantile(0.5)
	c, okC := control.Hist.Quantile(0.5)
	if !okF || !okC {
		return LabelInsufficient
	}
	switch {
	case float64(f) <= float64(c)*0.8:
		return LabelEffective
	case float64(f) >= float64(c)*1.1:
		return LabelSlower
	default:
		return LabelNoDifference
	}
}
