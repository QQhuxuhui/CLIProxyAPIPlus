package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/latencytier"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestLatencyTierEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}

	// Not installed -> 503.
	latencytier.SetDefault(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/latency-tier", nil)
	h.GetLatencyTier(c)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when not installed, got %d", w.Code)
	}

	tracker := latencytier.New(nil, latencytier.DefaultConfig(), "")
	latencytier.SetDefault(tracker)
	defer latencytier.SetDefault(nil)
	tracker.HandleUsage(context.Background(), usage.Record{Provider: "codex", AuthType: "oauth", AuthID: "a1", Model: "gpt-6-luna", TTFT: 1200 * time.Millisecond, Stream: true, Detail: usage.Detail{OutputTokens: 2, TotalTokens: 5}})

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/latency-tier/recompute", nil)
	h.PostLatencyTierRecompute(c)
	if w.Code != http.StatusOK {
		t.Fatalf("recompute status %d: %s", w.Code, w.Body.String())
	}
	var snap latencytier.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Tracked != 1 || len(snap.Accounts) != 1 || snap.Accounts[0].Samples != 1 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}

	// Enable through PUT with a partial body: untouched fields keep defaults.
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/latency-tier/config", strings.NewReader(`{"enabled":true,"min_samples":3}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PutLatencyTierConfig(c)
	if w.Code != http.StatusOK {
		t.Fatalf("put status %d: %s", w.Code, w.Body.String())
	}
	cfg := tracker.Config()
	if !cfg.Enabled || cfg.MinSamples != 3 || cfg.HalfLifeSeconds != latencytier.DefaultConfig().HalfLifeSeconds {
		t.Fatalf("config not merged: %+v", cfg)
	}

	// Invalid parameters are rejected and leave the config untouched.
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/latency-tier/config", strings.NewReader(`{"fast_exit_percent":0.05}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PutLatencyTierConfig(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid config, got %d", w.Code)
	}
	if tracker.Config().FastExitPercent != latencytier.DefaultConfig().FastExitPercent {
		t.Fatalf("invalid config must not be applied")
	}
}
