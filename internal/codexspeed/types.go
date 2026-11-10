package codexspeed

import (
	"encoding/json"
	"time"
)

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

// Speed hour schemas: ranked is written by the speed-ranked strategy; legacy_ab marks hours
// migrated from the earlier A/B records, whose overall stats are approximate.
const (
	SpeedSchemaRanked   = "ranked"
	SpeedSchemaLegacyAB = "legacy_ab"
)

// SpeedGroupHour is one account group's upstream Codex attempts for one hour.
type SpeedGroupHour struct {
	Attempts  int64     `json:"attempts"`
	Overloads int64     `json:"overloads"`
	Hist      Histogram `json:"ttft_hist,omitempty"`
	Samples   uint64    `json:"ttft_samples"`
	P50Ms     *int64    `json:"p50_ms"`
	P90Ms     *int64    `json:"p90_ms"`
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
	g.Hist.Merge(other.Hist)
	g.Finalize()
}

// SpeedHour is one hour of speed-ranked selection stats. Account counts and utilization are
// averages over the hour's sampled minutes; group stats are attributed to the hour in which
// each attempt finished.
type SpeedHour struct {
	Hour             time.Time      `json:"hour"`
	Schema           string         `json:"schema"`
	Enabled          bool           `json:"enabled"`
	Minutes          int            `json:"minutes"`
	ReadyMinutes     int            `json:"ready_minutes"`
	FastPercent      int            `json:"fast_percent"`
	Fast             SpeedGroupHour `json:"fast"`
	Slow             SpeedGroupHour `json:"slow"`
	Overall          SpeedGroupHour `json:"overall"`
	FastAccounts     float64        `json:"fast_accounts"`
	SlowAccounts     float64        `json:"slow_accounts"`
	UnscoredAccounts float64        `json:"unscored_accounts"`
	FastUtil         *float64       `json:"fast_util"`
	SlowUtil         *float64       `json:"slow_util"`
	OverflowShare    *float64       `json:"overflow_share"`
	FastToSlow       int64          `json:"fast_to_slow"`
	SlowToFast       int64          `json:"slow_to_fast"`
	InstantDemotions int64          `json:"instant_demotions"`
	Probes           int64          `json:"probes"`
}

// FinalizeGroups refreshes the derived fields of the three groups and the overflow share.
func (h *SpeedHour) FinalizeGroups() {
	h.Fast.Finalize()
	h.Slow.Finalize()
	h.Overall.Finalize()
	h.OverflowShare = nil
	if total := h.Fast.Attempts + h.Slow.Attempts; total > 0 {
		v := float64(h.Slow.Attempts) / float64(total)
		h.OverflowShare = &v
	}
}

// StripHist drops raw histogram buckets; the page only needs derived fields.
func (h SpeedHour) StripHist() SpeedHour {
	h.Fast.Hist, h.Slow.Hist, h.Overall.Hist = nil, nil, nil
	return h
}

// UnmarshalJSON reads both the ranked format and the earlier A/B format, whose treatment and
// control attempts are merged into Overall once.
func (h *SpeedHour) UnmarshalJSON(data []byte) error {
	type plain SpeedHour
	var aux struct {
		plain
		Treatment  *SpeedGroupHour `json:"treatment"`
		Control    *SpeedGroupHour `json:"control"`
		Generation string          `json:"generation"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*h = SpeedHour(aux.plain)
	if aux.Treatment != nil || aux.Control != nil || (h.Schema == "" && aux.Generation != "") {
		h.Schema = SpeedSchemaLegacyAB
		for _, g := range []*SpeedGroupHour{aux.Treatment, aux.Control} {
			if g != nil {
				h.Overall.Merge(SpeedGroupHour{Attempts: g.Attempts, Overloads: g.Overloads, Hist: g.Hist})
			}
		}
	}
	if h.Schema == "" {
		h.Schema = SpeedSchemaRanked
	}
	h.FinalizeGroups()
	return nil
}

// mergePart folds another segment of the same hour into h.
func (h *SpeedHour) mergePart(o SpeedHour) {
	h.Fast.Merge(o.Fast)
	h.Slow.Merge(o.Slow)
	h.Overall.Merge(o.Overall)
	h.FastToSlow += o.FastToSlow
	h.SlowToFast += o.SlowToFast
	h.InstantDemotions += o.InstantDemotions
	h.Probes += o.Probes
	h.FinalizeGroups()
}

// HourRecord is one persisted line of history.jsonl.
type HourRecord struct {
	Hour        time.Time        `json:"hour"`
	FastSession *FastSessionHour `json:"fast_session,omitempty"`
	Speed       *SpeedHour       `json:"speed,omitempty"`
}

// UnmarshalJSON also reads the earlier speed_parts segments and merges them into Speed.
func (r *HourRecord) UnmarshalJSON(data []byte) error {
	type plain HourRecord
	var aux struct {
		plain
		SpeedParts []SpeedHour `json:"speed_parts"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*r = HourRecord(aux.plain)
	for _, part := range aux.SpeedParts {
		if r.Speed == nil {
			p := part
			r.Speed = &p
			continue
		}
		r.Speed.mergePart(part)
	}
	return nil
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
