package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// streamQuotaErr simulates an error that arrives mid-stream carrying structured quota info.
type streamQuotaErr struct {
	reset time.Time
}

func (e streamQuotaErr) Error() string   { return "mid-stream quota exhausted" }
func (e streamQuotaErr) StatusCode() int { return 429 }
func (e streamQuotaErr) QuotaDetail() (cliproxyexecutor.QuotaDetail, bool) {
	return cliproxyexecutor.QuotaDetail{Model: "gemini-pro-agent", ResetAt: e.reset, ReasonCode: "QUOTA_EXHAUSTED"}, true
}

func TestWrapStreamResult_PostBootstrapPropagatesQuota(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-1", Provider: "antigravity", Status: StatusActive}
	m.mu.Lock()
	m.auths[auth.ID] = auth
	m.mu.Unlock()

	reset := time.Now().Add(90 * time.Hour).UTC().Truncate(time.Second)
	remaining := make(chan cliproxyexecutor.StreamChunk, 1)
	remaining <- cliproxyexecutor.StreamChunk{Err: streamQuotaErr{reset: reset}}
	close(remaining)

	sr := m.wrapStreamResult(context.Background(), auth.Clone(), "antigravity", "gemini-pro-agent",
		nil, nil, remaining, OAuthModelAliasResult{})
	for range sr.Chunks { // drain to trigger emit -> MarkResult
	}

	m.mu.Lock()
	state := auth.ModelStates["gemini-pro-agent"]
	m.mu.Unlock()
	if state == nil {
		t.Fatal("expected model state recorded from post-bootstrap error")
	}
	if !state.NextRetryAfter.Equal(reset) {
		t.Errorf("NextRetryAfter = %v, want absolute reset %v", state.NextRetryAfter, reset)
	}
	if state.Quota.ReasonCode != "QUOTA_EXHAUSTED" {
		t.Errorf("ReasonCode = %q", state.Quota.ReasonCode)
	}
}
