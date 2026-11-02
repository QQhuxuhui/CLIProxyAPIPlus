package management

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexspeed"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
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
	for _, rec := range rt.Hours() {
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
	h.persistConfigLocked(c, next)
}
