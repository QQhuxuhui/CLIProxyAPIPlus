package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type concurrencyExecutor struct {
	retryRoundCallExecutor
	execute func(context.Context, *Auth) (exec.Response, error)
	stream  func(context.Context, *Auth) (*exec.StreamResult, error)
	refresh func(context.Context, *Auth) (*Auth, error)
}

func (e *concurrencyExecutor) Execute(ctx context.Context, a *Auth, _ exec.Request, _ exec.Options) (exec.Response, error) {
	e.mu.Lock()
	e.executeIDs = append(e.executeIDs, a.ID)
	e.mu.Unlock()
	if e.execute == nil {
		return exec.Response{Payload: []byte(`{"ok":true}`)}, nil
	}
	return e.execute(ctx, a)
}

func (e *concurrencyExecutor) ExecuteStream(ctx context.Context, a *Auth, _ exec.Request, _ exec.Options) (*exec.StreamResult, error) {
	e.mu.Lock()
	e.streamIDs = append(e.streamIDs, a.ID)
	e.mu.Unlock()
	return e.stream(ctx, a)
}

func (e *concurrencyExecutor) Refresh(ctx context.Context, a *Auth) (*Auth, error) {
	if e.refresh == nil {
		return a, nil
	}
	return e.refresh(ctx, a)
}

type concurrencyFixture struct {
	m     *Manager
	e     *concurrencyExecutor
	model string
	ids   []string
}

func concurrencyTestConfig(enabled bool, limit int, retryBudget bool) *internalconfig.Config {
	return &internalconfig.Config{Codex: internalconfig.CodexConfig{
		RetryBudget:        internalconfig.CodexRetryBudgetConfig{Enable: retryBudget},
		AccountConcurrency: internalconfig.CodexAccountConcurrencyConfig{Enabled: enabled, DefaultLimit: &limit},
	}}
}

func oauthCodex(id string) *Auth {
	return &Auth{ID: id, Provider: "codex", Status: StatusActive, Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth}, Metadata: map[string]any{"access_token": "t"}}
}

func addConcurrencyAccount(t *testing.T, f *concurrencyFixture, a *Auth) {
	t.Helper()
	registry.GetGlobalRegistry().RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: f.model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	if _, err := f.m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	f.ids = append(f.ids, a.ID)
}

func newConcurrencyFixture(t *testing.T, m *Manager, n int) *concurrencyFixture {
	t.Helper()
	e := &concurrencyExecutor{retryRoundCallExecutor: retryRoundCallExecutor{identifier: "codex"}}
	m.RegisterExecutor(e)
	m.SetRetryConfig(1, 0, 0)
	f := &concurrencyFixture{m: m, e: e, model: "concurrency-" + uuid.NewString()}
	for i := range n {
		addConcurrencyAccount(t, f, oauthCodex(fmt.Sprintf("%s-%02d", f.model, i)))
	}
	return f
}

func enabledFixture(t *testing.T, n, limit int, retryBudget bool) *concurrencyFixture {
	t.Helper()
	m := NewManager(nil, nil, nil)
	m.SetConfig(concurrencyTestConfig(true, limit, retryBudget))
	return newConcurrencyFixture(t, m, n)
}

// fill occupies every slot of the accounts and releases them when the test ends.
func (f *concurrencyFixture) fill(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		for {
			lease, ok := f.m.accountConcurrency.tryAcquire(oauthCodex(id))
			if !ok {
				break
			}
			if lease == nil {
				t.Fatal("nil lease")
			}
			t.Cleanup(lease.release)
			if f.m.accountConcurrency.limitFor(id) == 0 {
				break
			}
		}
	}
}

func (f *concurrencyFixture) inFlight(id string) int {
	return f.m.accountConcurrency.view().counts[id]
}

func (f *concurrencyFixture) idle(t *testing.T) {
	t.Helper()
	view := f.m.accountConcurrency.view()
	if len(view.counts) != 0 || view.total != 0 {
		t.Fatalf("leaked in-flight counts: %v total=%d", view.counts, view.total)
	}
}

func (f *concurrencyFixture) rejections() int {
	total := 0
	for _, point := range f.m.accountConcurrency.series() {
		total += point.ConcurrencyRejections
	}
	return total
}

func requireConcurrency529(t *testing.T, err error) {
	t.Helper()
	if !isConcurrencyCapacityError(err) {
		t.Fatalf("error = %v, want concurrency capacity error", err)
	}
	if status := statusCodeFromError(err); status != 529 {
		t.Fatalf("status = %d", status)
	}
	if got := SafeResponseHeaders(err).Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q", got)
	}
}

func TestAccountConcurrencyDefaultOffKeepsSchedulingButCounts(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{})
	f := newConcurrencyFixture(t, m, 1)
	if m.accountConcurrency.active.Load() {
		t.Fatal("zero config must leave the limit off")
	}
	if got := m.accountConcurrency.limitFor(f.ids[0]); got != 2 {
		t.Fatalf("default limit = %d", got)
	}
	for range 5 {
		lease, ok := m.accountConcurrency.tryAcquire(oauthCodex(f.ids[0]))
		if !ok {
			t.Fatal("disabled limit rejected")
		}
		t.Cleanup(lease.release)
	}
	if f.inFlight(f.ids[0]) != 5 {
		t.Fatalf("disabled limit must still count: %d", f.inFlight(f.ids[0]))
	}
	if _, err := m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{}); err != nil {
		t.Fatal(err)
	}
	if got := m.CodexPoolSnapshot(); got.ConcurrencyEnabled || got.InFlight != 5 {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestAccountConcurrencyTryAcquireRules(t *testing.T) {
	c := newAccountConcurrency()
	limit := 1
	c.configure(internalconfig.CodexAccountConcurrencyConfig{Enabled: true, DefaultLimit: &limit}, false)
	first, ok := c.tryAcquire(oauthCodex("a"))
	if !ok {
		t.Fatal("first acquire")
	}
	if _, ok = c.tryAcquire(oauthCodex("a")); ok {
		t.Fatal("over limit")
	}
	apiKey := &Auth{ID: "k", Provider: "codex", Attributes: map[string]string{AttributeAPIKey: "x"}}
	other := &Auth{ID: "c", Provider: "claude", Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth}}
	for _, a := range []*Auth{apiKey, apiKey, other, other} {
		lease, okLease := c.tryAcquire(a)
		if !okLease {
			t.Fatalf("inapplicable %s rejected", a.ID)
		}
		lease.release()
	}
	first.release()
	first.release()
	if view := c.view(); len(view.counts) != 0 || view.total != 0 {
		t.Fatalf("double release corrupted counts: %+v", view)
	}
}

func TestAccountConcurrencyTryAcquireRaceNeverExceedsLimit(t *testing.T) {
	c := newAccountConcurrency()
	limit := 3
	c.configure(internalconfig.CodexAccountConcurrencyConfig{Enabled: true, DefaultLimit: &limit}, false)
	a := oauthCodex("a")
	var current, peak atomic.Int64
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				lease, ok := c.tryAcquire(a)
				if !ok {
					continue
				}
				now := current.Add(1)
				for {
					old := peak.Load()
					if now <= old || peak.CompareAndSwap(old, now) {
						break
					}
				}
				current.Add(-1)
				lease.release()
			}
		}()
	}
	wg.Wait()
	if peak.Load() > 3 {
		t.Fatalf("peak = %d", peak.Load())
	}
	if view := c.view(); len(view.counts) != 0 {
		t.Fatalf("leak: %v", view.counts)
	}
}

// Requests racing through the real execution path never exceed the cap on any account.
func TestAccountConcurrencyExecuteRaceNeverExceedsLimit(t *testing.T) {
	f := enabledFixture(t, 3, 2, false)
	var mu sync.Mutex
	current, peak := map[string]int{}, map[string]int{}
	release := make(chan struct{})
	started := make(chan struct{}, 64)
	f.e.execute = func(_ context.Context, a *Auth) (exec.Response, error) {
		mu.Lock()
		current[a.ID]++
		peak[a.ID] = max(peak[a.ID], current[a.ID])
		mu.Unlock()
		started <- struct{}{}
		<-release
		mu.Lock()
		current[a.ID]--
		mu.Unlock()
		return exec.Response{}, nil
	}
	results := make(chan error, 64)
	for range 64 {
		go func() {
			_, err := f.m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
			results <- err
		}()
	}
	rejected := 0
	for range 64 - 6 {
		if err := <-results; err != nil {
			requireConcurrency529(t, err)
			rejected++
		}
	}
	for range 6 {
		<-started
	}
	close(release)
	for range 6 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if rejected != 58 {
		t.Fatalf("rejected = %d", rejected)
	}
	for id, n := range peak {
		if n > 2 {
			t.Fatalf("%s peak %d", id, n)
		}
	}
	f.idle(t)
}

type firstIDSelector struct{}

func (firstIDSelector) Pick(_ context.Context, _, _ string, _ exec.Options, auths []*Auth) (*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth"}
	}
	sorted := append([]*Auth(nil), auths...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	return sorted[0], nil
}

func selectionVariants() map[string]func(t *testing.T) *concurrencyFixture {
	return map[string]func(t *testing.T) *concurrencyFixture{
		"scheduler": func(t *testing.T) *concurrencyFixture { return enabledFixture(t, 2, 1, false) },
		"legacy": func(t *testing.T) *concurrencyFixture {
			m := NewManager(nil, firstIDSelector{}, nil)
			m.SetConfig(concurrencyTestConfig(true, 1, false))
			return newConcurrencyFixture(t, m, 2)
		},
		"plugin": func(t *testing.T) *concurrencyFixture {
			m := NewManager(nil, nil, nil)
			m.SetConfig(concurrencyTestConfig(true, 1, false))
			m.SetPluginScheduler(firstCandidateScheduler{})
			return newConcurrencyFixture(t, m, 2)
		},
		"pool": func(t *testing.T) *concurrencyFixture {
			m := newPoolStorageManager(t, 2)
			limit := 1
			m.accountConcurrency.configure(internalconfig.CodexAccountConcurrencyConfig{Enabled: true, DefaultLimit: &limit}, false)
			return newConcurrencyFixture(t, m, 2)
		},
	}
}

type firstCandidateScheduler struct{}

func (firstCandidateScheduler) PickAuth(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	if len(req.Candidates) == 0 {
		return pluginapi.SchedulerPickResponse{}, false, nil
	}
	sort.Slice(req.Candidates, func(i, j int) bool { return req.Candidates[i].ID < req.Candidates[j].ID })
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: req.Candidates[0].ID}, true, nil
}

func TestAccountConcurrencySelectionSkipsFullThenRejects(t *testing.T) {
	for name, build := range selectionVariants() {
		t.Run(name, func(t *testing.T) {
			f := build(t)
			f.fill(t, f.ids[0])
			for range 3 {
				selected, err := f.m.SelectAuth(context.Background(), "codex", f.model, exec.Options{})
				if err != nil {
					t.Fatal(err)
				}
				if selected.ID != f.ids[1] {
					t.Fatalf("selected full account %s", selected.ID)
				}
			}
			if f.inFlight(f.ids[1]) != 0 {
				t.Fatal("SelectAuth must only select, not reserve")
			}
			if _, err := f.m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{}); err != nil {
				t.Fatal(err)
			}
			f.fill(t, f.ids[1])
			_, err := f.m.SelectAuth(context.Background(), "codex", f.model, exec.Options{})
			requireConcurrency529(t, err)
			_, err = f.m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
			requireConcurrency529(t, err)
			if selected, errExempt := f.m.SelectAuthByKind(WithConcurrencyExempt(context.Background()), "codex", "", AuthKindOAuth, exec.Options{}); errExempt != nil || selected == nil {
				t.Fatalf("Live must select by the original rules: %v", errExempt)
			}
		})
	}
}

func TestAccountConcurrencyPinnedFullReturnsCapacityWithoutMigrating(t *testing.T) {
	for name, build := range selectionVariants() {
		t.Run(name, func(t *testing.T) {
			f := build(t)
			f.fill(t, f.ids[0])
			opts := exec.Options{Metadata: map[string]any{exec.PinnedAuthMetadataKey: f.ids[0]}}
			_, err := f.m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, opts)
			requireConcurrency529(t, err)
			if got := f.e.ids("execute"); len(got) != 0 {
				t.Fatalf("pinned request migrated: %v", got)
			}
		})
	}
}

func TestAccountConcurrencyRaceAfterFilterExcludesAccountAndRepicks(t *testing.T) {
	f := enabledFixture(t, 2, 1, false)
	// A slot disappears between filtering and reservation.
	hook := &raceFillSelector{f: f, target: f.ids[0], t: t}
	f.m.SetPluginScheduler(hook)
	if _, err := f.m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{}); err != nil {
		t.Fatal(err)
	}
	if got := f.e.ids("execute"); len(got) != 1 || got[0] != f.ids[1] {
		t.Fatalf("executed %v", got)
	}
}

// raceFillSelector takes the slot of the account it is about to return, as a racing request would.
type raceFillSelector struct {
	f      *concurrencyFixture
	target string
	t      *testing.T
	once   sync.Once
}

func (s *raceFillSelector) PickAuth(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	for _, candidate := range req.Candidates {
		if candidate.ID == s.target {
			s.once.Do(func() { s.f.fill(s.t, s.target) })
			return pluginapi.SchedulerPickResponse{Handled: true, AuthID: s.target}, true, nil
		}
	}
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: req.Candidates[0].ID}, true, nil
}

func TestAccountConcurrencyRaceOnOnlyAccountReturnsCapacity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		f := enabledFixture(t, 1, 1, false)
		f.m.SetPluginScheduler(&raceFillSelector{f: f, target: f.ids[0], t: t})
		var err error
		if stream {
			_, err = f.m.ExecuteStream(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
		} else {
			_, err = f.m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
		}
		requireConcurrency529(t, err)
		if f.rejections() != 1 {
			t.Fatalf("rejections = %d", f.rejections())
		}
	}
}

func TestAccountConcurrencySoftStickyMovesToFreeAccount(t *testing.T) {
	for name, pooled := range map[string]bool{"legacy": false, "pool": true} {
		t.Run(name, func(t *testing.T) {
			m := NewManager(nil, NewSessionAffinitySelector(&RoundRobinSelector{}), nil)
			if pooled {
				m.initializeCodexPools(2)
			}
			limit := 1
			m.accountConcurrency.configure(internalconfig.CodexAccountConcurrencyConfig{Enabled: true, DefaultLimit: &limit}, false)
			f := newConcurrencyFixture(t, m, 2)
			opts := exec.Options{Headers: http.Header{"Session-Id": {"sticky-session"}}}
			first, err := m.SelectAuth(context.Background(), "codex", f.model, opts)
			if err != nil {
				t.Fatal(err)
			}
			f.fill(t, first.ID)
			second, err := m.SelectAuth(context.Background(), "codex", f.model, opts)
			if err != nil {
				t.Fatal(err)
			}
			if second.ID == first.ID {
				t.Fatal("sticky session stayed on a full account")
			}
			other := f.ids[0]
			if other == first.ID {
				other = f.ids[1]
			}
			f.fill(t, other)
			_, err = m.SelectAuth(context.Background(), "codex", f.model, opts)
			requireConcurrency529(t, err)
		})
	}
}

func TestAccountConcurrencyCrossPoolFallback(t *testing.T) {
	m := newPoolStorageManager(t, 4)
	limit := 1
	m.accountConcurrency.configure(internalconfig.CodexAccountConcurrencyConfig{Enabled: true, DefaultLimit: &limit}, false)
	f := newConcurrencyFixture(t, m, 8)
	pools := m.codexPools.Load()
	preferred := pools.table.Load().owners[codexSlot("cross-pool-session")]
	var outside string
	for _, id := range f.ids {
		if pools.table.Load().owners[codexSlot(id)] == preferred {
			f.fill(t, id)
		} else if outside == "" {
			outside = id
		} else {
			f.fill(t, id)
		}
	}
	if outside == "" {
		t.Fatal("all accounts hashed into one pool")
	}
	selected, err := m.SelectAuth(context.Background(), "codex", f.model, exec.Options{Headers: http.Header{"Session-Id": {"cross-pool-session"}}})
	if err != nil || selected.ID != outside {
		t.Fatalf("selected %v err=%v, want %s", selected, err, outside)
	}
	f.fill(t, outside)
	_, err = m.SelectAuth(context.Background(), "codex", f.model, exec.Options{})
	requireConcurrency529(t, err)
}

func TestAccountConcurrencyMixedProviderFallsThroughFullCodex(t *testing.T) {
	f := enabledFixture(t, 1, 1, false)
	other := &concurrencyExecutor{retryRoundCallExecutor: retryRoundCallExecutor{identifier: "claude"}}
	f.m.RegisterExecutor(other)
	claude := &Auth{ID: f.model + "-claude", Provider: "claude", Status: StatusActive}
	addConcurrencyAccount(t, f, claude)
	f.fill(t, f.ids[0])
	if _, err := f.m.Execute(context.Background(), []string{"codex", "claude"}, exec.Request{Model: f.model}, exec.Options{}); err != nil {
		t.Fatal(err)
	}
	if got := other.ids("execute"); len(got) != 1 {
		t.Fatalf("other provider not used: %v", got)
	}
}

func TestAccountConcurrencyNonStreamReleasesOnEveryOutcome(t *testing.T) {
	failure := &Error{HTTPStatus: 500, Message: "boom"}
	for name, run := range map[string]func(context.Context, *Auth) (exec.Response, error){
		"success": func(context.Context, *Auth) (exec.Response, error) { return exec.Response{}, nil },
		"error":   func(context.Context, *Auth) (exec.Response, error) { return exec.Response{}, failure },
		"canceled": func(ctx context.Context, _ *Auth) (exec.Response, error) {
			<-ctx.Done()
			return exec.Response{}, ctx.Err()
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := enabledFixture(t, 2, 1, false)
			entered := make(chan string, 2)
			f.e.execute = func(ctx context.Context, a *Auth) (exec.Response, error) {
				entered <- a.ID
				if got := f.inFlight(a.ID); got != 1 {
					t.Errorf("in-flight during attempt = %d", got)
				}
				return run(ctx, a)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "canceled" {
				go func() { <-entered; cancel() }()
			}
			_, _ = f.m.Execute(ctx, []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
			f.idle(t)
		})
	}
}

func TestAccountConcurrencyPanicReleases(t *testing.T) {
	f := enabledFixture(t, 1, 1, false)
	f.e.execute = func(context.Context, *Auth) (exec.Response, error) { panic("executor panic") }
	f.e.stream = func(context.Context, *Auth) (*exec.StreamResult, error) { panic("executor panic") }
	for _, run := range []func(){
		func() {
			_, _ = f.m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
		},
		func() {
			_, _ = f.m.ExecuteStream(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
		},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("panic swallowed")
				}
			}()
			run()
		}()
		f.idle(t)
	}
}

func TestAccountConcurrencyAccountSwitchReleasesPreviousLease(t *testing.T) {
	f := enabledFixture(t, 3, 1, true)
	var attempts int
	f.e.execute = func(_ context.Context, a *Auth) (exec.Response, error) {
		attempts++
		held := f.m.accountConcurrency.view().total
		if held != 1 {
			t.Errorf("attempt %d holds %d slots", attempts, held)
		}
		if attempts == 1 {
			return exec.Response{}, &Error{HTTPStatus: 503, Message: "overloaded"}
		}
		return exec.Response{}, nil
	}
	if _, err := f.m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{}); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d", attempts)
	}
	f.idle(t)
}

func TestAccountConcurrencySameAccountRefreshDoesNotDoubleCount(t *testing.T) {
	f := enabledFixture(t, 1, 1, false)
	f.m.mu.Lock()
	f.m.auths[f.ids[0]].Metadata["refresh_token"] = "r"
	f.m.mu.Unlock()
	var attempts atomic.Int32
	f.e.refresh = func(_ context.Context, a *Auth) (*Auth, error) { return a.Clone(), nil }
	f.e.execute = func(_ context.Context, a *Auth) (exec.Response, error) {
		if got := f.inFlight(a.ID); got != 1 {
			t.Errorf("in-flight = %d", got)
		}
		if attempts.Add(1) == 1 {
			return exec.Response{}, &Error{HTTPStatus: http.StatusUnauthorized, Message: "expired"}
		}
		return exec.Response{}, nil
	}
	_, _ = f.m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
	f.idle(t)
}

func streamOf(chunks ...exec.StreamChunk) *exec.StreamResult {
	ch := make(chan exec.StreamChunk, len(chunks))
	for _, chunk := range chunks {
		ch <- chunk
	}
	close(ch)
	return &exec.StreamResult{Chunks: ch}
}

func awaitIdle(t *testing.T, f *concurrencyFixture) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	released := make(chan struct{})
	go func() {
		for f.m.accountConcurrency.view().total != 0 {
			time.Sleep(time.Millisecond)
		}
		close(released)
	}()
	select {
	case <-released:
	case <-deadline:
		t.Fatalf("lease was not released: %v", f.m.accountConcurrency.view().counts)
	}
}

func TestAccountConcurrencyStreamHoldsLeaseUntilStreamEnds(t *testing.T) {
	f := enabledFixture(t, 1, 1, false)
	chunks := make(chan exec.StreamChunk)
	f.e.stream = func(context.Context, *Auth) (*exec.StreamResult, error) {
		return &exec.StreamResult{Chunks: chunks}, nil
	}
	go func() { chunks <- exec.StreamChunk{Payload: []byte("first")} }()
	result, err := f.m.ExecuteStream(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f.inFlight(f.ids[0]) != 1 {
		t.Fatal("lease must outlive ExecuteStream")
	}
	if chunk := <-result.Chunks; string(chunk.Payload) != "first" {
		t.Fatalf("chunk = %q", chunk.Payload)
	}
	_, errFull := f.m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
	requireConcurrency529(t, errFull)
	close(chunks)
	for range result.Chunks {
	}
	awaitIdle(t, f)
}

func TestAccountConcurrencyStreamFailuresRelease(t *testing.T) {
	boom := &Error{HTTPStatus: 500, Message: "boom"}
	for name, stream := range map[string]func(context.Context, *Auth) (*exec.StreamResult, error){
		"start error": func(context.Context, *Auth) (*exec.StreamResult, error) { return nil, boom },
		"empty":       func(context.Context, *Auth) (*exec.StreamResult, error) { return streamOf(), nil },
		"bootstrap": func(context.Context, *Auth) (*exec.StreamResult, error) {
			return streamOf(exec.StreamChunk{Err: boom}), nil
		},
		"mid stream": func(context.Context, *Auth) (*exec.StreamResult, error) {
			return streamOf(exec.StreamChunk{Payload: []byte("x")}, exec.StreamChunk{Err: boom}), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := enabledFixture(t, 2, 1, false)
			f.e.stream = stream
			result, err := f.m.ExecuteStream(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
			if err == nil && result != nil {
				for range result.Chunks {
				}
			}
			awaitIdle(t, f)
		})
	}
}

func TestAccountConcurrencySilentStreamReleasesOnCancel(t *testing.T) {
	f := enabledFixture(t, 1, 1, false)
	chunks := make(chan exec.StreamChunk, 1)
	chunks <- exec.StreamChunk{Payload: []byte("first")}
	f.e.stream = func(context.Context, *Auth) (*exec.StreamResult, error) {
		return &exec.StreamResult{Chunks: chunks}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := f.m.ExecuteStream(ctx, []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	<-result.Chunks
	cancel()
	// The upstream stays silent and its channel stays open; the disconnect alone must free the slot.
	awaitIdle(t, f)
	close(chunks)
	for range result.Chunks {
	}
}

func TestAccountConcurrencyStreamRPMRejectionReleases(t *testing.T) {
	f := enabledFixture(t, 1, 1, false)
	cfg := concurrencyTestConfig(true, 1, false)
	cfg.ProviderRequestRateCap = map[string]internalconfig.RequestRateCapConfig{"codex": {Enabled: true, FailFast: true, InitialPerMinute: 1, MinPerMinute: 1}}
	f.m.SetConfig(cfg)
	f.m.rateCap.start(f.ids[0])
	f.e.stream = func(context.Context, *Auth) (*exec.StreamResult, error) { return streamOf(), nil }
	_, err := f.m.ExecuteStream(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
	if !isPoolCapacityError(err) || isConcurrencyCapacityError(err) {
		t.Fatalf("err = %v", err)
	}
	f.idle(t)
	if f.rejections() != 0 {
		t.Fatal("RPM rejection counted as concurrency")
	}
}

func TestAccountConcurrencyIgnoresRateCapOffAndBypass(t *testing.T) {
	f := enabledFixture(t, 1, 1, false)
	f.fill(t, f.ids[0])
	ctx := context.WithValue(context.Background(), rateBypassContextKey{}, true)
	_, err := f.m.SelectAuth(ctx, "codex", f.model, exec.Options{})
	requireConcurrency529(t, err)
	_, _, _, err = f.m.pickNextMixed(ctx, []string{"codex"}, f.model, exec.Options{}, nil)
	requireConcurrency529(t, err)
}

func TestAccountConcurrencyFullWritesNoFailureStateAndNoRPM(t *testing.T) {
	f := enabledFixture(t, 1, 1, false)
	cfg := concurrencyTestConfig(true, 1, false)
	cfg.ProviderRequestRateCap = map[string]internalconfig.RequestRateCapConfig{"codex": {Enabled: true}}
	f.m.SetConfig(cfg)
	f.fill(t, f.ids[0])
	_, err := f.m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
	requireConcurrency529(t, err)
	a, _ := f.m.GetByID(f.ids[0])
	if a.Failed != 0 || a.LastError != nil || a.Unavailable || len(a.ModelStates) != 0 {
		t.Fatalf("failure state written: %+v", a)
	}
	if count, _, _ := f.m.rateCap.account(f.ids[0]); count != 0 {
		t.Fatalf("RPM consumed: %d", count)
	}
	if got := f.m.CodexPoolSnapshot(); got.FastFailures != 0 {
		t.Fatalf("fast failures = %d", got.FastFailures)
	}
	if f.rejections() != 1 {
		t.Fatalf("rejections = %d", f.rejections())
	}
}

func TestAccountConcurrencyCapacityWinsOverEarlierFailures(t *testing.T) {
	failures := []struct {
		name    string
		failure error
	}{
		{"overload", &Error{HTTPStatus: 503, Message: "overloaded"}},
		{"quota", &Error{HTTPStatus: 429, Message: `{"error":{"type":"usage_limit_reached"}}`}},
	}
	for _, failure := range failures {
		for _, budget := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				for _, mixed := range []bool{false, true} {
					name := fmt.Sprintf("%s/budget=%t/stream=%t/mixed=%t", failure.name, budget, stream, mixed)
					t.Run(name, func(t *testing.T) {
						f := enabledFixture(t, 2, 1, budget)
						f.m.SetRetryConfig(1, 0, 0)
						providers := []string{"codex"}
						if mixed {
							providers = []string{"codex", "claude"}
							f.m.RegisterExecutor(&concurrencyExecutor{retryRoundCallExecutor: retryRoundCallExecutor{identifier: "claude"}})
						}
						fillOther := func(a *Auth) {
							for _, id := range f.ids {
								if id != a.ID {
									f.fill(t, id)
								}
							}
						}
						f.e.execute = func(_ context.Context, a *Auth) (exec.Response, error) {
							fillOther(a)
							return exec.Response{}, failure.failure
						}
						f.e.stream = func(_ context.Context, a *Auth) (*exec.StreamResult, error) {
							fillOther(a)
							return nil, failure.failure
						}
						var err error
						if stream {
							_, err = f.m.ExecuteStream(context.Background(), providers, exec.Request{Model: f.model}, exec.Options{})
						} else {
							_, err = f.m.Execute(context.Background(), providers, exec.Request{Model: f.model}, exec.Options{})
						}
						requireConcurrency529(t, err)
						if !strings.Contains(err.Error(), concurrencyLimitExceededReason) {
							t.Fatalf("body = %s", err)
						}
						if f.rejections() != 1 {
							t.Fatalf("rejections = %d", f.rejections())
						}
						if got := f.m.CodexPoolSnapshot(); got.FastFailures != 0 {
							t.Fatalf("fast failures = %d", got.FastFailures)
						}
					})
				}
			}
		}
	}
}

func TestAccountConcurrencyRPMRejectionKeepsExistingFinalError(t *testing.T) {
	f := enabledFixture(t, 2, 1, true)
	b := &codexRetryBudget{now: time.Now, deadline: time.Now().Add(time.Second), attempted: map[string]struct{}{}}
	b.observe(&Error{HTTPStatus: 503, Message: "overloaded"})
	rpm := newPoolCapacityError(time.Second)
	var final *codexRetryFinalError
	if err := f.m.codexFinalError(b, f.model, rpm); !errors.As(err, &final) {
		t.Fatalf("RPM capacity must keep overload precedence: %v", err)
	}
	if err := preferredExecutionAttemptError(&Error{HTTPStatus: 500}, newConcurrencyCapacityError()); !isConcurrencyCapacityError(err) {
		t.Fatalf("preferred = %v", err)
	}
}

func TestAccountConcurrencyStaysFinalWhenRateWindowIsAlsoFull(t *testing.T) {
	f := enabledFixture(t, 1, 1, false)
	cfg := concurrencyTestConfig(true, 1, false)
	cfg.ProviderRequestRateCap = map[string]internalconfig.RequestRateCapConfig{"codex": {Enabled: true, FailFast: true, InitialPerMinute: 1, MinPerMinute: 1}}
	f.m.SetConfig(cfg)
	f.fill(t, f.ids[0])
	f.m.rateCap.start(f.ids[0])
	_, err := f.m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
	requireConcurrency529(t, err)
	if got := f.m.CodexPoolSnapshot().FastFailures; got != 0 {
		t.Fatalf("fast failures = %d", got)
	}
}

func TestAccountConcurrencyFullPoolDoesNotLoop(t *testing.T) {
	for _, budget := range []bool{false, true} {
		t.Run(fmt.Sprintf("budget=%t", budget), func(t *testing.T) {
			f := enabledFixture(t, 2, 1, budget)
			f.m.SetRetryConfig(3, time.Hour, 3)
			f.fill(t, f.ids...)
			done := make(chan error, 1)
			go func() {
				_, err := f.m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
				done <- err
			}()
			select {
			case err := <-done:
				requireConcurrency529(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("capacity error looped")
			}
			if got := f.e.ids("execute"); len(got) != 0 {
				t.Fatalf("executed %v", got)
			}
		})
	}
}

func TestCodexRetryWaitStopsWhenHealthyAccountsAreFull(t *testing.T) {
	f := enabledFixture(t, 1, 1, true)
	ctx := WithCodexRetryBudget(context.Background())
	if wait, retry := f.m.codexRetryWait(ctx, f.model, exec.Options{}, time.Minute); !retry || wait != 0 {
		t.Fatalf("free account: wait=%v retry=%t", wait, retry)
	}
	f.fill(t, f.ids[0])
	if wait, retry := f.m.codexRetryWait(ctx, f.model, exec.Options{}, time.Minute); retry || wait != 0 {
		t.Fatalf("full account must stop the wait: wait=%v retry=%t", wait, retry)
	}
}

func TestAccountConcurrencyCooldownOutranksConcurrencyOnlyWhenNothingIsHealthy(t *testing.T) {
	f := enabledFixture(t, 2, 1, false)
	f.m.mu.Lock()
	cooling := f.m.auths[f.ids[1]]
	cooling.Unavailable, cooling.NextRetryAfter = true, time.Now().Add(time.Minute)
	f.m.mu.Unlock()
	f.m.syncScheduler()
	if _, err := f.m.SelectAuth(context.Background(), "codex", f.model, exec.Options{}); err != nil {
		t.Fatal(err)
	}
	f.fill(t, f.ids[0])
	_, err := f.m.SelectAuth(context.Background(), "codex", f.model, exec.Options{})
	requireConcurrency529(t, err)
}

func TestAccountConcurrencyLoweredLimitOnlyBlocksNewWork(t *testing.T) {
	f := enabledFixture(t, 1, 3, false)
	f.fill(t, f.ids[0])
	f.m.SetConfig(concurrencyTestConfig(true, 1, false))
	if f.inFlight(f.ids[0]) != 3 {
		t.Fatal("lowering the limit changed in-flight work")
	}
	_, err := f.m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
	requireConcurrency529(t, err)
}

func TestAccountConcurrencyEnablingUsesExistingCounts(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(concurrencyTestConfig(false, 1, false))
	f := newConcurrencyFixture(t, m, 1)
	lease, _ := m.accountConcurrency.tryAcquire(oauthCodex(f.ids[0]))
	defer lease.release()
	m.SetConfig(concurrencyTestConfig(true, 1, false))
	_, err := m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
	requireConcurrency529(t, err)
}

func TestAccountConcurrencyDeletedAccountReleasesAndCleans(t *testing.T) {
	f := enabledFixture(t, 1, 1, false)
	lease, ok := f.m.accountConcurrency.tryAcquire(oauthCodex(f.ids[0]))
	if !ok {
		t.Fatal("acquire")
	}
	f.m.Remove(context.Background(), f.ids[0])
	if f.inFlight(f.ids[0]) != 1 {
		t.Fatal("removal must not touch in-flight counts")
	}
	lease.release()
	f.idle(t)
}

func TestAccountConcurrencyHomeIsNeverActive(t *testing.T) {
	m := NewManager(nil, nil, nil)
	cfg := concurrencyTestConfig(true, 1, false)
	cfg.Home.Enabled = true
	m.SetConfig(cfg)
	if m.accountConcurrency.active.Load() {
		t.Fatal("configured limit must not apply under Home")
	}
	snapshot := m.CodexPoolSnapshot()
	if snapshot.ConcurrencyEnabled || snapshot.ConcurrencyCapacity != 0 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	cfg = concurrencyTestConfig(true, 1, false)
	m.SetConfig(cfg)
	if !m.accountConcurrency.active.Load() {
		t.Fatal("leaving Home must restore the switch")
	}
}

func TestAccountConcurrencyCurveUsesSamplesAndCountsRequests(t *testing.T) {
	f := enabledFixture(t, 2, 2, false)
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	f.m.accountConcurrency.nowFunc = func() time.Time { return now }
	first, _ := f.m.accountConcurrency.tryAcquire(oauthCodex(f.ids[0]))
	second, _ := f.m.accountConcurrency.tryAcquire(oauthCodex(f.ids[0]))
	f.m.sampleCodexConcurrency()
	first.release()
	second.release()
	now = now.Add(5 * time.Second)
	f.m.sampleCodexConcurrency()
	f.fill(t, f.ids...)
	_, err := f.m.Execute(context.Background(), []string{"codex"}, exec.Request{Model: f.model}, exec.Options{})
	requireConcurrency529(t, err)
	now = now.Add(5 * time.Second)
	points := f.m.CodexPoolSnapshot().ConcurrencyPoints
	if len(points) != 60 {
		t.Fatalf("points = %d", len(points))
	}
	last := points[59]
	if last.Time != now.Truncate(time.Minute) || last.InFlight != 0 || last.InFlightPeak != 2 || last.ConcurrencyCapacity != 4 || last.ConcurrencyRejections != 1 {
		t.Fatalf("point = %+v", last)
	}
	now = now.Add(time.Minute)
	points = f.m.CodexPoolSnapshot().ConcurrencyPoints
	if points[58].InFlightPeak != 2 || points[59].InFlightPeak != 0 {
		t.Fatalf("minute rollover: %+v %+v", points[58], points[59])
	}
	now = now.Add(61 * time.Minute)
	if points = f.m.CodexPoolSnapshot().ConcurrencyPoints; points[59].ConcurrencyCapacity != 0 || points[59].Time != now.Truncate(time.Minute) {
		t.Fatalf("stale bucket survived: %+v", points[59])
	}
}

func TestAccountConcurrencySnapshotUnlimitedAndCapacity(t *testing.T) {
	dir := t.TempDir()
	f := enabledFixture(t, 3, 2, false)
	f.m.accountConcurrency.store.open(dir)
	zero, plus := 0, 8
	if _, err := f.m.SetCodexAccountConcurrencyLimits(f.ids[:1], &zero); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.SetCodexAccountConcurrencyLimits(f.ids[1:2], &plus); err != nil {
		t.Fatal(err)
	}
	lease, _ := f.m.accountConcurrency.tryAcquire(oauthCodex(f.ids[2]))
	defer lease.release()
	got := f.m.CodexPoolSnapshot()
	if !got.ConcurrencyEnabled || got.InFlight != 1 || got.ConcurrencyCapacity != 10 || got.ConcurrencyRemaining != 9 || got.UnlimitedAccounts != 1 || got.ConcurrencyFullAccounts != 0 {
		t.Fatalf("snapshot = %+v", got)
	}
	view := f.m.CodexAccountConcurrencyView()
	entry, ok := view.Entry(oauthCodex(f.ids[0]))
	if !ok || entry.Limit != 0 || entry.Source != "override" || entry.Override == nil {
		t.Fatalf("entry = %+v", entry)
	}
	entry, ok = view.Entry(oauthCodex(f.ids[2]))
	if !ok || entry.InFlight != 1 || entry.Limit != 2 || entry.Source != "default" || entry.Override != nil {
		t.Fatalf("entry = %+v", entry)
	}
	if _, ok = view.Entry(&Auth{ID: "x", Provider: "claude"}); ok {
		t.Fatal("inapplicable account has an entry")
	}
}

func TestAccountConcurrencyPoolSnapshotDoesNotDoubleCountAfterMigration(t *testing.T) {
	m := newPoolStorageManager(t, 4)
	limit := 2
	m.accountConcurrency.configure(internalconfig.CodexAccountConcurrencyConfig{Enabled: true, DefaultLimit: &limit}, false)
	f := newConcurrencyFixture(t, m, 4)
	lease, _ := m.accountConcurrency.tryAcquire(oauthCodex(f.ids[0]))
	defer lease.release()
	before := m.CodexPoolSnapshot()
	pools := m.codexPools.Load()
	source := pools.table.Load().owners[codexSlot(f.ids[0])]
	var target *codexAccountPool
	for _, p := range pools.table.Load().pools {
		if p != source {
			target = p
			break
		}
	}
	if err := pools.moveSlot(context.Background(), codexSlot(f.ids[0]), target); err != nil {
		t.Fatal(err)
	}
	after := m.CodexPoolSnapshot()
	if before.ConcurrencyCapacity != 8 || after.ConcurrencyCapacity != 8 || after.InFlight != 1 {
		t.Fatalf("before=%d after=%d inflight=%d", before.ConcurrencyCapacity, after.ConcurrencyCapacity, after.InFlight)
	}
	partitions := m.CodexPoolsSnapshot()
	inFlight := 0
	for _, partition := range partitions.Pools {
		inFlight += partition.InFlight
	}
	if inFlight != 1 {
		t.Fatalf("partition in-flight = %d", inFlight)
	}
}

func TestAcquireAuthWithCredentialPolicyHoldsAndReleases(t *testing.T) {
	f := enabledFixture(t, 1, 1, false)
	f.m.mu.Lock()
	f.m.auths[f.ids[0]].Metadata["account_id"] = "acct"
	f.m.mu.Unlock()
	selected, release, err := f.m.AcquireAuthWithCredentialPolicy(context.Background(), "codex", f.model, CredentialPolicyCodexAlphaSearchV1, exec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if selected == nil || f.inFlight(f.ids[0]) != 1 {
		t.Fatal("search must hold a lease")
	}
	_, releaseFull, err := f.m.AcquireAuthWithCredentialPolicy(context.Background(), "codex", f.model, CredentialPolicyCodexAlphaSearchV1, exec.Options{})
	releaseFull()
	requireConcurrency529(t, err)
	if f.rejections() != 1 {
		t.Fatalf("rejections = %d", f.rejections())
	}
	release()
	release()
	f.idle(t)
}

func TestCodexProbeHoldsLeaseAndSkipsWhenFull(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(concurrencyTestConfig(true, 1, false))
	a := oauthCodex("a")
	m.auths["a"] = a
	registerCodexProbeTestClient(t, "a")
	before, _, _ := m.rateCap.account("a")
	probe, reason := m.AcquireCodexProbeAuth("gpt-6-luna")
	if probe == nil {
		t.Fatalf("acquire: %s", reason)
	}
	if got := m.accountConcurrency.view().counts["a"]; got != 1 {
		t.Fatalf("probe in-flight = %d", got)
	}
	counted, _, _ := m.rateCap.account("a")
	if counted != before+1 {
		t.Fatalf("rate attempts = %d", counted)
	}
	skipped, reason := m.AcquireCodexProbeAuth("gpt-6-luna")
	if skipped != nil || reason != CodexProbeConcurrencyFull {
		t.Fatalf("full account: %v %q", skipped, reason)
	}
	if after, _, _ := m.rateCap.account("a"); after != counted {
		t.Fatal("skipped probe consumed RPM")
	}
	m.MarkCodexProbeResult(context.Background(), probe, "gpt-6-luna", context.Canceled)
	if got := m.accountConcurrency.view(); len(got.counts) != 0 {
		t.Fatalf("leak: %v", got.counts)
	}
	m.ReleaseCodexProbeAuth(probe)
	if got := m.accountConcurrency.view(); got.total != 0 {
		t.Fatalf("double release: %+v", got)
	}
}

func TestCodexProbeCompetesWithBusinessRequests(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(concurrencyTestConfig(true, 2, false))
	m.auths["a"] = oauthCodex("a")
	registerCodexProbeTestClient(t, "a")
	business, _ := m.accountConcurrency.tryAcquire(oauthCodex("a"))
	defer business.release()
	probe, _ := m.AcquireCodexProbeAuth("gpt-6-luna")
	if probe == nil {
		t.Fatal("probe should take the second slot")
	}
	defer m.ReleaseCodexProbeAuth(probe)
	if _, ok := m.accountConcurrency.tryAcquire(oauthCodex("a")); ok {
		t.Fatal("business request exceeded the limit beside a probe")
	}
}

func TestPooledCodexProbeHoldsLease(t *testing.T) {
	m := newPoolStorageManager(t, 2)
	limit := 1
	m.accountConcurrency.configure(internalconfig.CodexAccountConcurrencyConfig{Enabled: true, DefaultLimit: &limit}, false)
	f := newConcurrencyFixture(t, m, 1)
	probe, reason := m.AcquireCodexProbeAuth(f.model)
	if probe == nil {
		t.Fatalf("acquire: %s", reason)
	}
	if skipped, why := m.AcquireCodexProbeAuth(f.model); skipped != nil || why != CodexProbeConcurrencyFull {
		t.Fatalf("full pool: %v %q", skipped, why)
	}
	m.ReleaseCodexProbeAuth(probe)
	f.idle(t)
}

func TestConcurrencyPickErrorKeepsCancellation(t *testing.T) {
	busy := newConcurrencyCapacityError()
	if got := concurrencyPickError(context.Canceled, busy); got != nil {
		t.Fatalf("cancellation replaced: %v", got)
	}
	if got := concurrencyPickError(&Error{Code: "auth_not_found"}, nil); got != nil {
		t.Fatalf("no capacity seen: %v", got)
	}
	if got := concurrencyPickError(&Error{Code: "auth_not_found"}, busy); got == nil {
		t.Fatal("lost race must surface capacity")
	}
}
