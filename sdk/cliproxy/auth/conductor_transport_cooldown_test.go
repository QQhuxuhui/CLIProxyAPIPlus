package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func transportCooldownManager(t *testing.T, authID string) *Manager {
	t.Helper()
	m := NewManager(nil, nil, nil)
	if _, err := m.Register(context.Background(), &Auth{
		ID:       authID,
		Provider: "antigravity",
		Status:   StatusActive,
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	return m
}

func markTransportFailure(t *testing.T, m *Manager, authID, model, message string) *Auth {
	t.Helper()
	m.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: "antigravity",
		Model:    model,
		Success:  false,
		Error:    &Error{Message: message},
	})
	auth, ok := m.GetByID(authID)
	if !ok || auth == nil {
		t.Fatalf("auth %s missing after MarkResult", authID)
	}
	return auth
}

// TestTransportCooldownDoesNotLeak429 pins the downstream contract. A pool that
// is fully benched by transport cooldowns must not look like a quota event:
// modelCooldownError renders as HTTP 429, which downstream gateways treat as a
// reason to disable an account. Transport cooldowns leave Quota.Exceeded unset,
// so getAvailableAuths returns auth_unavailable (rendered as 503) instead.
func TestTransportCooldownDoesNotLeak429(t *testing.T) {
	const model = "gemini-3-flash"
	now := time.Now()
	auth := &Auth{ID: "auth-drained", Provider: "antigravity", Status: StatusActive}
	state := ensureModelState(auth, model)
	state.Unavailable = true
	state.Status = StatusError
	state.NextRetryAfter = now.Add(time.Minute)

	_, err := getAvailableAuths([]*Auth{auth}, "antigravity", model, now)
	if err == nil {
		t.Fatal("expected the drained pool to fail selection")
	}
	if coded, ok := err.(interface{ StatusCode() int }); ok {
		if coded.StatusCode() == http.StatusTooManyRequests {
			t.Fatalf("transport cooldown leaked a 429 downstream: %v", err)
		}
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil || authErr.Code != "auth_unavailable" {
		t.Fatalf("expected auth_unavailable, got %T %v", err, err)
	}
}
