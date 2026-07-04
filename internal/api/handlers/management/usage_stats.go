package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagestats"
)

// parseUsageStatsRange parses from/to as YYYY-MM-DD (inclusive). Empty values
// default to today. Returns an error if to < from.
func parseUsageStatsRange(fromStr, toStr string, loc *time.Location) (time.Time, time.Time, error) {
	today := time.Now().In(loc)
	parse := func(s string, def time.Time) (time.Time, error) {
		s = strings.TrimSpace(s)
		if s == "" {
			return time.Date(def.Year(), def.Month(), def.Day(), 0, 0, 0, 0, loc), nil
		}
		return time.ParseInLocation("2006-01-02", s, loc)
	}
	from, err := parse(fromStr, today)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to, err := parse(toStr, today)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if to.Before(from) {
		return time.Time{}, time.Time{}, errUsageStatsRange
	}
	return from, to, nil
}

var errUsageStatsRange = &usageStatsError{"to must not be before from"}

type usageStatsError struct{ msg string }

func (e *usageStatsError) Error() string { return e.msg }

// GetUsageStats returns per-account per-model call counts over a date range.
func (h *Handler) GetUsageStats(c *gin.Context) {
	if h == nil || !h.cfg.UsageStatsEnabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage stats not enabled"})
		return
	}
	from, to, err := parseUsageStatsRange(c.Query("from"), c.Query("to"), time.Local)
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
		"from":     from.Format("2006-01-02"),
		"to":       to.Format("2006-01-02"),
		"accounts": stats,
	})
}
