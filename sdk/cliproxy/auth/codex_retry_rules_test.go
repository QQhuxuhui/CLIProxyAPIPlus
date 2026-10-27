package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type codexRulesExecutor struct {
	retryRoundCallExecutor
	failure      error
	succeedAfter int
	onExecute    func()
}

func (e *codexRulesExecutor) Execute(_ context.Context, a *Auth, _ exec.Request, _ exec.Options) (exec.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.executeIDs = append(e.executeIDs, a.ID)
	if e.onExecute != nil {
		e.onExecute()
	}
	if e.succeedAfter > 0 && len(e.executeIDs) > e.succeedAfter {
		return exec.Response{Payload: []byte(`{"ok":true}`)}, nil
	}
	return exec.Response{}, e.failure
}
func (e *codexRulesExecutor) ExecuteStream(_ context.Context, a *Auth, _ exec.Request, _ exec.Options) (*exec.StreamResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.streamIDs = append(e.streamIDs, a.ID)
	return nil, e.failure
}
func newCodexRulesManager(t *testing.T, n int, failure error) (*Manager, *codexRulesExecutor, string, []string) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 3, 0)
	e := &codexRulesExecutor{retryRoundCallExecutor: retryRoundCallExecutor{identifier: "codex"}, failure: failure}
	m.RegisterExecutor(e)
	model := "codex-rules-" + uuid.NewString()
	ids := make([]string, n)
	for i := range ids {
		id := fmt.Sprintf("%s-%02d", model, i)
		ids[i] = id
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex"}); err != nil {
			t.Fatal(err)
		}
	}
	return m, e, model, ids
}
func TestCodexRetryRulesSixDistinctAccounts(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			m, e, model, _ := newCodexRulesManager(t, 10, &Error{HTTPStatus: 503, Message: `{"error":{"code":"server_is_overloaded"}}`})
			var err error
			kind := "execute"
			if stream {
				_, err = m.ExecuteStream(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{})
				kind = "stream"
			} else {
				_, err = m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{})
			}
			if statusCodeFromError(err) != 529 {
				t.Errorf("status=%d, want 529; error=%v", statusCodeFromError(err), err)
			}
			ids := e.ids(kind)
			if len(ids) != 6 || len(countRetryRoundIDs(ids)) != 6 {
				t.Fatalf("attempts=%v, want six distinct credentials", ids)
			}
		})
	}
}
func TestCodexRetryRulesOverloadDoesNotPenalizeAccount(t *testing.T) {
	m, _, model, ids := newCodexRulesManager(t, 1, &Error{HTTPStatus: 503, Message: "overloaded"})
	_, _ = m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{})
	a, ok := m.GetByID(ids[0])
	if !ok {
		t.Fatal("missing auth")
	}
	if a.Failed != 0 || a.Unavailable || len(a.ModelStates) != 0 {
		t.Fatalf("overload penalized account: failures=%d unavailable=%v states=%v", a.Failed, a.Unavailable, a.ModelStates)
	}
}
func TestCodexRetryRulesNoAccountReuse(t *testing.T) {
	m, e, model, _ := newCodexRulesManager(t, 2, dialRefusedError())
	_, _ = m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{})
	ids := e.ids("execute")
	if len(ids) != 2 || len(countRetryRoundIDs(ids)) != 2 {
		t.Fatalf("attempts=%v, want exactly two", ids)
	}
}
func TestCodexRetryRulesQuotaWinsOverWrapped503(t *testing.T) {
	m, _, model, ids := newCodexRulesManager(t, 1, &Error{HTTPStatus: 503, Message: `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`})
	_, err := m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{})
	if statusCodeFromError(err) != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429", statusCodeFromError(err))
	}
	a, _ := m.GetByID(ids[0])
	if a.Failed == 0 {
		t.Fatal("rate limit must count as failure")
	}
}

func TestCodexRetryRulesSharedBudgetAcrossManagerCalls(t *testing.T) {
	m, e, model, _ := newCodexRulesManager(t, 10, &Error{HTTPStatus: 503, Message: "overloaded"})
	ctx := WithCodexRetryBudget(context.Background())
	for range 2 {
		_, _ = m.Execute(ctx, []string{"codex"}, exec.Request{Model: model}, exec.Options{})
	}
	if got := len(e.ids("execute")); got != 6 {
		t.Fatalf("calls=%d, budget was reset", got)
	}
}
func TestCodexRetryRulesSoftDeadlineKeepsInflightSuccess(t *testing.T) {
	m, e, model, _ := newCodexRulesManager(t, 2, &Error{HTTPStatus: 503, Message: "overloaded"})
	ctx := WithCodexRetryBudget(context.Background())
	b := codexBudget(ctx)
	now := time.Now()
	b.deadline = now.Add(20 * time.Second)
	b.now = func() time.Time { return now }
	e.succeedAfter = 0
	// The executor advances a controlled clock while the request is in flight.
	e.onExecute = func() { now = now.Add(21 * time.Second) }
	e.failure = nil
	_, err := m.Execute(ctx, []string{"codex"}, exec.Request{Model: model}, exec.Options{})
	if err != nil {
		t.Fatalf("in-flight success was interrupted: %v", err)
	}
	_, _ = m.Execute(ctx, []string{"codex"}, exec.Request{Model: model}, exec.Options{})
	if got := len(e.ids("execute")); got != 1 {
		t.Fatalf("calls=%d after deadline", got)
	}
	if ctx.Err() != nil {
		t.Fatalf("soft budget cancelled context: %v", ctx.Err())
	}
}
func TestCodexRetryRulesQuotaEnvelopeSurvivesLaterOverload(t *testing.T) {
	m, e, model, _ := newCodexRulesManager(t, 2, &Error{HTTPStatus: 429, Message: `{"error":{"type":"usage_limit_reached","message":"quota exhausted"}}`})
	first := e.failure
	e.onExecute = func() {
		if len(e.executeIDs) == 1 {
			e.failure = first
		} else {
			e.failure = &Error{HTTPStatus: 503, Message: "overloaded"}
		}
	}
	_, err := m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{})
	if statusCodeFromError(err) != 429 || !strings.Contains(err.Error(), "quota exhausted") {
		t.Fatalf("quota outcome lost: %v", err)
	}
}

func TestCodexRetryRulesMixedRouteUsesSameBudget(t *testing.T) {
	m, e, model, _ := newCodexRulesManager(t, 10, &Error{HTTPStatus: 503, Message: "overloaded"})
	m.SetRetryConfig(0, 3, 0)
	_, err := m.Execute(context.Background(), []string{"codex", "openai"}, exec.Request{Model: model}, exec.Options{})
	if got := len(e.ids("execute")); got != 6 {
		t.Fatalf("mixed route calls=%d want 6", got)
	}
	if statusCodeFromError(err) != 529 {
		t.Fatalf("mixed route status=%d want 529: %v", statusCodeFromError(err), err)
	}
}

func TestCodexRetryRulesOverloadMetricsAreIndependent(t *testing.T) {
	m, _, model, ids := newCodexRulesManager(t, 1, &Error{HTTPStatus: 503, Message: "overloaded"})
	_, _ = m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: model}, exec.Options{})
	points := m.CodexPoolSnapshot().UpstreamOverloads
	if len(points) != 1 || points[0].AuthID != ids[0] || points[0].Count != 1 {
		t.Fatalf("overload metrics=%v", points)
	}
	a, _ := m.GetByID(ids[0])
	if a.Failed != 0 {
		t.Fatal("overload metric changed account failure count")
	}
}

func TestCodexRetryRulesExpiredBudgetPreservesCancellation(t *testing.T) {
	m, _, model, _ := newCodexRulesManager(t, 1, &Error{HTTPStatus: 503, Message: "overloaded"})
	ctx, cancel := context.WithCancel(WithCodexRetryBudget(context.Background()))
	b := codexBudget(ctx)
	b.deadline = b.now().Add(-time.Second)
	cancel()
	_, err := m.Execute(ctx, []string{"codex"}, exec.Request{Model: model}, exec.Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v want context cancellation", err)
	}
}
func TestCodexRetryRulesMixedStreamBootstrapFinalStatus(t *testing.T) {
	m, _, model, _ := newCodexRulesManager(t, 10, &Error{HTTPStatus: 503, Message: "overloaded"})
	m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", streamFn: func(context.Context, *Auth, exec.Request, exec.Options) (*exec.StreamResult, error) {
		ch := make(chan exec.StreamChunk, 1)
		ch <- exec.StreamChunk{Err: &Error{HTTPStatus: 503, Message: "overloaded"}}
		close(ch)
		return &exec.StreamResult{Chunks: ch}, nil
	}})
	result, err := m.ExecuteStream(context.Background(), []string{"codex", "openai"}, exec.Request{Model: model}, exec.Options{})
	if statusCodeFromError(err) != 529 {
		if result != nil {
			for range result.Chunks {
			}
		}
		t.Fatalf("error=%v want terminal 529", err)
	}
}

func TestCodexRetryRulesDeadlineStopsNewAccounts(t *testing.T) {
	m, e, model, _ := newCodexRulesManager(t, 10, &Error{HTTPStatus: 503, Message: "overloaded"})
	ctx := WithCodexRetryBudget(context.Background())
	b := codexBudget(ctx)
	now := time.Now()
	b.now = func() time.Time { return now }
	b.deadline = now.Add(20 * time.Second)
	e.onExecute = func() { now = now.Add(21 * time.Second) }
	_, err := m.Execute(ctx, []string{"codex"}, exec.Request{Model: model}, exec.Options{})
	if len(e.ids("execute")) != 1 || statusCodeFromError(err) != 529 {
		t.Fatalf("calls=%v err=%v", e.ids("execute"), err)
	}
}
func TestCodexRetryRulesUnauthorizedRefreshUsesSameAccountOnce(t *testing.T) {
	m, e, model, ids := newCodexRulesManager(t, 1, &Error{HTTPStatus: 401, Message: "expired access token"})
	a, _ := m.GetByID(ids[0])
	a.Metadata = map[string]any{"refresh_token": "fake-refresh", "access_token": "fake-access"}
	if _, err := m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	e.onExecute = func() {
		if len(e.executeIDs) == 2 {
			e.failure = nil
		}
	}
	ctx := WithCodexRetryBudget(context.Background())
	_, err := m.Execute(ctx, []string{"codex"}, exec.Request{Model: model}, exec.Options{})
	if err != nil || len(e.ids("execute")) != 2 || codexBudget(ctx).count() != 1 {
		t.Fatalf("calls=%v accounts=%d err=%v", e.ids("execute"), codexBudget(ctx).count(), err)
	}
}

func TestCodexRetryRulesCooldownWaitSkipsZeroWeight(t *testing.T) {
	m, _, model, ids := newCodexRulesManager(t, 1, &Error{HTTPStatus: 503, Message: "overloaded"})
	m.SetSelector(&WeightedRoundRobinSelector{})
	a, _ := m.GetByID(ids[0])
	a.Attributes = map[string]string{AttributeWeight: "0"}
	if _, err := m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	_, retry := m.codexRetryWait(WithCodexRetryBudget(context.Background()), model, exec.Options{}, 0)
	if retry {
		t.Fatal("zero-weight account must not trigger an immediate retry loop")
	}
}

func TestCodexRetryRulesInheritsIngressBudget(t *testing.T) {
	ingress := WithCodexRetryBudget(context.Background())
	handler := WithCodexRetryBudget(context.Background(), ingress)
	if codexBudget(handler) != codexBudget(ingress) {
		t.Fatal("handler reset the ingress budget")
	}
}
