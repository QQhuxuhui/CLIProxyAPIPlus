package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestFetchAntigravityCredits_RequestBody(t *testing.T) {
	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request body: %v", errRead)
		}
		bodyCh <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"paidTier":{"id":"pro"}}`))
	}))
	defer server.Close()

	auth := &coreauth.Auth{
		ID:       "antigravity-test",
		Provider: "antigravity",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
	}

	if _, _, errFetch := (&Handler{}).fetchAntigravityCredits(context.Background(), auth, "test-token"); errFetch != nil {
		t.Fatalf("fetchAntigravityCredits() error = %v", errFetch)
	}

	var payload struct {
		Metadata struct {
			IDEType string `json:"ideType"`
		} `json:"metadata"`
	}
	if errDecode := json.Unmarshal(<-bodyCh, &payload); errDecode != nil {
		t.Fatalf("request body is not valid JSON: %v", errDecode)
	}
	if payload.Metadata.IDEType != "ANTIGRAVITY" {
		t.Fatalf("metadata.ideType = %q, want ANTIGRAVITY", payload.Metadata.IDEType)
	}
}

func TestAntigravityCreditsHTTPClient_NoTimeout(t *testing.T) {
	client := (&Handler{}).antigravityCreditsHTTPClient(nil)
	if client.Timeout != 0 {
		t.Fatalf("Timeout = %s, want 0", client.Timeout)
	}
}

func TestParseAntigravityCredits(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantCache     bool
		wantKnown     bool
		wantAvailable bool
		wantTier      string
	}{
		{
			name:          "valid credits",
			body:          `{"paidTier":{"id":"pro","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"10","minimumCreditAmountForUsage":"1"}]}}`,
			wantCache:     true,
			wantKnown:     true,
			wantAvailable: true,
			wantTier:      "pro",
		},
		{
			name:      "missing array",
			body:      `{"paidTier":{"id":"free"}}`,
			wantCache: true,
			wantKnown: true,
			wantTier:  "free",
		},
		{
			name:      "unmatched credits",
			body:      `{"paidTier":{"id":"pro","availableCredits":[{"creditType":"OTHER","creditAmount":"10","minimumCreditAmountForUsage":"1"}]}}`,
			wantCache: false,
			wantKnown: false,
			wantTier:  "pro",
		},
		{
			name:      "malformed target",
			body:      `{"paidTier":{"id":"pro","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"bad","minimumCreditAmountForUsage":"1"}]}}`,
			wantCache: false,
			wantKnown: false,
			wantTier:  "pro",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hint, cacheable := parseAntigravityCredits([]byte(tt.body))
			if cacheable != tt.wantCache {
				t.Errorf("cacheable = %t, want %t", cacheable, tt.wantCache)
			}
			if hint.Known != tt.wantKnown {
				t.Errorf("Known = %t, want %t", hint.Known, tt.wantKnown)
			}
			if hint.Available != tt.wantAvailable {
				t.Errorf("Available = %t, want %t", hint.Available, tt.wantAvailable)
			}
			if hint.PaidTierID != tt.wantTier {
				t.Errorf("PaidTierID = %q, want %q", hint.PaidTierID, tt.wantTier)
			}
		})
	}
}

func TestRefreshAntigravityCredits_DoesNotCacheUnmatchedCredits(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	if errConfigure := coreauth.ConfigureAntigravityPlanStore(context.Background(), coreauth.NewFileAntigravityPlanStore(t.TempDir())); errConfigure != nil {
		t.Fatalf("configure plan store: %v", errConfigure)
	}
	t.Cleanup(func() {
		_ = coreauth.ConfigureAntigravityPlanStore(context.Background(), nil)
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"paidTier":{"id":"pro","availableCredits":[{"creditType":"OTHER","creditAmount":"10","minimumCreditAmountForUsage":"1"}]}}`))
	}))
	defer server.Close()

	auth := &coreauth.Auth{
		ID:       "antigravity-unmatched-credits",
		Provider: "antigravity",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"access_token": "test-token",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	authIndex := auth.EnsureIndex()
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/antigravity-credits/refresh", strings.NewReader(`{"auth_index":"`+authIndex+`"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.RefreshAntigravityCredits(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d with body %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if hint, ok := coreauth.GetAntigravityCreditsHint(auth.ID); ok {
		t.Fatalf("unexpected cached hint: %+v", hint)
	}
	if record, ok := coreauth.GetAntigravityDisplayPlan(auth.ID); !ok || record.PaidTierID != "pro" {
		t.Fatalf("display plan = %#v, %t; want pro", record, ok)
	}
}
