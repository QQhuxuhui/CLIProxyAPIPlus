package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func markUnauthorized(m *Manager, authID string) {
	m.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    "gpt-image-2",
		Success:  false,
		Error:    &Error{Message: "web image credential was rejected (token_invalidated)", HTTPStatus: 401},
	})
}

func TestMarkResult_CodexConsecutiveUnauthorizedParksCredential(t *testing.T) {
	store := &recordingCooldownStateStore{}
	m := NewManager(nil, nil, nil)
	m.SetCooldownStateStore(store)
	auth := &Auth{ID: "codex-dead-1", Provider: "codex", Status: StatusActive}
	m.mu.Lock()
	m.auths[auth.ID] = auth
	m.mu.Unlock()

	for i := 1; i < codexAuthFailureThreshold; i++ {
		markUnauthorized(m, auth.ID)
	}
	m.mu.Lock()
	state := auth.ModelStates["gpt-image-2"]
	failures := auth.AuthFailures
	m.mu.Unlock()
	if state == nil {
		t.Fatal("expected model state")
	}
	if failures != codexAuthFailureThreshold-1 {
		t.Fatalf("AuthFailures = %d, want %d", failures, codexAuthFailureThreshold-1)
	}
	if state.NextRetryAfter.After(time.Now().Add(time.Hour)) {
		t.Fatalf("NextRetryAfter = %v escalated before reaching the threshold", state.NextRetryAfter)
	}

	markUnauthorized(m, auth.ID)
	m.mu.Lock()
	state = auth.ModelStates["gpt-image-2"]
	message := auth.StatusMessage
	stateMessage := state.StatusMessage
	failures = auth.AuthFailures
	m.mu.Unlock()
	if failures != codexAuthFailureThreshold {
		t.Fatalf("AuthFailures = %d, want %d", failures, codexAuthFailureThreshold)
	}
	if state.NextRetryAfter.Before(time.Now().Add(codexAuthFailureCooldown - time.Minute)) {
		t.Fatalf("NextRetryAfter = %v, want roughly %v ahead", state.NextRetryAfter, codexAuthFailureCooldown)
	}
	if !strings.Contains(message, "re-login required") || !strings.Contains(message, "token_invalidated") {
		t.Fatalf("unexpected auth status message: %q", message)
	}
	if stateMessage != message {
		t.Fatalf("model state message %q should mirror auth message %q", stateMessage, message)
	}
	if !auth.Unavailable || auth.NextRetryAfter.Before(time.Now().Add(codexAuthFailureCooldown-time.Minute)) {
		t.Fatalf("credential-wide cooldown = unavailable %v until %v", auth.Unavailable, auth.NextRetryAfter)
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "another-codex-model", time.Now()); !blocked {
		t.Fatal("invalidated credential remained selectable for another model")
	}

	m.MarkResult(context.Background(), Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-image-2", Success: true})
	m.mu.Lock()
	failures = auth.AuthFailures
	m.mu.Unlock()
	if failures != 0 {
		t.Fatalf("AuthFailures = %d after success, want 0", failures)
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "another-codex-model", time.Now()); blocked {
		t.Fatal("successful request did not clear credential-wide invalidation")
	}
	if strings.HasPrefix(auth.StatusMessage, "credential invalidated:") {
		t.Fatalf("successful request left invalidation marker %q", auth.StatusMessage)
	}
	store.mu.Lock()
	records := cloneCooldownStateRecords(store.records)
	store.mu.Unlock()
	for _, record := range records {
		if record.AuthID == auth.ID && record.Model == "" && record.AuthFailures > 0 {
			t.Fatalf("successful request left persisted invalidation record: %+v", record)
		}
	}
}

func TestManagerUpdatePreservesCodexAuthFailuresWhenCredentialsAreUnchanged(t *testing.T) {
	m := NewManager(nil, nil, nil)
	existing := &Auth{
		ID:           "codex-hot-reload",
		Provider:     "codex",
		Status:       StatusActive,
		AuthFailures: 2,
		Metadata: map[string]any{
			"access_token":  "access-old",
			"refresh_token": "refresh-old",
		},
	}
	if _, errRegister := m.Register(context.Background(), existing); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, errUpdate := m.Update(context.Background(), &Auth{
		ID:       existing.ID,
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token":  "access-old",
			"refresh_token": "refresh-old",
			"note":          "hot reloaded",
		},
	})
	if errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	updated, ok := m.GetByID(existing.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth missing")
	}
	if updated.AuthFailures != 2 {
		t.Fatalf("AuthFailures = %d, want 2 after non-credential update", updated.AuthFailures)
	}
}

func TestManagerUpdateClearsCodexInvalidationWhenCredentialsChange(t *testing.T) {
	store := &recordingCooldownStateStore{}
	m := NewManager(nil, nil, nil)
	m.SetCooldownStateStore(store)
	existing := &Auth{
		ID:             "codex-relogin",
		Provider:       "codex",
		Status:         StatusError,
		StatusMessage:  "credential invalidated",
		Unavailable:    true,
		NextRetryAfter: time.Now().Add(codexAuthFailureCooldown),
		AuthFailures:   codexAuthFailureThreshold,
		LastError:      &Error{Message: "unauthorized", HTTPStatus: http.StatusUnauthorized},
		Metadata: map[string]any{
			"access_token":  "access-old",
			"refresh_token": "refresh-old",
		},
		ModelStates: map[string]*ModelState{
			"gpt-image-web": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: time.Now().Add(codexAuthFailureCooldown),
				LastError:      &Error{Message: "unauthorized", HTTPStatus: http.StatusUnauthorized},
			},
		},
	}
	if _, errRegister := m.Register(context.Background(), existing); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	m.persistCooldownStates(context.Background())
	initialSaves := store.saveCount.Load()

	_, errUpdate := m.Update(context.Background(), &Auth{
		ID:       existing.ID,
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token":  "access-new",
			"refresh_token": "refresh-new",
		},
	})
	if errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	updated, ok := m.GetByID(existing.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth missing")
	}
	if updated.AuthFailures != 0 || updated.Unavailable || !updated.NextRetryAfter.IsZero() || updated.LastError != nil {
		t.Fatalf("re-login left stale auth state: failures=%d unavailable=%v next=%v last=%v", updated.AuthFailures, updated.Unavailable, updated.NextRetryAfter, updated.LastError)
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("re-login inherited %d stale model states", len(updated.ModelStates))
	}
	if got := store.saveCount.Load(); got <= initialSaves {
		t.Fatalf("re-login saved cooldown state %d times, want more than %d", got, initialSaves)
	}
	store.mu.Lock()
	records := cloneCooldownStateRecords(store.records)
	store.mu.Unlock()
	for _, record := range records {
		if record.AuthID == existing.ID {
			t.Fatalf("re-login left stale persisted cooldown: %+v", record)
		}
	}
}

func TestMarkResult_CodexInvalidationIgnoresDisableCooling(t *testing.T) {
	SetQuotaCooldownDisabled(true)
	t.Cleanup(func() { SetQuotaCooldownDisabled(false) })

	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "codex-disable-cooling", Provider: "codex", Status: StatusActive}
	m.mu.Lock()
	m.auths[auth.ID] = auth
	m.mu.Unlock()

	for i := 0; i < codexAuthFailureThreshold; i++ {
		markUnauthorized(m, auth.ID)
	}
	if !codexAuthFailureParkActive(auth, time.Now()) {
		t.Fatalf("credential was not parked with disable-cooling: %#v", auth)
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "another-codex-model", time.Now()); !blocked {
		t.Fatal("disable-cooling left invalidated credential selectable")
	}
}

func TestMarkResult_CodexUnauthorizedInvalidGrantStillParksCredential(t *testing.T) {
	SetQuotaCooldownDisabled(true)
	t.Cleanup(func() { SetQuotaCooldownDisabled(false) })

	for _, model := range []string{"gpt-image-2", ""} {
		t.Run(map[bool]string{true: "model", false: "auth"}[model != ""], func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			auth := &Auth{ID: "codex-invalid-grant-" + model, Provider: "codex", Status: StatusActive}
			m.mu.Lock()
			m.auths[auth.ID] = auth
			m.mu.Unlock()
			for i := 0; i < codexAuthFailureThreshold; i++ {
				m.MarkResult(context.Background(), Result{
					AuthID:   auth.ID,
					Provider: "codex",
					Model:    model,
					Success:  false,
					Error:    &Error{Code: "invalid_grant", Message: "invalid_grant", HTTPStatus: http.StatusUnauthorized},
				})
			}
			if !codexAuthFailureParkActive(auth, time.Now()) {
				t.Fatalf("401 invalid_grant did not park credential: %#v", auth)
			}
		})
	}
}

func TestManagerUpdateCodexTokenRefreshPreservesUnrelatedQuotaCooldown(t *testing.T) {
	m := NewManager(nil, nil, nil)
	recoverAt := time.Now().Add(2 * time.Hour)
	existing := &Auth{
		ID:       "codex-token-refresh",
		Provider: "codex",
		Status:   StatusError,
		Metadata: map[string]any{
			"access_token":  "access-old",
			"refresh_token": "refresh-same",
		},
		ModelStates: map[string]*ModelState{
			"gpt-image-web": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: recoverAt,
				Quota:          QuotaState{Exceeded: true, NextRecoverAt: recoverAt},
				LastError:      &Error{Message: "quota", HTTPStatus: http.StatusTooManyRequests},
			},
		},
	}
	if _, errRegister := m.Register(context.Background(), existing); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, errUpdate := m.Update(context.Background(), &Auth{
		ID:       existing.ID,
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token":  "access-refreshed",
			"refresh_token": "refresh-same",
		},
	})
	if errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	updated, ok := m.GetByID(existing.ID)
	if !ok || updated == nil || updated.ModelStates["gpt-image-web"] == nil {
		t.Fatalf("token refresh dropped unrelated quota state: %#v", updated)
	}
	if got := updated.ModelStates["gpt-image-web"].NextRetryAfter; !got.Equal(recoverAt) {
		t.Fatalf("quota cooldown = %v, want %v", got, recoverAt)
	}
}

func TestMarkResult_CodexUnauthorizedCountResetsOnDifferentFailure(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "codex-sequence", Provider: "codex", Status: StatusActive}
	m.mu.Lock()
	m.auths[auth.ID] = auth
	m.mu.Unlock()

	markUnauthorized(m, auth.ID)
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-image-2",
		Success:  false,
		Error:    &Error{Message: "upstream error", HTTPStatus: http.StatusBadGateway},
	})

	m.mu.RLock()
	failures := auth.AuthFailures
	m.mu.RUnlock()
	if failures != 0 {
		t.Fatalf("AuthFailures = %d after non-401 failure, want 0", failures)
	}
}

func TestRestoredCodexInvalidationBlocksModelsAndClearsOnCredentialChange(t *testing.T) {
	m := NewManager(nil, nil, nil)
	parkUntil := time.Now().Add(codexAuthFailureCooldown)
	restored := &Auth{
		ID:             "codex-restored-invalidation",
		Provider:       "codex",
		Status:         StatusError,
		StatusMessage:  "credential invalidated: 3 consecutive 401 responses, re-login required (unauthorized)",
		Unavailable:    true,
		NextRetryAfter: parkUntil,
		Metadata: map[string]any{
			"access_token":  "access-old",
			"refresh_token": "refresh-old",
		},
		ModelStates: map[string]*ModelState{
			"gpt-image-web": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: parkUntil,
				LastError:      &Error{Message: "unauthorized", HTTPStatus: http.StatusUnauthorized},
			},
		},
	}
	if _, errRegister := m.Register(context.Background(), restored); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	current, _ := m.GetByID(restored.ID)
	if blocked, _, _ := isAuthBlockedForModel(current, "another-codex-model", time.Now()); !blocked {
		t.Fatal("restored credential invalidation did not block another model")
	}

	_, errUpdate := m.Update(context.Background(), &Auth{
		ID:       restored.ID,
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token":  "access-new",
			"refresh_token": "refresh-new",
		},
	})
	if errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}
	updated, _ := m.GetByID(restored.ID)
	if updated.Unavailable || !updated.NextRetryAfter.IsZero() || len(updated.ModelStates) != 0 {
		t.Fatalf("new credentials did not clear restored invalidation: %#v", updated)
	}
}

func TestMarkResult_NonCodexUnauthorizedKeepsDefaultCooldown(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "gemini-1", Provider: "gemini", Status: StatusActive}
	m.mu.Lock()
	m.auths[auth.ID] = auth
	m.mu.Unlock()

	for i := 0; i < codexAuthFailureThreshold+1; i++ {
		m.MarkResult(context.Background(), Result{
			AuthID:   auth.ID,
			Provider: "gemini",
			Model:    "gemini-pro",
			Success:  false,
			Error:    &Error{Message: "unauthorized", HTTPStatus: 401},
		})
	}
	m.mu.Lock()
	state := auth.ModelStates["gemini-pro"]
	m.mu.Unlock()
	if state == nil {
		t.Fatal("expected model state")
	}
	if state.NextRetryAfter.After(time.Now().Add(time.Hour)) {
		t.Fatalf("NextRetryAfter = %v, non-codex providers must keep the 30m cooldown", state.NextRetryAfter)
	}
}
