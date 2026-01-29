package management

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestGetKiroUsage_MissingAuthIndex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{}
	router := gin.New()
	router.GET("/kiro-usage", h.GetKiroUsage)

	req := httptest.NewRequest(http.MethodGet, "/kiro-usage", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestGetKiroUsage_InvalidAuthIndex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{authManager: coreauth.NewManager(nil, nil, nil)}
	router := gin.New()
	router.GET("/kiro-usage", h.GetKiroUsage)

	req := httptest.NewRequest(http.MethodGet, "/kiro-usage?auth_index=abc", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d", http.StatusNotFound, w.Code)
	}
}

func TestGetKiroUsage_NilAuthManager(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{authManager: nil}
	router := gin.New()
	router.GET("/kiro-usage", h.GetKiroUsage)

	req := httptest.NewRequest(http.MethodGet, "/kiro-usage?auth_index=0", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d, got %d", http.StatusInternalServerError, w.Code)
	}
}

func TestExtractKiroCredentials_Nil(t *testing.T) {
	token, arn := extractKiroCredentials(nil)
	if token != "" || arn != "" {
		t.Errorf("expected empty strings for nil auth")
	}
}

func TestExtractKiroCredentials_Metadata(t *testing.T) {
	auth := &coreauth.Auth{
		Metadata: map[string]any{
			"access_token": "test-token",
			"profile_arn":  "test-arn",
		},
	}
	token, arn := extractKiroCredentials(auth)
	if token != "test-token" {
		t.Errorf("expected token 'test-token', got '%s'", token)
	}
	if arn != "test-arn" {
		t.Errorf("expected arn 'test-arn', got '%s'", arn)
	}
}

func TestExtractKiroCredentials_CamelCase(t *testing.T) {
	auth := &coreauth.Auth{
		Metadata: map[string]any{
			"accessToken": "camel-token",
			"profileArn":  "camel-arn",
		},
	}
	token, arn := extractKiroCredentials(auth)
	if token != "camel-token" {
		t.Errorf("expected token 'camel-token', got '%s'", token)
	}
	if arn != "camel-arn" {
		t.Errorf("expected arn 'camel-arn', got '%s'", arn)
	}
}

func TestExtractKiroEmail_Metadata(t *testing.T) {
	auth := &coreauth.Auth{
		Metadata: map[string]any{
			"email": "test@example.com",
		},
	}
	email := extractKiroEmail(auth)
	if email != "test@example.com" {
		t.Errorf("expected email 'test@example.com', got '%s'", email)
	}
}

func TestExtractKiroEmail_Nil(t *testing.T) {
	email := extractKiroEmail(nil)
	if email != "" {
		t.Errorf("expected empty string for nil auth")
	}
}

func TestParseKiroResetTime_Seconds(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	resetTime := now.Add(48 * time.Hour)
	resetStr := strconv.FormatInt(resetTime.Unix(), 10)

	days, nextDate := parseKiroResetTime(resetStr, now)
	if days != 2 {
		t.Fatalf("expected days_until 2, got %d", days)
	}
	if nextDate != resetTime.Format(time.RFC3339) {
		t.Fatalf("expected next_date %q, got %q", resetTime.Format(time.RFC3339), nextDate)
	}
}

func TestParseKiroResetTime_Milliseconds(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	resetTime := now.Add(72 * time.Hour)
	resetStr := strconv.FormatInt(resetTime.UnixMilli(), 10)

	days, nextDate := parseKiroResetTime(resetStr, now)
	if days != 3 {
		t.Fatalf("expected days_until 3, got %d", days)
	}
	if nextDate != resetTime.Format(time.RFC3339) {
		t.Fatalf("expected next_date %q, got %q", resetTime.Format(time.RFC3339), nextDate)
	}
}

func TestParseKiroResetTime_Invalid(t *testing.T) {
	dayCount, nextDate := parseKiroResetTime("not-a-number", time.Unix(0, 0))
	if dayCount != 0 || nextDate != "" {
		t.Fatalf("expected empty parse result, got days=%d date=%q", dayCount, nextDate)
	}
}
