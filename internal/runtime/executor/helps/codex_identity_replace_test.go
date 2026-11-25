package helps

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestReplaceCodexIdentityInPayloadLeavesProtocolFields(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"status":"completed","prompt_cache_key":"completed","note":"key completed:0"}}`)
	got := ReplaceCodexIdentityInPayload(payload, "completed", "uuid-1")
	if gjson.GetBytes(got, "type").String() != "response.completed" || gjson.GetBytes(got, "response.status").String() != "completed" {
		t.Fatalf("protocol fields rewritten: %s", got)
	}
	if gjson.GetBytes(got, "response.prompt_cache_key").String() != "uuid-1" || gjson.GetBytes(got, "response.note").String() != "key uuid-1:0" {
		t.Fatalf("identity values not replaced: %s", got)
	}
}

func TestReplaceCodexIdentityInPayloadLeavesKeysAndErrorShape(t *testing.T) {
	payload := []byte(`{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"error","message":"error for key error"}}`)
	got := ReplaceCodexIdentityInPayload(payload, "error", "uuid-2")
	if !gjson.GetBytes(got, "error").IsObject() || gjson.GetBytes(got, "type").String() != "error" || gjson.GetBytes(got, "error.code").String() != "error" || gjson.GetBytes(got, "error.type").String() != "rate_limit_error" {
		t.Fatalf("error shape changed: %s", got)
	}
	if gjson.GetBytes(got, "error.message").String() != "uuid-2 for key uuid-2" {
		t.Fatalf("message not replaced: %s", got)
	}
}

func TestReplaceCodexIdentityInPayloadKeepsJSONValid(t *testing.T) {
	original := `ke"y\with`
	payload := []byte(`data: {"type":"response.created","response":{"prompt_cache_key":"uuid-3"}}`)
	got := ReplaceCodexIdentityInPayload(payload, "uuid-3", original)
	data := got[len("data: "):]
	if !json.Valid(data) {
		t.Fatalf("invalid JSON after restore: %s", got)
	}
	if gjson.GetBytes(data, "response.prompt_cache_key").String() != original {
		t.Fatalf("restored value = %q, want %q", gjson.GetBytes(data, "response.prompt_cache_key").String(), original)
	}
	back := ReplaceCodexIdentityInPayload(got, original, "uuid-3")
	if gjson.GetBytes(back[len("data: "):], "response.prompt_cache_key").String() != "uuid-3" {
		t.Fatalf("escaped original not found again: %s", back)
	}
}

func TestReplaceCodexIdentityInPayloadPlainTextAndEventLines(t *testing.T) {
	if got := string(ReplaceCodexIdentityInPayload([]byte("event: response.completed"), "completed", "x")); got != "event: response.completed" {
		t.Fatalf("event line rewritten: %q", got)
	}
	if got := string(ReplaceCodexIdentityInPayload([]byte("bad key k1 here"), "k1", "x")); got != "bad key x here" {
		t.Fatalf("plain text not replaced: %q", got)
	}
	unchanged := []byte(`{"a":"b"}`)
	if got := ReplaceCodexIdentityInPayload(unchanged, "zzz", "x"); &got[0] != &unchanged[0] {
		t.Fatal("payload without the value was copied")
	}
}
