package openai

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestRewriteOpenAIResponseModel(t *testing.T) {
	in := []byte(`{"id":"chatcmpl_x","object":"chat.completion","model":"gpt-5","choices":[]}`)
	out := rewriteOpenAIResponseModel(in, "g5")
	if !gjson.ValidBytes(out) {
		t.Fatalf("expected valid JSON, got: %q", string(out))
	}
	if got := gjson.GetBytes(out, "model").String(); got != "g5" {
		t.Fatalf("expected model %q, got %q", "g5", got)
	}
	if got := gjson.GetBytes(out, "id").String(); got != "chatcmpl_x" {
		t.Fatalf("expected id preserved, got %q", got)
	}
}

func TestRewriteOpenAIResponseModel_SkipsErrorPayload(t *testing.T) {
	in := []byte(`{"error":{"message":"boom"}}`)
	out := rewriteOpenAIResponseModel(in, "g5")
	if !bytes.Equal(bytes.TrimSpace(out), bytes.TrimSpace(in)) {
		t.Fatalf("expected payload unchanged, got: %q", string(out))
	}
}

func TestRewriteOpenAIStreamChunkModel_StripsDataPrefixAndRewritesModel(t *testing.T) {
	in := []byte(`data: {"id":"chatcmpl_x","object":"chat.completion.chunk","model":"gpt-5","choices":[]}`)
	out := rewriteOpenAIStreamChunkModel(in, "g5")
	if !gjson.ValidBytes(out) {
		t.Fatalf("expected valid JSON, got: %q", string(out))
	}
	if got := gjson.GetBytes(out, "model").String(); got != "g5" {
		t.Fatalf("expected model %q, got %q", "g5", got)
	}
}

func TestRewriteOpenAIStreamChunkModel_DonePassthrough(t *testing.T) {
	in := []byte(`[DONE]`)
	out := rewriteOpenAIStreamChunkModel(in, "g5")
	if string(out) != "[DONE]" {
		t.Fatalf("expected [DONE], got %q", string(out))
	}
}

func TestRewriteOpenAIResponsesStreamChunkModel_RewritesResponseModel(t *testing.T) {
	in := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-5\"}}\n\n")
	out := rewriteOpenAIResponsesStreamChunkModel(in, "g5")
	if !bytes.Contains(out, []byte(`"model":"g5"`)) {
		t.Fatalf("expected model rewritten, got: %q", string(out))
	}
	if bytes.Contains(out, []byte(`"model":"gpt-5"`)) {
		t.Fatalf("expected original model removed, got: %q", string(out))
	}
}

func TestRewriteOpenAIResponsesStreamChunkModel_RewritesRootModel(t *testing.T) {
	in := []byte("data: {\"type\":\"response.created\",\"model\":\"gpt-5\"}\n")
	out := rewriteOpenAIResponsesStreamChunkModel(in, "g5")
	if !bytes.Contains(out, []byte(`"model":"g5"`)) {
		t.Fatalf("expected model rewritten, got: %q", string(out))
	}
}
