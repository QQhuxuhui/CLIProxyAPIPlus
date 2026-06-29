package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestMarkResult_AntigravityQuotaUsesAbsoluteReset(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-1", Provider: "antigravity", Status: StatusActive}
	m.mu.Lock()
	m.auths[auth.ID] = auth
	m.mu.Unlock()

	reset := time.Now().Add(90 * time.Hour).UTC().Truncate(time.Second)
	delay := 5 * time.Minute // intentionally inconsistent with absolute reset to verify absolute takes priority
	m.MarkResult(context.Background(), Result{
		AuthID:     auth.ID,
		Provider:   "antigravity",
		Model:      "gemini-pro-agent",
		Success:    false,
		Error:      &Error{Message: "quota", HTTPStatus: 429},
		RetryAfter: &delay,
		QuotaDetail: &cliproxyexecutor.QuotaDetail{
			Model:      "gemini-pro-agent",
			ResetAt:    reset,
			ReasonCode: "QUOTA_EXHAUSTED",
		},
	})

	m.mu.Lock()
	state := auth.ModelStates["gemini-pro-agent"]
	m.mu.Unlock()
	if state == nil {
		t.Fatal("expected model state")
	}
	if !state.Quota.ResetAt.Equal(reset) {
		t.Errorf("Quota.ResetAt = %v, want %v", state.Quota.ResetAt, reset)
	}
	if !state.NextRetryAfter.Equal(reset) {
		t.Errorf("NextRetryAfter = %v, want absolute reset %v", state.NextRetryAfter, reset)
	}
	if state.Quota.ReasonCode != "QUOTA_EXHAUSTED" {
		t.Errorf("ReasonCode = %q", state.Quota.ReasonCode)
	}
	if state.Quota.UpstreamModel != "gemini-pro-agent" {
		t.Errorf("UpstreamModel = %q", state.Quota.UpstreamModel)
	}
}
