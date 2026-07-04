package helps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestExtractClaudeCodeSessionIDFromPayloadJSON(t *testing.T) {
	payload := []byte(`{"metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"cache-session-1\"}"}}`)
	got := ExtractClaudeCodeSessionID(context.Background(), payload, nil)
	if got != "cache-session-1" {
		t.Fatalf("ExtractClaudeCodeSessionID() = %q, want cache-session-1", got)
	}
}

func TestExtractClaudeCodeSessionIDFromHeader(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ginCtx.Request.Header.Set(ClaudeCodeSessionHeader, "header-session-1")
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	got := ExtractClaudeCodeSessionID(ctx, []byte(`{"model":"gpt-5.4"}`), nil)
	if got != "header-session-1" {
		t.Fatalf("ExtractClaudeCodeSessionID() = %q, want header-session-1", got)
	}
}

func TestClaudeCodePromptCacheStableAcrossRequests(t *testing.T) {
	// A caller identity is required now that prompt cache keys are scoped to the
	// authenticated caller; without one the cache is intentionally disabled.
	ctx := ctxWithCaller(t, "stable-caller")
	payload := []byte(`{"metadata":{"user_id":"{\"session_id\":\"cache-session-2\"}"}}`)
	first, ok, err := ClaudeCodePromptCache(ctx, "grok-composer-2.5-fast", payload, nil)
	if err != nil {
		t.Fatalf("ClaudeCodePromptCache first error: %v", err)
	}
	if !ok || first.ID == "" {
		t.Fatalf("ClaudeCodePromptCache first = %#v, ok=%v, want cached id", first, ok)
	}
	second, ok, err := ClaudeCodePromptCache(ctx, "grok-composer-2.5-fast", payload, nil)
	if err != nil {
		t.Fatalf("ClaudeCodePromptCache second error: %v", err)
	}
	if !ok || second.ID != first.ID {
		t.Fatalf("second cache id = %q, want %q", second.ID, first.ID)
	}
}

func TestExtractClaudeCodeSessionIDPrefersHeaderOverPayload(t *testing.T) {
	payload := []byte(`{"metadata":{"user_id":"{"session_id":"payload-session"}"}}`)
	headers := http.Header{}
	headers.Set(ClaudeCodeSessionHeader, "header-session")

	got := ExtractClaudeCodeSessionID(context.Background(), payload, headers)
	if got != "header-session" {
		t.Fatalf("ExtractClaudeCodeSessionID() = %q, want header-session", got)
	}
}

// TestClaudeCodePromptCacheIsolatesCallers asserts that two callers sharing the
// same X-Claude-Code-Session-Id do not cross-read each other's prompt cache
// entry. Reverting the ScopeSessionKeyToCaller call in ClaudeCodePromptCache
// makes both callers resolve the same cache id and fails this test.
func TestClaudeCodePromptCacheIsolatesCallers(t *testing.T) {
	headers := http.Header{}
	headers.Set(ClaudeCodeSessionHeader, "shared-claude-session")

	callerA, ok, err := ClaudeCodePromptCache(ctxWithCaller(t, "caller-A"), "grok-composer-2.5-fast", nil, headers)
	if err != nil || !ok || callerA.ID == "" {
		t.Fatalf("caller A cache = %#v, ok=%v, err=%v", callerA, ok, err)
	}
	callerB, ok, err := ClaudeCodePromptCache(ctxWithCaller(t, "caller-B"), "grok-composer-2.5-fast", nil, headers)
	if err != nil || !ok || callerB.ID == "" {
		t.Fatalf("caller B cache = %#v, ok=%v, err=%v", callerB, ok, err)
	}
	if callerA.ID == callerB.ID {
		t.Fatalf("different callers must not share prompt cache id: %q", callerA.ID)
	}

	// Same caller + same session id must stay stable (cache hit preserved).
	callerAAgain, ok, err := ClaudeCodePromptCache(ctxWithCaller(t, "caller-A"), "grok-composer-2.5-fast", nil, headers)
	if err != nil || !ok || callerAAgain.ID != callerA.ID {
		t.Fatalf("same caller must reuse cache id: got %q want %q (ok=%v err=%v)", callerAAgain.ID, callerA.ID, ok, err)
	}
}

// TestClaudeCodePromptCacheNoCallerDisablesCache asserts an unidentifiable
// caller yields no shared prompt cache (fail closed).
func TestClaudeCodePromptCacheNoCallerDisablesCache(t *testing.T) {
	headers := http.Header{}
	headers.Set(ClaudeCodeSessionHeader, "orphan-claude-session")

	cache, ok, err := ClaudeCodePromptCache(context.Background(), "grok-composer-2.5-fast", nil, headers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || cache.ID != "" {
		t.Fatalf("no-caller request must not receive a shared cache: cache=%#v ok=%v", cache, ok)
	}
}
