package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestCodexProbeFastSessionPreservesCooldownErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		quota  bool
	}{
		{"http rate limit", 429, `{"error":{"message":"rate limited"}}`, true},
		{"stream quota", 200, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"usage_limit_reached\",\"message\":\"quota exhausted\",\"resets_in_seconds\":120}}}\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
			before := time.Now()
			_, err := NewCodexExecutor(nil).ProbeFastSession(context.Background(), auth, "gpt-6-luna", "candidate")
			var status statusErr
			if !errors.As(err, &status) || status.StatusCode() != 429 || !status.IsCredentialScoped() {
				t.Fatalf("expected structured credential cooldown, got %T: %v", err, err)
			}
			if tc.quota {
				quota, ok := status.QuotaDetail()
				if !ok || quota.ReasonCode != "RATE_LIMIT_EXCEEDED" || quota.ResetAt.Before(before.Add(120*time.Second)) || quota.ResetAt.After(time.Now().Add(120*time.Second)) {
					t.Fatalf("lost rate window: %+v, %v", quota, ok)
				}
			} else if retry := status.RetryAfter(); retry == nil || *retry != 120*time.Second {
				t.Fatalf("lost quota retry delay: %v", retry)
			}
		})
	}
}

func TestCodexProbeFastSessionBypassesMappingAndStats(t *testing.T) {
	type captured struct {
		sessionID string
		body      []byte
	}
	got := make(chan captured, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- captured{sessionID: codexSessionHeaderValue(r.Header), body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"))
		w.(http.Flusher).Flush()
		// Keep the stream open: the probe must stop on its own after the first delta.
		<-r.Context().Done()
	}))
	defer server.Close()

	// The mapping is enabled with a non-empty pool; a probe must still send its own value.
	cfg := codexFastSessionTestConfig(true, 100, "fast-a")
	helps.CodexFastSessions().ApplyConfig(cfg.Codex.FastSessionIDs)
	before := helps.CodexFastSessions().Snapshot(10).Groups
	exec := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}

	timing, err := exec.ProbeFastSession(context.Background(), auth, "gpt-6-luna", "candidate-1")
	if err != nil || timing.Created <= 0 || timing.First < timing.Created {
		t.Fatalf("ProbeFastSession = %+v, %v", timing, err)
	}
	req := <-got
	if req.sessionID != "candidate-1" {
		t.Fatalf("Session-Id = %q, want candidate-1", req.sessionID)
	}
	if gjson.GetBytes(req.body, "model").String() != "gpt-6-luna" || gjson.GetBytes(req.body, "store").Bool() || gjson.GetBytes(req.body, "reasoning.effort").String() != "low" || gjson.GetBytes(req.body, "prompt_cache_key").String() != "candidate-1" {
		t.Fatalf("probe body = %s", req.body)
	}
	after := helps.CodexFastSessions().Snapshot(10).Groups
	for _, group := range []string{helps.CodexFastSessionGroupFast, helps.CodexFastSessionGroupControl} {
		if after[group].TotalRequests != before[group].TotalRequests {
			t.Fatalf("probe fed %s stats: %+v -> %+v", group, before[group], after[group])
		}
	}
}

func TestCodexProbeFastSessionReportsFailures(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"status": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
		},
		"in-stream failure": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{}}\n\n"))
		},
		"no output": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{}}\n\n"))
		},
	} {
		server := httptest.NewServer(handler)
		exec := NewCodexExecutor(codexFastSessionTestConfig(false, 0))
		auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
		_, err := exec.ProbeFastSession(context.Background(), auth, "gpt-6-luna", "candidate-1")
		server.Close()
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		if name == "status" && !strings.Contains(err.Error(), "429") {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
}
