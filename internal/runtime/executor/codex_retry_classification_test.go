package executor

import (
	"context"
	"net/http"
	"testing"
)

func TestCodexRetryBootstrapPreservesRateLimitStatus(t *testing.T) {
	err := newCodexBootstrapOverloadErr([]byte(`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`))
	if err.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", err.StatusCode())
	}
}

func TestCodexRetryBootstrapPreservesSourceAndRateLearning(t *testing.T) {
	err := newCodexBootstrapOverloadErr([]byte(`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`))
	_, body, source := err.CodexFailureInfo()
	if source != "stream" || body == "" {
		t.Fatalf("lost original failure source: %q %q", source, body)
	}
	quota, ok := err.QuotaDetail()
	if !ok || quota.ReasonCode != "RATE_LIMIT_EXCEEDED" {
		t.Fatalf("missing rate-learning hint: %+v", quota)
	}
}

func TestCodexRetryBootstrapUsageLimitRotatesBeforeOutput(t *testing.T) {
	server := codexSSEServer(codexCreatedEvent, `{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"quota exhausted","resets_in_seconds":30}}}`)
	defer server.Close()
	req, opts := codexTestRequest()
	result, err := NewCodexExecutor(codexBufferingConfig(true)).ExecuteStream(context.Background(), codexTestAuth(server.URL), req, opts)
	if err == nil || result != nil {
		if result != nil {
			for range result.Chunks {
			}
		}
		t.Fatal("usage-limit failure must rotate before emitting handshake")
	}
	if statusCodeFromTestError(t, err) != 429 {
		t.Fatalf("error=%v want 429", err)
	}
}
