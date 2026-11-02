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

// HourRecord is one persisted line of history.jsonl.
type HourRecord struct {
	Hour        time.Time        `json:"hour"`
	FastSession *FastSessionHour `json:"fast_session,omitempty"`
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
