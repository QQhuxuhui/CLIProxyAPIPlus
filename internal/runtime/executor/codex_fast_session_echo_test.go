package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type codexFastSessionEchoResult struct {
	sentSessionID string
	client        []byte
}

// runCodexFastSessionEcho sends one request through the HTTP executor against an upstream that
// echoes echo(sentSessionID) as the response prompt_cache_key, and returns what the client got.
func runCodexFastSessionEcho(t *testing.T, cfg *config.Config, stream bool, echo func(sent string) string, authIDs ...string) codexFastSessionEchoResult {
	t.Helper()
	sent := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessionID := codexSessionHeaderValue(r.Header)
		sent <- sessionID
		key := echo(sessionID)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","prompt_cache_key":"` + key + `","output":[]}}` + "\n\n" +
			`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","prompt_cache_key":"` + key + `","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
	}))
	defer server.Close()

	exec := NewCodexExecutor(cfg)
	auth := &cliproxyauth.Auth{ID: "auth-1", Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	if len(authIDs) > 0 {
		auth.ID = authIDs[0]
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","prompt_cache_key":"conv-original","input":[{"type":"message","role":"user","content":"hi"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: stream}
	var client []byte
	if stream {
		result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
		if err != nil {
			t.Fatalf("ExecuteStream error: %v", err)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("stream chunk error: %v", chunk.Err)
			}
			client = append(client, chunk.Payload...)
			client = append(client, '\n')
		}
	} else {
		resp, err := exec.Execute(context.Background(), auth, req, opts)
		if err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		client = resp.Payload
	}
	return codexFastSessionEchoResult{sentSessionID: <-sent, client: client}
}

func echoSentSessionID(sent string) string { return sent }

func TestCodexFastSessionRestoresEchoedPromptCacheKey(t *testing.T) {
	seed := []string{"fast-a", "fast-b", "fast-c"}
	for _, stream := range []bool{true, false} {
		got := runCodexFastSessionEcho(t, codexFastSessionTestConfig(true, 100, seed...), stream, echoSentSessionID)
		if !slices.Contains(seed, got.sentSessionID) {
			t.Fatalf("stream=%t: sent Session-Id = %q, want a pool member", stream, got.sentSessionID)
		}
		if bytes.Contains(got.client, []byte(got.sentSessionID)) {
			t.Fatalf("stream=%t: fast value %q leaked to client: %s", stream, got.sentSessionID, got.client)
		}
		if !bytes.Contains(got.client, []byte(`"prompt_cache_key":"conv-original"`)) {
			t.Fatalf("stream=%t: client did not receive the original prompt_cache_key: %s", stream, got.client)
		}
	}
}

func TestCodexFastSessionLeavesEchoUntouchedOutsideFastGroup(t *testing.T) {
	echoFastA := func(string) string { return "fast-a" }
	cases := map[string]*config.Config{
		"off":     codexFastSessionTestConfig(false, 100, "fast-a"),
		"control": codexFastSessionTestConfig(true, 0, "fast-a"),
	}
	for name, cfg := range cases {
		for _, stream := range []bool{true, false} {
			got := runCodexFastSessionEcho(t, cfg, stream, echoFastA)
			if got.sentSessionID != "conv-original" {
				t.Fatalf("%s stream=%t: sent Session-Id = %q, want conv-original", name, stream, got.sentSessionID)
			}
			if !bytes.Contains(got.client, []byte(`"prompt_cache_key":"fast-a"`)) {
				t.Fatalf("%s stream=%t: echo must be untouched: %s", name, stream, got.client)
			}
		}
	}
}

func TestCodexFastSessionLeavesOtherEchoUntouched(t *testing.T) {
	echoOther := func(string) string { return "other-key" }
	for _, stream := range []bool{true, false} {
		got := runCodexFastSessionEcho(t, codexFastSessionTestConfig(true, 100, "fast-a"), stream, echoOther)
		if got.sentSessionID != "fast-a" {
			t.Fatalf("stream=%t: sent Session-Id = %q, want fast-a", stream, got.sentSessionID)
		}
		if !bytes.Contains(got.client, []byte(`"prompt_cache_key":"other-key"`)) {
			t.Fatalf("stream=%t: other echo must be untouched: %s", stream, got.client)
		}
	}
}

func TestCodexFastSessionRestoreWithIdentityConfuse(t *testing.T) {
	cfg := codexFastSessionTestConfig(true, 100, "fast-a", "fast-b")
	cfg.Codex.IdentityConfuse = true
	cfg.Routing.SessionAffinity = true
	confused := codexIdentityConfuseUUID("auth-1", "prompt-cache", "conv-original")
	for _, stream := range []bool{true, false} {
		got := runCodexFastSessionEcho(t, cfg, stream, echoSentSessionID)
		if got.sentSessionID != "fast-a" && got.sentSessionID != "fast-b" {
			t.Fatalf("stream=%t: sent Session-Id = %q, want a pool member", stream, got.sentSessionID)
		}
		if bytes.Contains(got.client, []byte(got.sentSessionID)) || bytes.Contains(got.client, []byte(confused)) {
			t.Fatalf("stream=%t: internal key leaked to client: %s", stream, got.client)
		}
		if !bytes.Contains(got.client, []byte(`"prompt_cache_key":"conv-original"`)) {
			t.Fatalf("stream=%t: client did not receive the original prompt_cache_key: %s", stream, got.client)
		}
	}
}

func TestCodexFastSessionRestoreResponsePayloadIsTargeted(t *testing.T) {
	state := codexFastSessionState{original: "orig", fast: "fast-a"}
	payload := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"object":"response","prompt_cache_key":"fast-a"}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"fast-a"}`,
		`{"object":"response","prompt_cache_key":"fast-a","metadata":{"prompt_cache_key":"fast-a"}}`,
		`{"object":"other","prompt_cache_key":"fast-a"}`,
		`data: {"type":"response.completed","response":{"prompt_cache_key":"fast-ab"}}`,
	}, "\n")
	got := strings.Split(string(state.restoreResponsePayload([]byte(payload))), "\n")
	if want := `data: {"type":"response.created","response":{"object":"response","prompt_cache_key":"orig"}}`; got[1] != want {
		t.Fatalf("response.created = %s, want %s", got[1], want)
	}
	if got[0] != "event: response.created" || got[2] != "" {
		t.Fatalf("non-data lines changed: %q", got[:3])
	}
	if !strings.Contains(got[3], `"delta":"fast-a"`) {
		t.Fatalf("text delta must be untouched: %s", got[3])
	}
	if gjson.Get(got[4], "prompt_cache_key").String() != "orig" || gjson.Get(got[4], "metadata.prompt_cache_key").String() != "fast-a" {
		t.Fatalf("top-level response object = %s", got[4])
	}
	if gjson.Get(got[5], "prompt_cache_key").String() != "fast-a" {
		t.Fatalf("non-response object must be untouched: %s", got[5])
	}
	if gjson.Get(got[6], "response.prompt_cache_key").String() != "fast-ab" {
		t.Fatalf("different value must be untouched: %s", got[6])
	}

	if out := (codexFastSessionState{}).restoreResponsePayload([]byte(payload)); string(out) != payload {
		t.Fatal("empty state must not change the payload")
	}
}

func TestCodexFastSessionCanaryStableAcrossAuthSwitch(t *testing.T) {
	seed := []string{"fast-a", "fast-b"}
	cfg := codexFastSessionTestConfig(true, 50, seed...)
	cfg.Codex.IdentityConfuse = true
	cfg.Routing.SessionAffinity = true
	var firstGroup bool
	var firstFast string
	for i := 0; i < 20; i++ {
		got := runCodexFastSessionEcho(t, cfg, true, echoSentSessionID, fmt.Sprintf("auth-%d", i))
		fast := slices.Contains(seed, got.sentSessionID)
		if i == 0 {
			firstGroup = fast
			firstFast = got.sentSessionID
		}
		if fast != firstGroup || (fast && got.sentSessionID != firstFast) {
			t.Fatalf("same conversation moved after account switch: first=%s now=%s", firstFast, got.sentSessionID)
		}
		if !bytes.Contains(got.client, []byte(`"prompt_cache_key":"conv-original"`)) {
			t.Fatalf("client identity changed: %s", got.client)
		}
	}
}
