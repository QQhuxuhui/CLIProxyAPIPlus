package helps

import (
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexWebsocketBridgeBucketHit(t *testing.T) {
	if CodexWebsocketBridgeBucketHit("req-1", 0) || CodexWebsocketBridgeBucketHit("req-1", -5) {
		t.Fatal("percent <= 0 must never bridge")
	}
	if !CodexWebsocketBridgeBucketHit("", 100) || !CodexWebsocketBridgeBucketHit("req-1", 150) {
		t.Fatal("percent >= 100 must always bridge")
	}
	if CodexWebsocketBridgeBucketHit("", 99) {
		t.Fatal("a request without an ID must not be sampled below 100 percent")
	}
	hits := 0
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("req-%d", i)
		first := CodexWebsocketBridgeBucketHit(id, 30)
		if first != CodexWebsocketBridgeBucketHit(id, 30) {
			t.Fatalf("bucket for %s is not stable", id)
		}
		if first {
			hits++
		}
	}
	if hits < 200 || hits > 400 {
		t.Fatalf("30 percent share hit %d of 1000", hits)
	}
}

func TestIsCodexWebsocketBridgeDroppedEvent(t *testing.T) {
	for payload, want := range map[string]bool{
		`{"type":"codex.rate_limits"}`:                 true,
		`{"type":"codex.response.metadata"}`:           true,
		`{"type":"responsesapi.websocket_timing"}`:     true,
		`{"type":"response.metadata"}`:                 false,
		`{"type":"response.completed"}`:                false,
		`{"type":"error","headers":{"retry-after":1}}`: false,
		`{}`: false,
	} {
		if got := IsCodexWebsocketBridgeDroppedEvent([]byte(payload)); got != want {
			t.Fatalf("IsCodexWebsocketBridgeDroppedEvent(%s) = %v, want %v", payload, got, want)
		}
	}
}

func TestFrameCodexWebsocketBridgeChunks(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"response.created","response":{}}`),
		[]byte(`data: {"no_type":true}`),
		[]byte(`: keepalive`),
	}
	got := FrameCodexWebsocketBridgeChunks(chunks)
	if string(got[0]) != "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{}}" {
		t.Fatalf("framed chunk = %q", got[0])
	}
	if string(got[1]) != `data: {"no_type":true}` || string(got[2]) != `: keepalive` {
		t.Fatalf("chunks without a type changed: %q", got[1:])
	}
}

func TestCleanCodexBridgeResponsesBody(t *testing.T) {
	body := CleanCodexBridgeResponsesBody([]byte(`{"model":"m","previous_response_id":"r","generate":false,"prompt_cache_retention":"24h","safety_identifier":"s","stream_options":{"include_obfuscation":false,"reasoning_summary_delivery":"inline"}}`))
	for _, field := range []string{"previous_response_id", "generate", "prompt_cache_retention", "safety_identifier", "stream_options.include_obfuscation"} {
		if gjson.GetBytes(body, field).Exists() {
			t.Fatalf("%s survived: %s", field, body)
		}
	}
	if gjson.GetBytes(body, "stream_options.reasoning_summary_delivery").String() != "inline" || gjson.GetBytes(body, "model").String() != "m" {
		t.Fatalf("kept fields changed: %s", body)
	}
	body = CleanCodexBridgeResponsesBody([]byte(`{"model":"m","stream_options":{"include_obfuscation":false}}`))
	if gjson.GetBytes(body, "stream_options").Exists() {
		t.Fatalf("empty stream_options survived: %s", body)
	}
}
