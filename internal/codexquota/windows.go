// Package codexquota aggregates Codex account quota for management reporting.
// It only reads quota data and never influences account scheduling.
package codexquota

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Window is one upstream quota window for an account.
type Window struct {
	Minutes     int       `json:"minutes"`
	UsedPercent float64   `json:"used_percent"`
	ResetAt     time.Time `json:"reset_at,omitempty"`
}

// remaining returns the percentage left, treating a passed reset as a full window.
func (w Window) remaining(now time.Time) float64 {
	if w.rolledOver(now) {
		return 100
	}
	return clampPercent(100 - w.UsedPercent)
}

func (w Window) rolledOver(now time.Time) bool {
	return !w.ResetAt.IsZero() && !w.ResetAt.After(now)
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

// WindowsFromSignals extracts the main windows from a passive response snapshot.
func WindowsFromSignals(signals map[string]string, observedAt time.Time) ([]Window, string) {
	if len(signals) == 0 {
		return nil, ""
	}
	lower := make(map[string]string, len(signals))
	for key, value := range signals {
		lower[strings.ToLower(key)] = strings.TrimSpace(value)
	}
	var windows []Window
	for _, name := range []string{"primary", "secondary"} {
		prefix := "x-codex-" + name + "-"
		minutes, errMinutes := strconv.Atoi(lower[prefix+"window-minutes"])
		used, errUsed := strconv.ParseFloat(lower[prefix+"used-percent"], 64)
		if errMinutes != nil || errUsed != nil || minutes <= 0 {
			continue
		}
		window := Window{Minutes: minutes, UsedPercent: clampPercent(used)}
		if at, errAt := strconv.ParseInt(lower[prefix+"reset-at"], 10, 64); errAt == nil && at > 0 {
			window.ResetAt = time.Unix(at, 0).UTC()
		} else if after, errAfter := strconv.ParseInt(lower[prefix+"reset-after-seconds"], 10, 64); errAfter == nil && after >= 0 {
			window.ResetAt = observedAt.Add(time.Duration(after) * time.Second).UTC()
		}
		windows = append(windows, window)
	}
	return windows, strings.ToLower(lower["x-codex-plan-type"])
}

// ParseUsage reads the windows from a /wham/usage response.
func ParseUsage(body []byte, now time.Time) ([]Window, string, error) {
	if !gjson.ValidBytes(body) {
		return nil, "", errors.New("codex quota: usage response is not JSON")
	}
	root := gjson.ParseBytes(body)
	rate := root.Get("rate_limit")
	reached := rate.Get("limit_reached").Bool() || (rate.Get("allowed").Exists() && !rate.Get("allowed").Bool())
	var windows []Window
	for _, name := range []string{"primary_window", "secondary_window"} {
		item := rate.Get(name)
		if !item.IsObject() {
			continue
		}
		seconds := item.Get("limit_window_seconds").Int()
		if seconds <= 0 {
			continue
		}
		used := item.Get("used_percent")
		if !used.Exists() && !reached {
			continue
		}
		window := Window{Minutes: int(seconds / 60), UsedPercent: 100}
		if used.Exists() {
			window.UsedPercent = clampPercent(used.Float())
		}
		if at := item.Get("reset_at").Int(); at > 0 {
			window.ResetAt = time.Unix(at, 0).UTC()
		} else if after := item.Get("reset_after_seconds"); after.Exists() && after.Int() >= 0 {
			window.ResetAt = now.Add(time.Duration(after.Int()) * time.Second).UTC()
		}
		windows = append(windows, window)
	}
	if len(windows) == 0 {
		return nil, "", errors.New("codex quota: usage response has no quota window")
	}
	return windows, strings.ToLower(strings.TrimSpace(root.Get("plan_type").String())), nil
}
