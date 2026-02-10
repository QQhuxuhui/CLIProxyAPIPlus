package claude

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

func TestBuildClaudeResponse_UsesSeparateCacheReadAndCreationTokens(t *testing.T) {
	usageInfo := usage.Detail{
		InputTokens:         100,
		OutputTokens:        20,
		CachedTokens:        30,
		CacheCreationTokens: 7,
	}

	resp := BuildClaudeResponse("hello", nil, "claude-3-5-sonnet", usageInfo, "end_turn")
	parsed := gjson.ParseBytes(resp)

	if got := parsed.Get("usage.cache_read_input_tokens").Int(); got != usageInfo.CachedTokens {
		t.Fatalf("cache_read_input_tokens = %d, want %d", got, usageInfo.CachedTokens)
	}
	if got := parsed.Get("usage.cache_creation_input_tokens").Int(); got != usageInfo.CacheCreationTokens {
		t.Fatalf("cache_creation_input_tokens = %d, want %d", got, usageInfo.CacheCreationTokens)
	}
}

func TestBuildClaudeMessageDeltaEvent_UsesSeparateCacheReadAndCreationTokens(t *testing.T) {
	usageInfo := usage.Detail{
		InputTokens:         256,
		OutputTokens:        64,
		CachedTokens:        128,
		CacheCreationTokens: 19,
	}

	event := BuildClaudeMessageDeltaEvent("end_turn", usageInfo)
	text := string(event)
	jsonText := strings.TrimPrefix(text, "event: message_delta\ndata: ")
	parsed := gjson.Parse(jsonText)

	if got := parsed.Get("usage.cache_read_input_tokens").Int(); got != usageInfo.CachedTokens {
		t.Fatalf("cache_read_input_tokens = %d, want %d", got, usageInfo.CachedTokens)
	}
	if got := parsed.Get("usage.cache_creation_input_tokens").Int(); got != usageInfo.CacheCreationTokens {
		t.Fatalf("cache_creation_input_tokens = %d, want %d", got, usageInfo.CacheCreationTokens)
	}
}
