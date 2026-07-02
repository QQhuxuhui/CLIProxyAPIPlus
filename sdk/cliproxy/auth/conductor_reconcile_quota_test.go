package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func reconcileTestManager(t *testing.T, authID, model string, state *ModelState) (*Manager, *Auth) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "antigravity", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:          authID,
		Provider:    "antigravity",
		Status:      StatusError,
		ModelStates: map[string]*ModelState{model: state},
	}
	m.mu.Lock()
	m.auths[auth.ID] = auth
	m.mu.Unlock()
	return m, auth
}

func TestReconcileRegistryModelStates_PreservesActiveQuota(t *testing.T) {
	future := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	m, auth := reconcileTestManager(t, "auth-reconcile-active", "gemini-3-pro-image", &ModelState{
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: future,
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: future,
			ResetAt:       future,
			ReasonCode:    "QUOTA_EXHAUSTED",
			UpstreamModel: "gemini-3-pro-image",
		},
	})

	m.ReconcileRegistryModelStates(context.Background(), auth.ID)

	m.mu.Lock()
	state := auth.ModelStates["gemini-3-pro-image"]
	m.mu.Unlock()
	if state == nil {
		t.Fatal("model state was pruned; want preserved")
	}
	if !state.Quota.Exceeded {
		t.Error("Quota.Exceeded cleared; want preserved")
	}
	if !state.Quota.ResetAt.Equal(future) {
		t.Errorf("Quota.ResetAt = %v, want %v", state.Quota.ResetAt, future)
	}
	if !state.Quota.NextRecoverAt.Equal(future) {
		t.Errorf("Quota.NextRecoverAt = %v, want %v", state.Quota.NextRecoverAt, future)
	}
	if state.Quota.ReasonCode != "QUOTA_EXHAUSTED" {
		t.Errorf("Quota.ReasonCode = %q, want QUOTA_EXHAUSTED", state.Quota.ReasonCode)
	}
	if state.Quota.UpstreamModel != "gemini-3-pro-image" {
		t.Errorf("Quota.UpstreamModel = %q", state.Quota.UpstreamModel)
	}
	if !state.NextRetryAfter.Equal(future) {
		t.Errorf("NextRetryAfter = %v, want %v (routing must stay blocked)", state.NextRetryAfter, future)
	}
	if !state.Unavailable {
		t.Error("Unavailable cleared; want preserved")
	}
}

func TestReconcileRegistryModelStates_ResetsExpiredState(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	m, auth := reconcileTestManager(t, "auth-reconcile-expired", "gemini-3-pro-image", &ModelState{
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: past,
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: past,
			ResetAt:       past,
		},
	})

	m.ReconcileRegistryModelStates(context.Background(), auth.ID)

	m.mu.Lock()
	state := auth.ModelStates["gemini-3-pro-image"]
	m.mu.Unlock()
	if state == nil {
		t.Fatal("model state was pruned; want reset in place")
	}
	if state.Quota.Exceeded || !state.Quota.NextRecoverAt.IsZero() {
		t.Errorf("expired quota not reset: %+v", state.Quota)
	}
	if state.Unavailable || !state.NextRetryAfter.IsZero() {
		t.Errorf("expired cooldown not reset: unavailable=%v nextRetryAfter=%v", state.Unavailable, state.NextRetryAfter)
	}
}

func TestReconcileRegistryModelStates_StillPrunesUnsupportedModels(t *testing.T) {
	future := time.Now().Add(48 * time.Hour)
	m, auth := reconcileTestManager(t, "auth-reconcile-prune", "gemini-3-pro-image", &ModelState{
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: future,
		Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: future},
	})
	// Add a state for a model that is NOT registered; it must be pruned even
	// though its cooldown is still active.
	m.mu.Lock()
	auth.ModelStates["removed-model"] = &ModelState{
		Status:         StatusError,
		Unavailable:    true,
		NextRetryAfter: future,
		Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: future},
	}
	m.mu.Unlock()

	m.ReconcileRegistryModelStates(context.Background(), auth.ID)

	m.mu.Lock()
	_, removedExists := auth.ModelStates["removed-model"]
	kept := auth.ModelStates["gemini-3-pro-image"]
	m.mu.Unlock()
	if removedExists {
		t.Error("state for unregistered model not pruned")
	}
	if kept == nil || !kept.Quota.Exceeded {
		t.Error("registered model's active quota state must survive the prune pass")
	}
}
