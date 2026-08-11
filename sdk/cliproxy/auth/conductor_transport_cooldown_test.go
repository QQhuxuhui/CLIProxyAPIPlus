package auth

import (
	"context"
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

// TestMarkResult_TransportErrorCooldown pins the fix for the retry storm: a
// failure that carries no HTTP status (status 0) must bench the credential.
// Before the fix these fell through to the default branch, left NextRetryAfter
// at its zero value, and isAuthBlockedForModel reported the credential as
// selectable again immediately, so every retry re-uploaded the whole request
// body to an already saturated uplink.
func TestMarkResult_TransportErrorCooldown(t *testing.T) {
	const model = "gemini-3-flash"
	messages := []string{
		`Post "https://upstream/v1": read tcp 10.0.0.1:5->10.0.0.2:443: connection reset by peer`,
		"unexpected EOF",
		"socks connect tcp 10.0.0.9:7347->upstream:443: i/o timeout",
		"context deadline exceeded (Client.Timeout exceeded while awaiting headers)",
		"upstream stream closed before first payload",
	}
	for _, message := range messages {
		m := transportCooldownManager(t, "auth-transport")
		auth := markTransportFailure(t, m, "auth-transport", model, message)
		state := auth.ModelStates[model]
		if state == nil || state.NextRetryAfter.IsZero() {
			t.Fatalf("expected a cooldown for %q, got zero", message)
		}
		if got := time.Until(state.NextRetryAfter).Round(time.Second); got != time.Minute {
			t.Fatalf("expected the default 60s transient cooldown for %q, got %v", message, got)
		}
		blocked, _, _ := isAuthBlockedForModel(auth, model, time.Now())
		if !blocked {
			t.Fatalf("selector still reports the auth as available after %q", message)
		}
	}
}

// TestMarkResult_CanceledIsNotBenched guards the other side: a downstream client
// hanging up also surfaces as a status-0 error, but the credential is healthy
// and must stay selectable.
func TestMarkResult_CanceledIsNotBenched(t *testing.T) {
	const model = "gemini-3-flash"
	messages := []string{
		`Post "https://upstream/v1": context canceled`,
		`Get "https://upstream/v1": net/http: request canceled while waiting for connection`,
	}
	for _, message := range messages {
		m := transportCooldownManager(t, "auth-canceled")
		auth := markTransportFailure(t, m, "auth-canceled", model, message)
		state := auth.ModelStates[model]
		if state != nil && !state.NextRetryAfter.IsZero() {
			t.Fatalf("cancellation %q benched the credential until %v", message, state.NextRetryAfter)
		}
		if blocked, _, _ := isAuthBlockedForModel(auth, model, time.Now()); blocked {
			t.Fatalf("cancellation %q blocked the credential", message)
		}
	}
}

// TestMarkResult_TransportCooldownIsTunable documents the operational escape
// hatches: transient-error-cooldown-seconds = -1 restores the pre-fix behaviour
// without a redeploy, and a positive value lengthens the bench.
func TestMarkResult_TransportCooldownIsTunable(t *testing.T) {
	const model = "gemini-3-flash"
	t.Cleanup(func() { SetTransientErrorCooldownSeconds(0) })

	SetTransientErrorCooldownSeconds(-1)
	m := transportCooldownManager(t, "auth-tunable")
	auth := markTransportFailure(t, m, "auth-tunable", model, "connection reset by peer")
	if state := auth.ModelStates[model]; state != nil && !state.NextRetryAfter.IsZero() {
		t.Fatalf("kill switch did not disable the transport cooldown: %v", state.NextRetryAfter)
	}

	SetTransientErrorCooldownSeconds(600)
	m = transportCooldownManager(t, "auth-tunable")
	auth = markTransportFailure(t, m, "auth-tunable", model, "connection reset by peer")
	state := auth.ModelStates[model]
	if state == nil {
		t.Fatal("missing model state")
	}
	if got := time.Until(state.NextRetryAfter).Round(time.Second); got != 10*time.Minute {
		t.Fatalf("expected a 600s cooldown, got %v", got)
	}
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
	authErr, ok := err.(*Error)
	if !ok || authErr.Code != "auth_unavailable" {
		t.Fatalf("expected auth_unavailable, got %T %v", err, err)
	}
}
