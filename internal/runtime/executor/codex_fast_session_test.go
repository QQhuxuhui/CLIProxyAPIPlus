package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

const codexFastSessionTestCompleted = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n"

func codexFastSessionTestConfig(enable bool, percent int, seed ...string) *config.Config {
	cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
	cfg.Codex.FastSessionIDs = config.CodexFastSessionIDsConfig{Enable: enable, Percent: percent, Seed: seed}
	return cfg
}

type codexFastSessionCapture struct {
	sessionID string
	body      []byte
}

func runCodexFastSessionHTTP(t *testing.T, cfg *config.Config, stream bool) codexFastSessionCapture {
	t.Helper()
	captured := make(chan codexFastSessionCapture, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured <- codexFastSessionCapture{sessionID: codexSessionHeaderValue(r.Header), body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexFastSessionTestCompleted))
	}))
	defer server.Close()

	exec := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","prompt_cache_key":"conv-original","input":[{"type":"message","role":"user","content":"hi"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: stream}
	if stream {
		result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
		if err != nil {
			t.Fatalf("ExecuteStream error: %v", err)
		}
		for range result.Chunks {
		}
	} else if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	return <-captured
}

func TestCodexFastSessionRewritesHTTPHeaderOnly(t *testing.T) {
	seed := []string{"fast-a", "fast-b", "fast-c"}
	for _, stream := range []bool{true, false} {
		before := helps.CodexFastSessions().Snapshot(10).Groups[helps.CodexFastSessionGroupFast].TotalRequests
		got := runCodexFastSessionHTTP(t, codexFastSessionTestConfig(true, 100, seed...), stream)
		if !slices.Contains(seed, got.sessionID) {
			t.Fatalf("stream=%t: Session-Id = %q, want a pool member", stream, got.sessionID)
		}
		if key := gjson.GetBytes(got.body, "prompt_cache_key").String(); key != "conv-original" {
			t.Fatalf("stream=%t: body prompt_cache_key = %q, want conv-original", stream, key)
		}
		after := helps.CodexFastSessions().Snapshot(10).Groups[helps.CodexFastSessionGroupFast].TotalRequests
		if after != before+1 {
			t.Fatalf("stream=%t: fast group requests = %d, want %d", stream, after, before+1)
		}
	}
}

func TestCodexFastSessionPassthroughHTTP(t *testing.T) {
	cases := map[string]*config.Config{
		"disabled":   codexFastSessionTestConfig(false, 100, "fast-a"),
		"empty pool": codexFastSessionTestConfig(true, 100),
		"control":    codexFastSessionTestConfig(true, 0, "fast-a"),
	}
	for name, cfg := range cases {
		got := runCodexFastSessionHTTP(t, cfg, true)
		if got.sessionID != "conv-original" {
			t.Fatalf("%s: Session-Id = %q, want conv-original", name, got.sessionID)
		}
	}
}

func TestCodexFastSessionDoesNotTouchWebsocket(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedSession := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedSession <- codexSessionHeaderValue(r.Header)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		completed := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		_ = conn.WriteMessage(websocket.TextMessage, completed)
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(codexFastSessionTestConfig(true, 100, "fast-a", "fast-b"))
	auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","prompt_cache_key":"conv-original","input":[{"role":"user","content":"hello"}]}`),
	}
	if _, err := exec.Execute(context.Background(), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	select {
	case got := <-capturedSession:
		if got != "conv-original" {
			t.Fatalf("websocket session header = %q, want conv-original", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for websocket upgrade")
	}
}
