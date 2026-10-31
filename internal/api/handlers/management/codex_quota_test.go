package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

// quotaAuthDir tolerates the service saving its cache while the test tears down.
func quotaAuthDir(t *testing.T) string {
	t.Helper()
	dir, errTemp := os.MkdirTemp("", "codex-quota-")
	if errTemp != nil {
		t.Fatalf("MkdirTemp() error = %v", errTemp)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestCodexQuotaEndpointsRequireStartedService(t *testing.T) {
	h := NewHandler(&config.Config{}, "", coreauth.NewManager(nil, nil, nil))
	if got := quotaRequest(t, h.GetCodexQuota, http.MethodGet, "").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", got)
	}
}

func TestCodexQuotaSettingsMergeValidateAndPersist(t *testing.T) {
	cfg := &config.Config{}
	cfg.AuthDir = quotaAuthDir(t)
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

func TestCodexResetRoutesConsumeThroughCredential(t *testing.T) {
	var consumed []map[string]string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Chatgpt-Account-Id") != "acct" {
			t.Errorf("headers = %v", r.Header)
		}
		switch r.URL.Path {
		case "/usage":
			count := 1 - len(consumed)
			_, _ = fmt.Fprintf(w, `{"rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_after_seconds":60}},"rate_limit_reset_credits":{"available_count":%d}}`, count)
		case "/credits":
			if r.Header.Get("Originator") != "Codex Desktop" {
				t.Errorf("credits headers = %v", r.Header)
			}
			_, _ = w.Write([]byte(`{"credits":[]}`))
		case "/consume":
			var body map[string]string
			if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil || r.Method != http.MethodPost {
				t.Errorf("consume method = %s err = %v", r.Method, errDecode)
			}
			consumed = append(consumed, body)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	restoreUsage, restoreCredits, restoreConsume := codexUsageURL, codexResetCreditsURL, codexResetConsumeURL
	codexUsageURL, codexResetCreditsURL, codexResetConsumeURL = upstream.URL+"/usage", upstream.URL+"/credits", upstream.URL+"/consume"
	defer func() {
		codexUsageURL, codexResetCreditsURL, codexResetConsumeURL = restoreUsage, restoreCredits, restoreConsume
	}()

	cfg := &config.Config{}
	cfg.AuthDir = quotaAuthDir(t)
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandler(cfg, "", manager)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	auth := &coreauth.Auth{ID: "a", FileName: "a.json", Provider: "codex", Metadata: map[string]any{"access_token": "token", "account_id": "acct"}}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	h.StartCodexQuota(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rows := h.codexQuotaService().Rows(); len(rows) > 0 || time.Now().After(deadline) {
			break
		}
		runtime.Gosched()
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	check, _ := gin.CreateTestContext(recorder)
	check.Request = httptest.NewRequest(http.MethodGet, "/v0/management/codex-quota/reset-check?name=a.json", nil)
	h.GetCodexQuotaResetCheck(check)
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(`"count":1`)) {
		t.Fatalf("check = %d %s", recorder.Code, recorder.Body.String())
	}
	if got := quotaRequest(t, h.PostCodexQuotaReset, http.MethodPost, `{"name":""}`).Code; got != http.StatusBadRequest {
		t.Fatalf("empty name status = %d", got)
	}
	if got := quotaRequest(t, h.PostCodexQuotaReset, http.MethodPost, `{"name":"missing.json"}`).Code; got != http.StatusNotFound {
		t.Fatalf("unknown name status = %d", got)
	}
	response := quotaRequest(t, h.PostCodexQuotaReset, http.MethodPost, `{"name":"a.json"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", response.Code, response.Body.String())
	}
	if len(consumed) != 1 || consumed[0]["redeem_request_id"] == "" {
		t.Fatalf("consumed = %v", consumed)
	}
	if got := quotaRequest(t, h.PostCodexQuotaReset, http.MethodPost, `{"name":"a.json"}`).Code; got != http.StatusConflict {
		t.Fatalf("second reset status = %d", got)
	}
}
