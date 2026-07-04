package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func codexCtxWithCaller(t *testing.T, apiKey string) context.Context {
	t.Helper()
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if apiKey != "" {
		ginCtx.Set("userApiKey", apiKey)
	}
	return context.WithValue(context.Background(), "gin", ginCtx)
}

// TestCodexReasoningReplaySessionKeyIsolatesCallers proves that the same
// client-supplied session id (X-Codex-Window-Id) resolves to different reasoning
// replay session keys for different authenticated callers. Removing the
// helps.ScopeSessionKeyToCaller wrapper in codexReasoningReplaySessionKey (so it
// returns the raw "window:shared-codex-window" value) makes both callers equal
// and fails this test.
func TestCodexReasoningReplaySessionKeyIsolatesCallers(t *testing.T) {
	from := sdktranslator.FromString("claude")
	req := cliproxyexecutor.Request{}
	opts := cliproxyexecutor.Options{Headers: http.Header{}}
	opts.Headers.Set("X-Codex-Window-Id", "shared-codex-window")

	keyA := codexReasoningReplaySessionKey(codexCtxWithCaller(t, "caller-A"), from, req, opts, nil)
	keyB := codexReasoningReplaySessionKey(codexCtxWithCaller(t, "caller-B"), from, req, opts, nil)
	if keyA == "" || keyB == "" {
		t.Fatalf("expected non-empty session keys, got keyA=%q keyB=%q", keyA, keyB)
	}
	if keyA == keyB {
		t.Fatalf("different callers must not share a reasoning replay session key: %q", keyA)
	}

	keyAAgain := codexReasoningReplaySessionKey(codexCtxWithCaller(t, "caller-A"), from, req, opts, nil)
	if keyAAgain != keyA {
		t.Fatalf("same caller+session must be stable: got %q want %q", keyAAgain, keyA)
	}
}

// TestCodexReasoningReplaySessionKeyNoCallerFailsClosed proves that a request
// with no identifiable caller resolves to an empty session key (cache disabled).
func TestCodexReasoningReplaySessionKeyNoCallerFailsClosed(t *testing.T) {
	from := sdktranslator.FromString("claude")
	req := cliproxyexecutor.Request{}
	opts := cliproxyexecutor.Options{Headers: http.Header{}}
	opts.Headers.Set("Session_id", "shared-codex-session")

	if key := codexReasoningReplaySessionKey(context.Background(), from, req, opts, nil); key != "" {
		t.Fatalf("no-caller request must fail closed with empty session key, got %q", key)
	}
}
