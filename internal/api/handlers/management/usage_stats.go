package management

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagestats"
)

const defaultUsageStatsRetentionDays = 90

var (
	errUsageStatsRange          = errors.New("to must not be before from")
	errUsageStatsPresetConflict = errors.New("preset must not be combined with from or to")
)

// resolveUsageStatsRange resolves a server-calendar preset or an inclusive
// explicit range. Only explicit ranges are constrained by maxCustomDays.
func resolveUsageStatsRange(preset, fromStr, toStr string, now time.Time, maxCustomDays int) (time.Time, time.Time, error) {
	preset = strings.TrimSpace(preset)
	fromStr = strings.TrimSpace(fromStr)
	toStr = strings.TrimSpace(toStr)
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	if preset != "" {
		if fromStr != "" || toStr != "" {
			return time.Time{}, time.Time{}, errUsageStatsPresetConflict
		}
		switch preset {
		case "today":
			return today, today, nil
		case "yesterday":
			yesterday := today.AddDate(0, 0, -1)
			return yesterday, yesterday, nil
		case "7d":
			return today.AddDate(0, 0, -6), today, nil
		default:
			return time.Time{}, time.Time{}, fmt.Errorf("unsupported preset %q", preset)
		}
	}

	parse := func(value string) (time.Time, error) {
		if value == "" {
			return today, nil
		}
		return time.ParseInLocation("2006-01-02", value, loc)
	}
	from, err := parse(fromStr)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to, err := parse(toStr)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if to.Before(from) {
		return time.Time{}, time.Time{}, errUsageStatsRange
	}
	if maxCustomDays <= 0 {
		maxCustomDays = defaultUsageStatsRetentionDays
	}
	days := 0
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		days++
		if days > maxCustomDays {
			return time.Time{}, time.Time{}, fmt.Errorf("date range must not exceed %d days", maxCustomDays)
		}
	}
	return from, to, nil
}

// GetUsageStats returns per-account per-model call counts over a date range.
func (h *Handler) GetUsageStats(c *gin.Context) {
	if h == nil || h.cfg == nil || !h.cfg.UsageStatsEnabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage stats not enabled"})
		return
	}
	now := time.Now().In(time.Local)
	maxCustomDays := h.cfg.UsageStatsRetentionDays
	if maxCustomDays <= 0 {
		maxCustomDays = defaultUsageStatsRetentionDays
	}
	from, to, err := resolveUsageStatsRange(c.Query("preset"), c.Query("from"), c.Query("to"), now, maxCustomDays)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	account := strings.TrimSpace(c.Query("account"))
	stats, err := usagestats.Query(from, to, account)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"from":              from.Format("2006-01-02"),
		"to":                to.Format("2006-01-02"),
		"server_today":      now.Format("2006-01-02"),
		"server_utc_offset": now.Format("-07:00"),
		"accounts":          stats,
	})
}
