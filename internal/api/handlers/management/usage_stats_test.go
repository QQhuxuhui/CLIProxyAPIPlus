package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestResolveUsageStatsRangePresets(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 7, 17, 23, 45, 0, 0, loc)
	tests := []struct {
		preset   string
		wantFrom string
		wantTo   string
	}{
		{preset: "today", wantFrom: "2026-07-17", wantTo: "2026-07-17"},
		{preset: "yesterday", wantFrom: "2026-07-16", wantTo: "2026-07-16"},
		{preset: "7d", wantFrom: "2026-07-11", wantTo: "2026-07-17"},
	}
	for _, tt := range tests {
		t.Run(tt.preset, func(t *testing.T) {
			from, to, err := resolveUsageStatsRange(tt.preset, "", "", now, 90)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got := from.Format("2006-01-02"); got != tt.wantFrom {
				t.Errorf("from = %s, want %s", got, tt.wantFrom)
			}
			if got := to.Format("2006-01-02"); got != tt.wantTo {
				t.Errorf("to = %s, want %s", got, tt.wantTo)
			}
		})
	}
}

func TestResolveUsageStatsRangeValidatesInputs(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if _, _, err := resolveUsageStatsRange("weekly", "", "", now, 90); err == nil {
		t.Error("unknown preset must fail")
	}
	if _, _, err := resolveUsageStatsRange("today", "2026-07-17", "", now, 90); err == nil {
		t.Error("preset combined with explicit dates must fail")
	}
	if _, _, err := resolveUsageStatsRange("", "2026-07-18", "2026-07-17", now, 90); err == nil {
		t.Error("to before from must fail")
	}
}

func TestResolveUsageStatsRangeLimitsOnlyExplicitRanges(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if _, _, err := resolveUsageStatsRange("", "2026-07-11", "2026-07-17", now, 7); err != nil {
		t.Fatalf("range equal to limit: %v", err)
	}
	if _, _, err := resolveUsageStatsRange("", "2026-07-10", "2026-07-17", now, 7); err == nil {
		t.Error("range over limit must fail")
	}
	if _, _, err := resolveUsageStatsRange("7d", "", "", now, 1); err != nil {
		t.Fatalf("fixed 7d preset must remain available with one-day retention: %v", err)
	}
}

func TestGetUsageStatsDisabledReturns503(t *testing.T) {
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-stats", nil)

	h := &Handler{cfg: &config.Config{UsageStatsEnabled: false}}
	h.GetUsageStats(ginCtx)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
}

func TestGetUsageStatsBadRangeReturns400(t *testing.T) {
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-stats?from=2026-07-03&to=2026-06-27", nil)

	h := &Handler{cfg: &config.Config{UsageStatsEnabled: true}}
	h.GetUsageStats(ginCtx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	if _, ok := payload["error"]; !ok {
		t.Fatalf("response missing error key: %s", rec.Body.String())
	}
}

func TestGetUsageStatsPresetReturnsServerCalendarMetadata(t *testing.T) {
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-stats?preset=7d", nil)
	h := &Handler{cfg: &config.Config{UsageStatsEnabled: true, UsageStatsRetentionDays: 1}}
	h.GetUsageStats(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		From            string `json:"from"`
		To              string `json:"to"`
		ServerToday     string `json:"server_today"`
		ServerUTCOffset string `json:"server_utc_offset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	from, err := time.ParseInLocation("2006-01-02", payload.From, time.Local)
	if err != nil {
		t.Fatalf("parse from: %v", err)
	}
	to, err := time.ParseInLocation("2006-01-02", payload.To, time.Local)
	if err != nil {
		t.Fatalf("parse to: %v", err)
	}
	if !from.AddDate(0, 0, 6).Equal(to) {
		t.Fatalf("preset range = %s..%s, want seven inclusive days", payload.From, payload.To)
	}
	if payload.ServerToday != payload.To {
		t.Errorf("server_today = %q, want preset to %q", payload.ServerToday, payload.To)
	}
	if matched, _ := regexp.MatchString(`^[+-][0-9]{2}:[0-9]{2}$`, payload.ServerUTCOffset); !matched {
		t.Errorf("server_utc_offset = %q, want +HH:MM or -HH:MM", payload.ServerUTCOffset)
	}
}

func TestGetUsageStatsRejectsPresetDateConflictAndOversizedRange(t *testing.T) {
	for _, target := range []string{
		"/v0/management/usage-stats?preset=today&from=2026-07-17",
		"/v0/management/usage-stats?from=2026-07-10&to=2026-07-17",
	} {
		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = httptest.NewRequest(http.MethodGet, target, nil)
		h := &Handler{cfg: &config.Config{UsageStatsEnabled: true, UsageStatsRetentionDays: 7}}
		h.GetUsageStats(ginCtx)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400 body=%s", target, rec.Code, rec.Body.String())
		}
	}
}

func TestGetUsageStatsEnabledNoDataReturns200(t *testing.T) {
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/usage-stats?from=2026-06-27&to=2026-07-03", nil)

	h := &Handler{cfg: &config.Config{UsageStatsEnabled: true}}
	h.GetUsageStats(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload struct {
		From            string            `json:"from"`
		To              string            `json:"to"`
		ServerToday     string            `json:"server_today"`
		ServerUTCOffset string            `json:"server_utc_offset"`
		Accounts        []json.RawMessage `json:"accounts"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	if payload.From != "2026-06-27" || payload.To != "2026-07-03" {
		t.Fatalf("range = %s..%s, want 2026-06-27..2026-07-03", payload.From, payload.To)
	}
	if payload.ServerToday == "" || payload.ServerUTCOffset == "" {
		t.Fatalf("server calendar metadata must be non-empty: today=%q offset=%q", payload.ServerToday, payload.ServerUTCOffset)
	}
	if payload.Accounts == nil || len(payload.Accounts) != 0 {
		t.Fatalf("accounts = %v, want empty non-null array", payload.Accounts)
	}
	if !strings.Contains(rec.Body.String(), `"accounts":[]`) {
		t.Fatalf("response body missing empty array accounts field: %s", rec.Body.String())
	}
}
