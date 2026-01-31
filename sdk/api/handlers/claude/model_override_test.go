package claude

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestRewriteClaudeResponseModel(t *testing.T) {
	in := []byte(`{"id":"msg_x","type":"message","role":"assistant","model":"claude-sonnet","content":[]}`)
	out := rewriteClaudeResponseModel(in, "cs")
	if !gjson.ValidBytes(out) {
		t.Fatalf("expected valid JSON, got: %q", string(out))
	}
	if got := gjson.GetBytes(out, "model").String(); got != "cs" {
		t.Fatalf("expected model %q, got %q", "cs", got)
	}
}

func TestRewriteClaudeStreamChunkModel_RewritesMessageStartDataLine(t *testing.T) {
	// One-line-per-chunk shape (common when upstream is Claude).
	in := []byte("data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet\"}}\n")
	out := rewriteClaudeStreamChunkModel(in, "cs")
	if !bytes.Contains(out, []byte(`"model":"cs"`)) {
		t.Fatalf("expected model rewritten, got: %q", string(out))
	}
}

func TestRewriteClaudeStreamChunkModel_MultiLineChunk(t *testing.T) {
	in := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet\"}}\n\n")
	out := rewriteClaudeStreamChunkModel(in, "cs")
	if !bytes.Contains(out, []byte(`"model":"cs"`)) {
		t.Fatalf("expected model rewritten, got: %q", string(out))
	}
}

