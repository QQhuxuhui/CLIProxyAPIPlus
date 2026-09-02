package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// busyCapacityTestError implements cliproxyexecutor.CapacityError: a healthy
// credential that is at its per-account concurrency limit.
type busyCapacityTestError struct{}

func (busyCapacityTestError) Error() string     { return "account busy" }
func (busyCapacityTestError) StatusCode() int   { return http.StatusTooManyRequests }
func (busyCapacityTestError) AccountBusy() bool { return true }

func newBusyRotationTestManager(t *testing.T, errByAuthSuffix map[string]error) (*Manager, *authFallbackExecutor, map[string]*Auth) {
	t.Helper()

	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, 8)

	executor := &authFallbackExecutor{id: "codex", executeErrors: map[string]error{}}
	m.RegisterExecutor(executor)

	base := uuid.NewString()
	reg := registry.GetGlobalRegistry()
	auths := make(map[string]*Auth, len(errByAuthSuffix))
	for suffix, execErr := range errByAuthSuffix {
		id := base + "-" + suffix
		auth := &Auth{ID: id, Provider: "codex", Status: StatusActive}
		reg.RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "gpt-image-web"}})
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", suffix, errRegister)
		}
		if execErr != nil {
			executor.executeErrors[id] = execErr
		}
		auths[suffix] = auth
	}
	return m, executor, auths
}

// A busy account is skipped in favour of an idle one within the same request,
// and the busy account's health is left untouched (no cooldown).
func TestExecute_BusyAccountRotatesToIdleWithoutPenalty(t *testing.T) {
	// "a-busy" sorts before "b-ok", so round-robin probes the busy one first.
	m, executor, auths := newBusyRotationTestManager(t, map[string]error{
		"a-busy": busyCapacityTestError{},
		"b-ok":   nil,
	})

	resp, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-image-web"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute returned error, want success via the idle account: %v", err)
	}
	if got := string(resp.Payload); got != auths["b-ok"].ID {
		t.Fatalf("served by %q, want the idle account %q", got, auths["b-ok"].ID)
	}

	calls := executor.ExecuteCalls()
	if len(calls) != 2 || calls[0] != auths["a-busy"].ID || calls[1] != auths["b-ok"].ID {
		t.Fatalf("execute calls = %v, want [busy, ok] rotation", calls)
	}

	// The busy account must remain healthy and selectable — it was skipped, not benched.
	busy, ok := m.GetByID(auths["a-busy"].ID)
	if !ok {
		t.Fatal("busy auth missing from manager")
	}
	if blocked, _, _ := isAuthBlockedForModel(busy, "gpt-image-web", time.Now()); blocked {
		t.Fatal("busy account was benched; a per-account concurrency skip must not cool it")
	}
	if busy.Status == StatusError {
		t.Fatalf("busy account status = %v, want it left healthy", busy.Status)
	}
}

// When every account is busy, the request fails fast with the retryable
// capacity (429) signal rather than blocking or reporting a generic error.
func TestExecute_AllAccountsBusyReturnsCapacityError(t *testing.T) {
	m, _, _ := newBusyRotationTestManager(t, map[string]error{
		"a-busy": busyCapacityTestError{},
		"b-busy": busyCapacityTestError{},
	})

	_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-image-web"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("Execute succeeded, want a capacity error when all accounts are busy")
	}
	se, ok := err.(cliproxyexecutor.StatusError)
	if !ok {
		t.Fatalf("error type = %T, want a StatusError carrying the capacity code", err)
	}
	if se.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("capacity error status = %d, want 429", se.StatusCode())
	}
}
