package management

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexspeed"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const codexSpeedRecentEvents = 10

type codexSpeedFastSessionView struct {
	Enable           bool                          `json:"enable"`
	Percent          int                           `json:"percent"`
	EffectivePercent int                           `json:"effective_percent"`
	Members          int                           `json:"members"`
	Events           []helps.CodexFastSessionEvent `json:"events"`
	Current          codexspeed.FastSessionHour    `json:"current"`
	Hours            []codexspeed.FastSessionHour  `json:"hours"`
	Labels24h        map[string]int                `json:"labels_24h"`
}

type codexSpeedView struct {
	HistoryAvailable bool                      `json:"history_available"`
	FastSession      codexSpeedFastSessionView `json:"fast_session"`
	Speed            codexSpeedPriorityView    `json:"speed"`
}

type codexSpeedPriorityView struct {
	Config     codexSpeedPriorityConfigView `json:"config"`
	Live       coreauth.CodexSpeedSnapshot  `json:"live"`
	Hours      []codexspeed.SpeedHour       `json:"hours"`
	Acceptance codexSpeedAcceptance         `json:"acceptance"`
}

type codexSpeedPriorityConfigView struct {
	Enable          bool           `json:"enable"`
	Percent         int            `json:"percent"`
	Weights         map[string]int `json:"weights"`
	PoolsConfigured bool           `json:"pools_configured"`
	PoolsRunning    bool           `json:"pools_running"`
	Rollback        bool           `json:"rollback"`
}

// Acceptance thresholds (D19, D32).
const (
	codexSpeedAcceptMinSamples  = 500
	codexSpeedAcceptMinRequests = 2000
	codexSpeedAcceptMinCohort   = 20
	codexSpeedAcceptTTFTDrop    = 0.30
	codexSpeedAcceptWearRatio   = 1.5
)

type codexSpeedCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type codexSpeedDay struct {
	Day             string   `json:"day"`
	BoostFastSet    int      `json:"boost_fast_set"`
	BoostFastToSlow int      `json:"boost_fast_to_slow"`
	HoldFastSet     int      `json:"hold_fast_set"`
	HoldFastToSlow  int      `json:"hold_fast_to_slow"`
	BoostSlowSet    int      `json:"boost_slow_set"`
	BoostSlowToFast int      `json:"boost_slow_to_fast"`
	HoldSlowSet     int      `json:"hold_slow_set"`
	HoldSlowToFast  int      `json:"hold_slow_to_fast"`
	QuotaBoost      float64  `json:"quota_boost_per_account_hour"`
	QuotaHold       float64  `json:"quota_hold_per_account_hour"`
	BoostRate       *float64 `json:"boost_rate"`
	HoldRate        *float64 `json:"hold_rate"`
	WearStatus      string   `json:"wear_status"`
}

// evaluateWear judges one day: both cohorts need at least codexSpeedAcceptMinCohort fast accounts.
func (d *codexSpeedDay) evaluateWear() {
	d.WearStatus = "insufficient"
	if d.BoostFastSet > 0 {
		v := float64(d.BoostFastToSlow) / float64(d.BoostFastSet)
		d.BoostRate = &v
	}
	if d.HoldFastSet > 0 {
		v := float64(d.HoldFastToSlow) / float64(d.HoldFastSet)
		d.HoldRate = &v
	}
	if d.BoostFastSet < codexSpeedAcceptMinCohort || d.HoldFastSet < codexSpeedAcceptMinCohort {
		return
	}
	b, h := *d.BoostRate, *d.HoldRate
	d.WearStatus = statusOf((b == 0 && h == 0) || (h > 0 && b <= h*codexSpeedAcceptWearRatio))
}

type codexSpeedAcceptance struct {
	Generation string                    `json:"generation"`
	RangeStart *time.Time                `json:"range_start"`
	RangeEnd   *time.Time                `json:"range_end"`
	Hours      int                       `json:"hours"`
	Treatment  codexspeed.SpeedGroupHour `json:"treatment"`
	Control    codexspeed.SpeedGroupHour `json:"control"`
	Checks     []codexSpeedCheck         `json:"checks"`
	Days       []codexSpeedDay           `json:"days"`
}

// GetCodexSpeed reports the speed-strategy page data: the fast Session-Id pool state, its
// current hour and up to seven days of hourly history.
func (h *Handler) GetCodexSpeed(c *gin.Context) {
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	pool := helps.CodexFastSessions()
	if cfg != nil {
		fastCfg := cfg.Codex.FastSessionIDs
		fastCfg.Normalize()
		pool.ApplyUnmanagedConfig(fastCfg)
	}
	now := time.Now()
	snap := pool.Snapshot(0)
	events := snap.Events
	if len(events) > codexSpeedRecentEvents {
		events = events[len(events)-codexSpeedRecentEvents:]
	}
	view := codexSpeedView{FastSession: codexSpeedFastSessionView{
		Enable:           snap.Enable,
		Percent:          snap.Percent,
		EffectivePercent: snap.EffectivePercent,
		Members:          len(snap.Members),
		Events:           events,
		Current:          stripFastSessionHist(pool.CurrentHour(now)),
		Hours:            []codexspeed.FastSessionHour{},
		Labels24h:        map[string]int{},
	}}
	rt := codexspeed.Default()
	view.HistoryAvailable = rt != nil
	cutoff := now.Add(-24 * time.Hour)
	records := rt.Hours()
	view.Speed = h.codexSpeedPriorityView(cfg, records)
	for _, rec := range records {
		if rec.FastSession == nil {
			continue
		}
		hour := stripFastSessionHist(*rec.FastSession)
		view.FastSession.Hours = append(view.FastSession.Hours, hour)
		if !hour.Hour.Before(cutoff) {
			view.FastSession.Labels24h[hour.Label]++
		}
	}
	c.JSON(http.StatusOK, view)
}

// stripFastSessionHist drops raw histogram buckets; the page only needs derived fields.
func stripFastSessionHist(h codexspeed.FastSessionHour) codexspeed.FastSessionHour {
	h.Fast.Hist = nil
	h.Control.Hist = nil
	return h
}

type codexSpeedConfigUpdate struct {
	FastSession *struct {
		Enable  *bool `json:"enable"`
		Percent *int  `json:"percent"`
	} `json:"fast_session"`
	SpeedPriority *struct {
		Enable  *bool `json:"enable"`
		Percent *int  `json:"percent"`
		Weights *struct {
			Fast    *int `json:"fast"`
			Unknown *int `json:"unknown"`
			Slow    *int `json:"slow"`
		} `json:"weights"`
	} `json:"speed_priority"`
}

// PutCodexSpeedConfig updates the speed-strategy switches in config.yaml; the running
// service picks them up through the normal hot reload.
func (h *Handler) PutCodexSpeedConfig(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration is not available"})
		return
	}
	var body codexSpeedConfigUpdate
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected one JSON configuration object"})
		return
	}
	next := h.cfg.CloneForRuntime()
	if fs := body.FastSession; fs != nil {
		if fs.Percent != nil {
			if *fs.Percent < 0 || *fs.Percent > 100 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "fast_session.percent must be between 0 and 100"})
				return
			}
			next.Codex.FastSessionIDs.Percent = *fs.Percent
		}
		if fs.Enable != nil {
			next.Codex.FastSessionIDs.Enable = *fs.Enable
		}
	}
	if sp := body.SpeedPriority; sp != nil {
		speed := &next.Codex.SpeedPriority
		if sp.Percent != nil {
			if *sp.Percent < 1 || *sp.Percent > 100 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "speed_priority.percent must be between 1 and 100"})
				return
			}
			speed.Percent = *sp.Percent
		}
		if w := sp.Weights; w != nil {
			for _, item := range []struct {
				name  string
				value *int
				dst   *int
			}{{"fast", w.Fast, &speed.Weights.Fast}, {"unknown", w.Unknown, &speed.Weights.Unknown}, {"slow", w.Slow, &speed.Weights.Slow}} {
				if item.value == nil {
					continue
				}
				if *item.value < 1 || *item.value > 1000 {
					c.JSON(http.StatusBadRequest, gin.H{"error": "speed_priority.weights." + item.name + " must be between 1 and 1000"})
					return
				}
				*item.dst = *item.value
			}
		}
		if sp.Enable != nil {
			if *sp.Enable {
				switch {
				case next.Home.Enabled:
					c.JSON(http.StatusBadRequest, gin.H{"error": "Home dispatch owns scheduling; speed priority is unavailable"})
					return
				case !next.Codex.Pools.Enable:
					c.JSON(http.StatusBadRequest, gin.H{"error": "speed priority needs codex.pools.enable; turn on Codex pools first"})
					return
				case h.authManager == nil || !h.authManager.CodexPoolsRunning():
					c.JSON(http.StatusBadRequest, gin.H{"error": "Codex pools are configured but not running; restart the service first"})
					return
				}
			}
			speed.Enable = *sp.Enable
		}
		speed.Normalize()
	}
	h.persistConfigLocked(c, next)
}

func (h *Handler) codexSpeedPriorityView(cfg *config.Config, records []codexspeed.HourRecord) codexSpeedPriorityView {
	tracker := coreauth.CodexSpeed()
	live := tracker.Snapshot()
	live.Current.Treatment.Hist, live.Current.Control.Hist = nil, nil
	view := codexSpeedPriorityView{Live: live, Hours: []codexspeed.SpeedHour{}}
	if cfg != nil {
		speedCfg := cfg.Codex.SpeedPriority
		speedCfg.Normalize()
		view.Config = codexSpeedPriorityConfigView{
			Enable: speedCfg.Enable, Percent: speedCfg.Percent, Rollback: speedCfg.RollbackEnabled(),
			Weights:         map[string]int{"fast": speedCfg.Weights.Fast, "unknown": speedCfg.Weights.Unknown, "slow": speedCfg.Weights.Slow},
			PoolsConfigured: cfg.Codex.Pools.Enable && !cfg.Home.Enabled,
			PoolsRunning:    h.authManager != nil && h.authManager.CodexPoolsRunning(),
		}
	}
	var speedHours []codexspeed.SpeedHour
	for _, rec := range records {
		speedHours = append(speedHours, rec.SpeedSegments()...)
	}
	current := tracker.Snapshot().Current
	view.Acceptance = codexSpeedAcceptanceFor(live.Generation, speedHours, current)
	for _, hour := range speedHours {
		hour.Treatment.Hist, hour.Control.Hist = nil, nil
		view.Hours = append(view.Hours, hour)
	}
	return view
}

// codexSpeedAcceptanceFor merges the current generation's hours (histograms added bucket by
// bucket) and evaluates the three full-rollout checks.
func codexSpeedAcceptanceFor(generation string, hours []codexspeed.SpeedHour, current codexspeed.SpeedHour) codexSpeedAcceptance {
	out := codexSpeedAcceptance{Generation: generation, Checks: []codexSpeedCheck{}, Days: []codexSpeedDay{}}
	all := append(append([]codexspeed.SpeedHour{}, hours...), current)
	dayIndex := map[string]int{}
	for _, hour := range all {
		if hour.Generation != generation {
			continue
		}
		out.Treatment.Merge(hour.Treatment)
		out.Control.Merge(hour.Control)
		out.Hours++
		h := hour.Hour
		if out.RangeStart == nil || h.Before(*out.RangeStart) {
			out.RangeStart = &h
		}
		if out.RangeEnd == nil || h.After(*out.RangeEnd) {
			out.RangeEnd = &h
		}
		if hour.Boost.Day == "" {
			continue
		}
		idx, ok := dayIndex[hour.Boost.Day]
		if !ok {
			idx = len(out.Days)
			dayIndex[hour.Boost.Day] = idx
			out.Days = append(out.Days, codexSpeedDay{Day: hour.Boost.Day})
		}
		d := &out.Days[idx]
		// Cohort counts are cumulative for the day, so the latest hour wins.
		d.BoostFastSet, d.BoostFastToSlow, d.BoostSlowSet, d.BoostSlowToFast = hour.Boost.FastSet, hour.Boost.FastToSlow, hour.Boost.SlowSet, hour.Boost.SlowToFast
		d.HoldFastSet, d.HoldFastToSlow, d.HoldSlowSet, d.HoldSlowToFast = hour.Hold.FastSet, hour.Hold.FastToSlow, hour.Hold.SlowSet, hour.Hold.SlowToFast
		d.QuotaBoost += hour.QuotaBoost.DeltaSum
		d.QuotaHold += hour.QuotaHold.DeltaSum
	}
	quotaAccounts := map[string][2]int{}
	for _, hour := range all {
		if hour.Generation == generation && hour.Boost.Day != "" {
			q := quotaAccounts[hour.Boost.Day]
			quotaAccounts[hour.Boost.Day] = [2]int{q[0] + hour.QuotaBoost.Accounts, q[1] + hour.QuotaHold.Accounts}
		}
	}
	for i := range out.Days {
		q := quotaAccounts[out.Days[i].Day]
		if q[0] > 0 {
			out.Days[i].QuotaBoost /= float64(q[0])
		}
		if q[1] > 0 {
			out.Days[i].QuotaHold /= float64(q[1])
		}
	}
	t, c := out.Treatment, out.Control
	enough := t.Samples >= codexSpeedAcceptMinSamples && c.Samples >= codexSpeedAcceptMinSamples && t.Requests >= codexSpeedAcceptMinRequests && c.Requests >= codexSpeedAcceptMinRequests
	ttft := codexSpeedCheck{Name: "ttft_drop", Status: "insufficient", Detail: "每组需要至少 500 个小请求首字样本和 2000 个请求"}
	if enough && t.P50Ms != nil && c.P50Ms != nil && *c.P50Ms > 0 {
		drop := 1 - float64(*t.P50Ms)/float64(*c.P50Ms)
		ttft.Status = statusOf(drop >= codexSpeedAcceptTTFTDrop)
		ttft.Detail = fmt.Sprintf("实验组 p50 %d ms，对照组 %d ms，下降 %.1f%%", *t.P50Ms, *c.P50Ms, 100*drop)
	}
	failures := codexSpeedCheck{Name: "overload_and_529", Status: "insufficient", Detail: ttft.Detail}
	if enough && t.Attempts > 0 && c.Attempts > 0 {
		tOv, cOv := ratio(t.Overloads, t.Attempts), ratio(c.Overloads, c.Attempts)
		t529, c529 := ratio(t.Final529, t.Requests), ratio(c.Final529, c.Requests)
		failures.Status = statusOf(tOv <= cOv && t529 <= c529)
		failures.Detail = fmt.Sprintf("过载率 %.1f%% vs %.1f%%，529 率 %.2f%% vs %.2f%%", 100*tOv, 100*cOv, 100*t529, 100*c529)
	}
	var judged, failed, insufficient int
	for i := range out.Days {
		out.Days[i].evaluateWear()
		switch out.Days[i].WearStatus {
		case "pass":
			judged++
		case "fail":
			judged++
			failed++
		default:
			insufficient++
		}
	}
	wear := codexSpeedCheck{Name: "fast_wear", Status: "insufficient", Detail: fmt.Sprintf("没有两组快号观察集合都达到 %d 个账号的日子（样本不足 %d 天）", codexSpeedAcceptMinCohort, insufficient)}
	if judged > 0 {
		wear.Status = statusOf(failed == 0)
		wear.Detail = fmt.Sprintf("逐日判定：参与 %d 天，未达标 %d 天，样本不足 %d 天（不参与）", judged, failed, insufficient)
	}
	out.Checks = append(out.Checks, ttft, failures, wear)
	out.Treatment.Hist, out.Control.Hist = nil, nil
	return out
}

func ratio(n, d int64) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func statusOf(pass bool) string {
	if pass {
		return "pass"
	}
	return "fail"
}
