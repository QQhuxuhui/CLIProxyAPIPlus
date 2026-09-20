package auth

import (
	"context"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestAfterAuthInterceptionRunsForEachAttempt(t *testing.T) {
	body := []byte(`{"session_id":"fixture"}`)
	var providers []string
	opts := cliproxyexecutor.Options{
		OriginalRequest:                     body,
		RequestAfterAuthInterceptorReadOnly: true,
		RequestAfterAuthInterceptor: func(_ context.Context, req cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
			providers = append(providers, req.Provider)
			if &req.Body[0] != &body[0] {
				t.Fatal("read-only interceptor received a clone")
			}
			return cliproxyexecutor.RequestAfterAuthInterceptResponse{}
		},
	}
	for _, provider := range []string{"codex", "antigravity"} {
		req, gotOpts, err := applyRequestAfterAuthInterceptor(context.Background(), nil, provider, cliproxyexecutor.Request{Payload: body}, opts, "model")
		if err != nil || &req.Payload[0] != &body[0] || &gotOpts.OriginalRequest[0] != &body[0] {
			t.Fatal("no-op attempt copied body")
		}
	}
	if len(providers) != 2 || providers[0] != "codex" || providers[1] != "antigravity" {
		t.Fatalf("attempt interception was cached: %v", providers)
	}
}

func TestAfterAuthNativeCallbackInputRemainsIsolated(t *testing.T) {
	body := []byte(`{"session_id":"fixture"}`)
	opts := cliproxyexecutor.Options{RequestAfterAuthInterceptor: func(_ context.Context, req cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
		req.Body[0] = 'X'
		return cliproxyexecutor.RequestAfterAuthInterceptResponse{}
	}}
	req, _, err := applyRequestAfterAuthInterceptor(context.Background(), nil, "codex", cliproxyexecutor.Request{Payload: body}, opts, "model")
	if err != nil || req.Payload[0] != '{' || body[0] != '{' {
		t.Fatal("native callback mutated the caller's payload")
	}
}

func TestAfterAuthHeaderOnlyChangePreservesBodySessionPriority(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"{\"session_id\":\"claude-body\"}"}}`)
	opts := cliproxyexecutor.Options{
		OriginalRequest: body,
		Headers:         http.Header{"X-Session-Id": {"generic"}},
		Metadata:        map[string]any{cliproxyexecutor.CanonicalSessionIDMetadataKey: "claude:claude-body"},
		RequestAfterAuthInterceptor: func(context.Context, cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
			return cliproxyexecutor.RequestAfterAuthInterceptResponse{Headers: http.Header{"X-Trace-Id": {"trace"}}}
		},
	}
	_, got, err := applyRequestAfterAuthInterceptor(context.Background(), nil, "claude", cliproxyexecutor.Request{Payload: body}, opts, "model")
	if err != nil || got.Metadata[cliproxyexecutor.CanonicalSessionIDMetadataKey] != "claude:claude-body" {
		t.Fatalf("header-only plugin changed session priority: %v, %v", got.Metadata, err)
	}
}
