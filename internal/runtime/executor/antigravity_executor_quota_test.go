package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// quotaBody429 is the shared per-model quota 429 body used across CountTokens quota tests.
var quotaBody429 = []byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[` +
	`{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED",` +
	`"metadata":{"model":"gemini-pro-agent","quotaResetTimeStamp":"2026-07-03T13:11:31Z"}},` +
	`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"351085s"}]}}`)

// TestCountTokens_QuotaDetail_Site1 tests that a per-model quota 429 returned by the
// first (non-fallback) base URL carries QuotaDetail via newAntigravityStatusErr.
// RED before the fix (manual statusErr misses quota), GREEN after.
func TestCountTokens_QuotaDetail_Site1(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write(quotaBody429)
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "auth-count-tokens-quota-site1",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		},
	}

	_, err := exec.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatAntigravity,
	})
	if err == nil {
		t.Fatal("CountTokens() error = nil, want 429")
	}

	type quotaDetailer interface {
		QuotaDetail() (cliproxyexecutor.QuotaDetail, bool)
	}
	qd, ok := err.(quotaDetailer)
	if !ok {
		t.Fatalf("error does not implement QuotaDetail(): %T", err)
	}
	detail, detailOK := qd.QuotaDetail()
	if !detailOK {
		t.Fatal("QuotaDetail() ok=false, want ok=true (bug: quota not populated via newAntigravityStatusErr)")
	}
	if detail.Model != "gemini-pro-agent" {
		t.Errorf("detail.Model = %q, want %q", detail.Model, "gemini-pro-agent")
	}
	if detail.ReasonCode != "QUOTA_EXHAUSTED" {
		t.Errorf("detail.ReasonCode = %q, want %q", detail.ReasonCode, "QUOTA_EXHAUSTED")
	}
	wantReset, _ := time.Parse(time.RFC3339, "2026-07-03T13:11:31Z")
	if !detail.ResetAt.Equal(wantReset) {
		t.Errorf("detail.ResetAt = %v, want %v", detail.ResetAt, wantReset)
	}

	type retryAfterProvider interface {
		RetryAfter() *time.Duration
	}
	if ra, ok2 := err.(retryAfterProvider); !ok2 || ra.RetryAfter() == nil {
		t.Error("RetryAfter() should be non-nil for a 429 with retryDelay")
	}
}

// TestCountTokens_QuotaDetail_Site2 tests the post-loop lastStatus branch (site 2):
// when there are two base URLs and both return 429, the final error from the
// `case lastStatus != 0` branch must also carry QuotaDetail.
// RED before the fix, GREEN after.
func TestCountTokens_QuotaDetail_Site2(t *testing.T) {
	// Two servers, each returning 429, so the loop exhausts all base URLs and
	// the post-loop switch hits `case lastStatus != 0`.
	server1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write(quotaBody429)
	}))
	defer server1.Close()
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write(quotaBody429)
	}))
	defer server2.Close()

	exec := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "auth-count-tokens-quota-site2",
		Attributes: map[string]string{
			"base_url":          server1.URL,
			"fallback_base_url": server2.URL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		},
	}

	_, err := exec.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatAntigravity,
	})
	if err == nil {
		t.Fatal("CountTokens() error = nil, want 429")
	}

	type quotaDetailer interface {
		QuotaDetail() (cliproxyexecutor.QuotaDetail, bool)
	}
	qd, ok := err.(quotaDetailer)
	if !ok {
		t.Fatalf("error does not implement QuotaDetail(): %T", err)
	}
	detail, detailOK := qd.QuotaDetail()
	if !detailOK {
		t.Fatal("QuotaDetail() ok=false, want ok=true (bug: post-loop lastStatus branch misses quota)")
	}
	if detail.Model != "gemini-pro-agent" {
		t.Errorf("detail.Model = %q, want %q", detail.Model, "gemini-pro-agent")
	}
	if detail.ReasonCode != "QUOTA_EXHAUSTED" {
		t.Errorf("detail.ReasonCode = %q, want %q", detail.ReasonCode, "QUOTA_EXHAUSTED")
	}
	wantReset, _ := time.Parse(time.RFC3339, "2026-07-03T13:11:31Z")
	if !detail.ResetAt.Equal(wantReset) {
		t.Errorf("detail.ResetAt = %v, want %v", detail.ResetAt, wantReset)
	}

	type retryAfterProvider interface {
		RetryAfter() *time.Duration
	}
	if ra, ok2 := err.(retryAfterProvider); !ok2 || ra.RetryAfter() == nil {
		t.Error("RetryAfter() should be non-nil for a 429 with retryDelay")
	}
}

func TestNewAntigravityStatusErr_PopulatesQuotaDetail(t *testing.T) {
	body := []byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[` +
		`{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED",` +
		`"metadata":{"model":"gemini-pro-agent","quotaResetTimeStamp":"2026-07-03T13:11:31Z"}},` +
		`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"351085s"}]}}`)
	err := newAntigravityStatusErr(http.StatusTooManyRequests, body)
	detail, ok := err.QuotaDetail()
	if !ok {
		t.Fatal("expected QuotaDetail ok=true")
	}
	if detail.Model != "gemini-pro-agent" || detail.ReasonCode != "QUOTA_EXHAUSTED" {
		t.Errorf("detail = %+v", detail)
	}
	want, _ := time.Parse(time.RFC3339, "2026-07-03T13:11:31Z")
	if !detail.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %v, want %v", detail.ResetAt, want)
	}
}

func TestStatusErr_NoQuotaDetailByDefault(t *testing.T) {
	err := statusErr{code: 500, msg: "boom"}
	if _, ok := err.QuotaDetail(); ok {
		t.Error("expected ok=false when quota unset")
	}
}
