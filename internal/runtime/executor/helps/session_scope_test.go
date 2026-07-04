package helps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func ctxWithCaller(t *testing.T, apiKey string) context.Context {
	t.Helper()
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if apiKey != "" {
		ginCtx.Set("userApiKey", apiKey)
	}
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func TestScopeSessionKeyToCallerIsolatesDifferentCallers(t *testing.T) {
	a := ScopeSessionKeyToCaller(ctxWithCaller(t, "caller-A"), "shared-session")
	b := ScopeSessionKeyToCaller(ctxWithCaller(t, "caller-B"), "shared-session")
	if a == "" || b == "" {
		t.Fatalf("expected non-empty scoped keys, got a=%q b=%q", a, b)
	}
	if a == b {
		t.Fatalf("different callers must not share a scoped key: a=%q b=%q", a, b)
	}
}

func TestScopeSessionKeyToCallerStableForSameCaller(t *testing.T) {
	first := ScopeSessionKeyToCaller(ctxWithCaller(t, "caller-A"), "shared-session")
	second := ScopeSessionKeyToCaller(ctxWithCaller(t, "caller-A"), "shared-session")
	if first == "" {
		t.Fatalf("expected non-empty scoped key")
	}
	if first != second {
		t.Fatalf("same caller+session must be stable: first=%q second=%q", first, second)
	}
}

func TestScopeSessionKeyToCallerEmptyRaw(t *testing.T) {
	if got := ScopeSessionKeyToCaller(ctxWithCaller(t, "caller-A"), ""); got != "" {
		t.Fatalf("empty raw must yield empty scoped key, got %q", got)
	}
	if got := ScopeSessionKeyToCaller(ctxWithCaller(t, "caller-A"), "   "); got != "" {
		t.Fatalf("whitespace raw must yield empty scoped key, got %q", got)
	}
}

func TestScopeSessionKeyToCallerNoCallerFailsClosed(t *testing.T) {
	if got := ScopeSessionKeyToCaller(context.Background(), "shared-session"); got != "" {
		t.Fatalf("missing caller must fail closed with empty key, got %q", got)
	}
	if got := ScopeSessionKeyToCaller(ctxWithCaller(t, ""), "shared-session"); got != "" {
		t.Fatalf("empty caller must fail closed with empty key, got %q", got)
	}
}
