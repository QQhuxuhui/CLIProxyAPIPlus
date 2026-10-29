package auth

import (
	"context"
	"errors"
	"testing"
)

type egressStatusErr struct {
	status int
	msg    string
}

func (e egressStatusErr) Error() string   { return e.msg }
func (e egressStatusErr) StatusCode() int { return e.status }

func TestCodexEgressRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"overload 502", egressStatusErr{502, `{"error":{"code":"server_is_overloaded","type":"service_unavailable_error"}}`}, true},
		{"server error 500", egressStatusErr{500, `{"error":{"type":"server_error","code":"server_error","message":"An error occurred"}}`}, true},
		{"transport eof", errors.New("unexpected EOF"), true},
		{"account 429", egressStatusErr{429, `{"detail":"Rate limit exceeded"}`}, false},
		{"quota", egressStatusErr{429, `{"error":{"type":"usage_limit_reached"}}`}, false},
		{"unauthorized", egressStatusErr{401, `{"error":{"code":"token_expired"}}`}, false},
		{"bad request", egressStatusErr{400, `{"error":{"type":"invalid_request_error"}}`}, false},
	}
	for _, tc := range cases {
		if got := codexEgressRetryable(tc.err); got != tc.want {
			t.Errorf("%s: got %v want %v (class %q)", tc.name, got, tc.want, codexFailureClass(tc.err))
		}
	}
}

func TestCodexEgressRetryPendingFollowsBudget(t *testing.T) {
	ctx := WithCodexRetryBudget(context.Background())
	if CodexEgressRetryPending(ctx) || CodexEgressRetryPending(context.Background()) {
		t.Fatal("fresh request must not be a retry")
	}
	m := NewManager(nil, nil, nil)
	m.executionResultError(ctx, "codex", egressStatusErr{429, `{"detail":"Rate limit exceeded"}`})
	if CodexEgressRetryPending(ctx) {
		t.Fatal("account 429 must not route retries to the egress pool")
	}
	m.executionResultError(ctx, "codex", egressStatusErr{502, `{"error":{"code":"server_is_overloaded"}}`})
	if !CodexEgressRetryPending(ctx) {
		t.Fatal("overload must mark the request for the egress pool")
	}
}

func TestCodexEgressRetryPendingWithRetryBudgetDisabled(t *testing.T) {
	m := NewManager(nil, nil, nil)
	if m.codexRetryEnabled() {
		t.Fatal("test expects codex.retry-budget disabled by default")
	}
	// Handlers install the budget; the manager then masks it because the retry budget is off.
	ctx := m.codexRequestContext(WithCodexRetryBudget(context.Background()), []string{"codex"})
	if codexBudget(ctx) != nil {
		t.Fatal("disabled retry budget must stay masked")
	}
	m.executionResultError(ctx, "codex", egressStatusErr{502, `{"error":{"code":"server_is_overloaded"}}`})
	if !CodexEgressRetryPending(ctx) {
		t.Fatal("overload must mark the request for the egress pool even with the retry budget disabled")
	}
}
