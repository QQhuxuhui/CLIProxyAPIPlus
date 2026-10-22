package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestRateCapWindowLearningAndProbe(t *testing.T) {
	now := time.Unix(1800000000, 0)
	r := newRequestRateCap()
	r.nowFunc = func() time.Time { return now }
	cfg := internalconfig.RequestRateCapConfig{Enabled: true, InitialPerMinute: 5, MinPerMinute: 2, MaxPerMinute: 6, ProbeInterval: "5m", Headroom: 1}
	r.configure(cfg)
	first := r.start("a")
	for i := 0; i < 4; i++ {
		r.start("a")
	}
	if count, limit, _ := r.account("a"); count != 5 || limit != 5 {
		t.Fatalf("count/limit = %d/%d", count, limit)
	}
	r.reject("a", first)
	if count, limit, _ := r.account("a"); count != 4 || limit != 3 {
		t.Fatalf("learned count/limit = %d/%d", count, limit)
	}
	r.reject("a", first)
	if count, limit, _ := r.account("a"); count != 4 || limit != 3 {
		t.Fatalf("duplicate result changed state: %d/%d", count, limit)
	}
	now = now.Add(time.Minute)
	if count, _, _ := r.account("a"); count != 0 {
		t.Fatalf("expired count = %d", count)
	}
	now = now.Add(4 * time.Minute)
	if _, limit, _ := r.account("a"); limit != 4 {
		t.Fatalf("probe limit = %d", limit)
	}
	cfg.Headroom = .5
	r.configure(cfg)
	if _, limit, _ := r.account("a"); limit != 2 {
		t.Fatalf("hot updated limit = %d", limit)
	}
}

func TestRateCapOutOfOrderRejection(t *testing.T) {
	now := time.Unix(1800000000, 0)
	r := newRequestRateCap()
	r.nowFunc = func() time.Time { return now }
	r.configure(internalconfig.RequestRateCapConfig{})
	old := r.start("a")
	now = now.Add(59 * time.Second)
	r.start("a")
	r.reject("a", old)
	now = now.Add(time.Second)
	if count, _, _ := r.account("a"); count != 1 {
		t.Fatalf("removed wrong attempt: %d", count)
	}
}

func TestRateCapSelectionAndFailFast(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy=", legacy), func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex", countFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				return cliproxyexecutor.Response{}, nil
			}})
			ids := registerOverloadAuths(t, m, 2)
			if legacy {
				m.SetSelector(NewSessionAffinitySelector(&FillFirstSelector{}))
			}
			cfg := &internalconfig.Config{ProviderRequestRateCap: map[string]internalconfig.RequestRateCapConfig{"codex": {Enabled: true, InitialPerMinute: 1, MinPerMinute: 1, MaxPerMinute: 5}}}
			m.SetConfig(cfg)
			m.rateCap.start(ids[0])
			selected, _, _, err := m.pickNextMixed(context.Background(), []string{"codex"}, "gpt-5.6-terra", cliproxyexecutor.Options{}, nil)
			if err != nil || selected.ID != ids[1] {
				t.Fatalf("free lower priority account not selected: %v %v", selected, err)
			}
			m.rateCap.start(ids[1])
			if _, _, _, err = m.pickNextMixed(context.Background(), []string{"codex"}, "gpt-5.6-terra", cliproxyexecutor.Options{}, nil); err != nil {
				t.Fatalf("soft cap did not fall back: %v", err)
			}
			c := cfg.ProviderRequestRateCap["codex"]
			c.FailFast = true
			cfg.ProviderRequestRateCap["codex"] = c
			m.SetConfig(cfg)
			_, _, _, err = m.pickNextMixed(context.Background(), []string{"codex"}, "gpt-5.6-terra", cliproxyexecutor.Options{}, nil)
			if !isPoolCapacityError(err) {
				t.Fatalf("want 529: %v", err)
			}
			var authErr *Error
			if !errors.As(err, &authErr) || authErr.HTTPStatus != 529 {
				t.Fatalf("auth error: %v", err)
			}
			if !strings.Contains(err.Error(), `"type":"upstream_pool_busy"`) {
				t.Fatal(err)
			}
			wait, retry := m.shouldRetryAfterErrorWithAttempted(context.Background(), cliproxyexecutor.Options{}, err, 0, []string{"codex"}, "gpt-5.6-terra", time.Minute, -1, 10, nil)
			if retry || wait != 0 {
				t.Fatal("capacity error retried")
			}
			if _, errCount := m.ExecuteCount(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra"}, cliproxyexecutor.Options{}); isPoolCapacityError(errCount) {
				t.Fatal("local token counting was capped")
			}
		})
	}
}

func TestRateCapCoolingAndLaterRecovery(t *testing.T) {
	r := newRequestRateCap()
	now := time.Unix(1800000000, 0)
	r.nowFunc = func() time.Time { return now }
	r.configure(internalconfig.RequestRateCapConfig{Enabled: true, FailFast: true, InitialPerMinute: 1, MinPerMinute: 1})
	a := &Auth{ID: "a", Provider: "codex"}
	r.start(a.ID)
	_, err := r.filter([]rateCandidate{{a, true, true, now.Add(7 * time.Second)}})
	var busy *poolCapacityError
	if !errors.As(err, &busy) || busy.Headers().Get("Retry-After") != "60" {
		t.Fatalf("expected later window reset: %v", err)
	}
	now = now.Add(time.Minute)
	_, err = r.filter([]rateCandidate{{a, true, true, now.Add(7 * time.Second)}})
	if !errors.As(err, &busy) || busy.Headers().Get("Retry-After") != "7" {
		t.Fatalf("expected cooldown reset: %v", err)
	}
}

func TestCodexRateLimitExplicitResetDoesNotBackoff(t *testing.T) {
	m := NewManager(nil, nil, nil)
	a := &Auth{ID: "rate-reset", Provider: "codex", Status: StatusActive, ModelStates: map[string]*ModelState{"second": {Status: StatusActive}}}
	m.auths[a.ID] = a
	for _, delay := range []time.Duration{time.Minute, 7 * time.Second, time.Minute, time.Minute} {
		reset := time.Now().Add(delay)
		m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: "codex", Model: "first", CredentialScope: true, Error: &Error{HTTPStatus: 429}, QuotaDetail: &cliproxyexecutor.QuotaDetail{ReasonCode: "RATE_LIMIT_EXCEEDED", ResetAt: reset}})
		if !a.Quota.NextRecoverAt.Equal(reset) {
			t.Fatalf("credential deadline = %v, want %v", a.Quota.NextRecoverAt, reset)
		}
		for _, model := range []string{"first", "second"} {
			state := a.ModelStates[model]
			if !state.NextRetryAfter.Equal(reset) || state.Quota.BackoffLevel != 0 {
				t.Fatalf("model %s reset/backoff: %+v", model, state)
			}
		}
	}
}

func TestRateCapConcurrentAdmissionReselects(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint("stream=", stream), func(t *testing.T) {
			m := NewManager(nil, &FillFirstSelector{}, nil)
			ids := registerOverloadAuths(t, m, 2)
			m.SetRetryConfig(0, 0, 1)
			m.SetConfig(&internalconfig.Config{ProviderRequestRateCap: map[string]internalconfig.RequestRateCapConfig{"codex": {Enabled: true, FailFast: true, InitialPerMinute: 1, MinPerMinute: 1}}})
			exec := &customStreamMockExecutor{identifier: "codex", mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				cliproxyexecutor.MarkUpstreamAttempt(ctx)
				return cliproxyexecutor.Response{}, nil
			}}, streamFn: func(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				cliproxyexecutor.MarkUpstreamAttempt(ctx)
				return successStreamResult(), nil
			}}
			m.RegisterExecutor(exec)
			var intercepted atomic.Int32
			ready := make(chan struct{})
			opts := cliproxyexecutor.Options{RequestAfterAuthInterceptor: func(ctx context.Context, in cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
				n := intercepted.Add(1)
				if n == 2 {
					close(ready)
				}
				if n <= 2 {
					<-ready
				}
				return cliproxyexecutor.RequestAfterAuthInterceptResponse{}
			}}
			results := make(chan error, 2)
			for i := 0; i < 2; i++ {
				go func() {
					if stream {
						response, err := m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra"}, opts)
						if response != nil {
							for range response.Chunks {
							}
						}
						results <- err
					} else {
						_, err := m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra"}, opts)
						results <- err
					}
				}()
			}
			for i := 0; i < 2; i++ {
				if err := <-results; err != nil {
					t.Fatalf("free alternate account was skipped: %v", err)
				}
			}
			for _, id := range ids {
				if count, _, _ := m.rateCap.account(id); count != 1 {
					t.Fatalf("account %s count=%d", id, count)
				}
			}
			if m.CodexPoolSnapshot().FastFailures != 0 {
				t.Fatal("internal admission races counted as public failures")
			}
		})
	}
}

func TestRateCapRefundAndInternalRetry(t *testing.T) {
	m := NewManager(nil, nil, nil)
	a := &Auth{ID: "a", Provider: "codex"}
	ctx, err := m.beginRateAttempt(newUpstreamAttemptContext(context.Background()), a)
	if err != nil {
		t.Fatal(err)
	}
	m.finishRateDispatch(ctx)
	if count, _, _ := m.rateCap.account(a.ID); count != 0 {
		t.Fatalf("local failure retained: %d", count)
	}
	ctx, err = m.beginRateAttempt(newUpstreamAttemptContext(context.Background()), a)
	if err != nil {
		t.Fatal(err)
	}
	cliproxyexecutor.MarkUpstreamAttempt(ctx)
	cliproxyexecutor.ObserveUpstreamRequestRetry(ctx)
	m.finishRateDispatch(ctx)
	m.observeRateResult(ctx, Result{AuthID: a.ID, Provider: "codex", Error: &Error{HTTPStatus: 429}, QuotaDetail: &cliproxyexecutor.QuotaDetail{ReasonCode: "RATE_LIMIT_EXCEEDED"}})
	if count, _, _ := m.rateCap.account(a.ID); count != 1 {
		t.Fatalf("retry refund removed wrong send: %d", count)
	}
	m.rateCap.mu.Lock()
	defer m.rateCap.mu.Unlock()
	if got := m.rateCap.metrics.bucket(m.rateCap.nowFunc()).Attempts; got != 2 {
		t.Fatalf("upstream attempts=%d, want 2 including rejection", got)
	}
}

func TestRateCapAllOverloadedFailsFast(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint("stream=", stream), func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			registerOverloadAuths(t, m, 2)
			m.SetRetryConfig(5, 60, 0)
			m.SetConfig(&internalconfig.Config{ProviderTransientErrorCooldownSeconds: map[string]int{"codex": 15}, ProviderRequestRateCap: map[string]internalconfig.RequestRateCapConfig{"codex": {Enabled: true, FailFast: true}}})
			var calls atomic.Int32
			fail := func(ctx context.Context) { calls.Add(1); cliproxyexecutor.MarkUpstreamAttempt(ctx) }
			m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				fail(ctx)
				return cliproxyexecutor.Response{}, customStatusError{code: 502, msg: "overload"}
			}}, streamFn: func(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				fail(ctx)
				return nil, customStatusError{code: 502, msg: "overload"}
			}})
			var err error
			if stream {
				_, err = m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra"}, cliproxyexecutor.Options{})
			} else {
				_, err = m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra"}, cliproxyexecutor.Options{})
			}
			if !isPoolCapacityError(err) || calls.Load() != 2 {
				t.Fatalf("expected immediate 529 after 2 sends: calls=%d err=%v", calls.Load(), err)
			}
			snapshot := m.CodexPoolSnapshot()
			if snapshot.Cooling != 2 || snapshot.FastFailures != 1 {
				t.Fatalf("pool = %+v", snapshot)
			}
		})
	}
}

func TestRateCapCredentialBudgetStillChecksCapacity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint("stream=", stream), func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			registerOverloadAuths(t, m, 1)
			m.SetRetryConfig(0, 60, 1)
			m.SetConfig(&internalconfig.Config{ProviderTransientErrorCooldownSeconds: map[string]int{"codex": 15}, ProviderRequestRateCap: map[string]internalconfig.RequestRateCapConfig{"codex": {Enabled: true, FailFast: true}}})
			m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				cliproxyexecutor.MarkUpstreamAttempt(ctx)
				return cliproxyexecutor.Response{}, customStatusError{code: 502, msg: "overload"}
			}}, streamFn: func(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				cliproxyexecutor.MarkUpstreamAttempt(ctx)
				return nil, customStatusError{code: 502, msg: "overload"}
			}})
			var err error
			if stream {
				_, err = m.ExecuteStream(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra"}, cliproxyexecutor.Options{})
			} else {
				_, err = m.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.6-terra"}, cliproxyexecutor.Options{})
			}
			if !isPoolCapacityError(err) {
				t.Fatalf("credential budget hid capacity: %v", err)
			}
		})
	}
}

func TestRateCapZeroWeightFollowsSelectionStrategy(t *testing.T) {
	for _, strategy := range []string{"round-robin", "fill-first", "weighted-round-robin"} {
		for _, affinity := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/affinity=%t/stream=%t", strategy, affinity, stream), func(t *testing.T) {
					var selector Selector
					switch strategy {
					case "round-robin":
						selector = &RoundRobinSelector{}
					case "fill-first":
						selector = &FillFirstSelector{}
					default:
						selector = &WeightedRoundRobinSelector{}
					}
					if affinity {
						selector = NewSessionAffinitySelector(selector)
					}
					m := NewManager(nil, selector, nil)
					ids := registerOverloadAuths(t, m, 1)
					a := m.List()[0]
					a.Attributes[AttributeWeight] = "0"
					if _, err := m.Update(context.Background(), a); err != nil {
						t.Fatal(err)
					}
					m.SetRetryConfig(0, 60, 1)
					m.SetConfig(&internalconfig.Config{ProviderTransientErrorCooldownSeconds: map[string]int{"codex": 15}, ProviderRequestRateCap: map[string]internalconfig.RequestRateCapConfig{"codex": {Enabled: true, FailFast: true}}})
					var calls int
					fail := func(ctx context.Context) error {
						calls++
						cliproxyexecutor.MarkUpstreamAttempt(ctx)
						return customStatusError{code: 502, msg: "overload"}
					}
					m.RegisterExecutor(&customStreamMockExecutor{identifier: "codex", mockCustomErrorExecutor: mockCustomErrorExecutor{executeFn: func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
						return cliproxyexecutor.Response{}, fail(ctx)
					}}, streamFn: func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
						return nil, fail(ctx)
					}})
					var err error
					request := cliproxyexecutor.Request{Model: "gpt-5.6-terra"}
					if stream {
						_, err = m.ExecuteStream(context.Background(), []string{"codex"}, request, cliproxyexecutor.Options{})
					} else {
						_, err = m.Execute(context.Background(), []string{"codex"}, request, cliproxyexecutor.Options{})
					}
					if strategy == "weighted-round-robin" {
						if calls != 0 || isPoolCapacityError(err) {
							t.Fatalf("weighted selection admitted excluded account: calls=%d err=%v", calls, err)
						}
						// A cooldown on an excluded credential must not turn an empty eligible pool into capacity failure.
						m.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "codex", Model: request.Model, Error: &Error{HTTPStatus: 502}})
						_, _, _, err = m.pickNextMixed(context.Background(), []string{"codex"}, request.Model, cliproxyexecutor.Options{}, nil)
						if isPoolCapacityError(err) {
							t.Fatalf("excluded credential counted as cooling capacity: %v", err)
						}
					} else if calls != 1 || !isPoolCapacityError(err) {
						t.Fatalf("expected one upstream attempt then 529: calls=%d err=%v", calls, err)
					}
				})
			}
		}
	}
}
