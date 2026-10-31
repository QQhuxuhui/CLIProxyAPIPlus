package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexquota"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func quotaRequest(t *testing.T, handle gin.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, "/v0/management/codex-quota", bytes.NewBufferString(body))
	handle(ctx)
	return recorder
}

func TestCodexQuotaEndpointsRequireStartedService(t *testing.T) {
	h := NewHandler(&config.Config{}, "", coreauth.NewManager(nil, nil, nil))
	if got := quotaRequest(t, h.GetCodexQuota, http.MethodGet, "").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", got)
	}
}

func TestCodexQuotaSettingsMergeValidateAndPersist(t *testing.T) {
	cfg := &config.Config{}
	cfg.AuthDir = t.TempDir()
	h := NewHandler(cfg, "", coreauth.NewManager(nil, nil, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.StartCodexQuota(ctx)

	response := quotaRequest(t, h.PutCodexQuotaSettings, http.MethodPut, `{"scan_enabled":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	var saved codexquota.Settings
	if errParse := json.Unmarshal(response.Body.Bytes(), &saved); errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if !saved.ScanEnabled || saved.ScanConcurrency != codexquota.DefaultSettings().ScanConcurrency {
		t.Fatalf("settings = %+v", saved)
	}
	if got := quotaRequest(t, h.PutCodexQuotaSettings, http.MethodPut, `{"scan_concurrency":0}`).Code; got != http.StatusBadRequest {
		t.Fatalf("invalid value status = %d", got)
	}
	if got := quotaRequest(t, h.PutCodexQuotaSettings, http.MethodPut, `{"unknown":1}`).Code; got != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", got)
	}
	reloaded := codexquota.New(filepath.Join(cfg.AuthDir, codexquota.DirName), func() codexquota.Source { return nil }, nil)
	if !reloaded.Settings().ScanEnabled {
		t.Fatal("settings were not persisted")
	}
}

func TestProbeCodexUsageRejectsUnknownAndExpiredCredentials(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandler(&config.Config{}, "", manager)
	if _, _, errProbe := h.probeCodexUsage(context.Background(), "missing"); errProbe == nil {
		t.Fatal("expected error for unknown credential")
	}
	auth := &coreauth.Auth{ID: "expired", Provider: "codex", Metadata: map[string]any{
		"access_token": "token",
		"expired":      time.Now().Add(-time.Hour).Format(time.RFC3339),
	}}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	status, _, errProbe := h.probeCodexUsage(context.Background(), "expired")
	if errProbe == nil || status == http.StatusUnauthorized {
		t.Fatalf("status = %d err = %v, want a retryable local error", status, errProbe)
	}
}
