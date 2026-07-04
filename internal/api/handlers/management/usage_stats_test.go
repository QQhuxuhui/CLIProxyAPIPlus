package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestParseUsageStatsRange(t *testing.T) {
	loc := time.Local
	from, to, err := parseUsageStatsRange("2026-06-27", "2026-07-03", loc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if from.Format("2006-01-02") != "2026-06-27" || to.Format("2006-01-02") != "2026-07-03" {
		t.Errorf("range = %s..%s", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
}

func TestParseUsageStatsRange_DefaultsToToday(t *testing.T) {
	loc := time.Local
	from, to, err := parseUsageStatsRange("", "", loc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	today := time.Now().In(loc).Format("2006-01-02")
	if from.Format("2006-01-02") != today || to.Format("2006-01-02") != today {
		t.Errorf("empty range should default to today %s, got %s..%s", today, from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
}

func TestParseUsageStatsRange_ToBeforeFrom(t *testing.T) {
	if _, _, err := parseUsageStatsRange("2026-07-03", "2026-06-27", time.Local); err == nil {
		t.Error("to<from must error")
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
		From     string            `json:"from"`
		To       string            `json:"to"`
		Accounts []json.RawMessage `json:"accounts"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal response: %v", errUnmarshal)
	}
	if payload.From != "2026-06-27" || payload.To != "2026-07-03" {
		t.Fatalf("range = %s..%s, want 2026-06-27..2026-07-03", payload.From, payload.To)
	}
	if payload.Accounts == nil || len(payload.Accounts) != 0 {
		t.Fatalf("accounts = %v, want empty non-null array", payload.Accounts)
	}
	if !strings.Contains(rec.Body.String(), `"accounts":[]`) {
		t.Fatalf("response body missing empty array accounts field: %s", rec.Body.String())
	}
}
