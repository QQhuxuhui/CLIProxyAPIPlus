package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func mark429(t *testing.T, m *Manager, authID string, detail *cliproxyexecutor.QuotaDetail) {
	t.Helper()
	m.MarkResult(context.Background(), Result{
		AuthID:      authID,
		Provider:    "antigravity",
		Model:       "gemini-pro-agent",
		Success:     false,
		Error:       &Error{Message: "quota", HTTPStatus: 429},
		QuotaDetail: detail,
	})
}

func TestMarkResult_DetaillessSecond429KeepsQuotaFacts(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-merge-1", Provider: "antigravity", Status: StatusActive}
	m.mu.Lock()
	m.auths[auth.ID] = auth
	m.mu.Unlock()

	reset := time.Now().Add(90 * time.Hour).UTC().Truncate(time.Second)
	mark429(t, m, auth.ID, &cliproxyexecutor.QuotaDetail{
		Model:      "gemini-pro-agent",
		ResetAt:    reset,
		ReasonCode: "QUOTA_EXHAUSTED",
	})
	// Second 429 with no upstream detail at all.
	mark429(t, m, auth.ID, nil)

	m.mu.Lock()
	state := auth.ModelStates["gemini-pro-agent"]
	m.mu.Unlock()
	if state == nil {
		t.Fatal("expected model state")
	}
	if !state.Quota.ResetAt.Equal(reset) {
		t.Errorf("Quota.ResetAt = %v, want %v (must survive a detail-less 429)", state.Quota.ResetAt, reset)
	}
	if state.Quota.ReasonCode != "QUOTA_EXHAUSTED" {
		t.Errorf("Quota.ReasonCode = %q, want QUOTA_EXHAUSTED", state.Quota.ReasonCode)
	}
	if state.Quota.UpstreamModel != "gemini-pro-agent" {
		t.Errorf("Quota.UpstreamModel = %q, want gemini-pro-agent", state.Quota.UpstreamModel)
	}
	if state.NextRetryAfter.Before(reset) {
		t.Errorf("NextRetryAfter = %v shortened below known reset %v (zero-probe policy)", state.NextRetryAfter, reset)
	}
	if state.Quota.NextRecoverAt.Before(reset) {
		t.Errorf("Quota.NextRecoverAt = %v shortened below known reset %v", state.Quota.NextRecoverAt, reset)
	}
}

func TestMarkResult_ExplicitNewResetOverridesOldOne(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-merge-2", Provider: "antigravity", Status: StatusActive}
	m.mu.Lock()
	m.auths[auth.ID] = auth
	m.mu.Unlock()

	oldReset := time.Now().Add(90 * time.Hour).UTC().Truncate(time.Second)
	newReset := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	mark429(t, m, auth.ID, &cliproxyexecutor.QuotaDetail{
		Model:      "gemini-pro-agent",
		ResetAt:    oldReset,
		ReasonCode: "QUOTA_EXHAUSTED",
	})
	// Upstream's latest explicit word wins, even if earlier than before.
	mark429(t, m, auth.ID, &cliproxyexecutor.QuotaDetail{
		Model:      "gemini-pro-agent",
		ResetAt:    newReset,
		ReasonCode: "QUOTA_EXHAUSTED",
	})

	m.mu.Lock()
	state := auth.ModelStates["gemini-pro-agent"]
	m.mu.Unlock()
	if state == nil {
		t.Fatal("expected model state")
	}
	if !state.Quota.ResetAt.Equal(newReset) {
		t.Errorf("Quota.ResetAt = %v, want new explicit %v", state.Quota.ResetAt, newReset)
	}
	if !state.NextRetryAfter.Equal(newReset) {
		t.Errorf("NextRetryAfter = %v, want new explicit %v", state.NextRetryAfter, newReset)
	}
}
