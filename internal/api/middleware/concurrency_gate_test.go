package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newGateEngine builds a Gin engine wired like the production server: recovery first,
// then the concurrency gate, then the routes.
func newGateEngine(gate *ConcurrencyGate, register func(*gin.Engine)) *gin.Engine {
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(gate.Handler())
	register(engine)
	return engine
}

// waitFor polls cond until it becomes true or the timeout elapses.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

func TestConcurrencyGateDisabledPassthrough(t *testing.T) {
	gate := NewConcurrencyGate(ConcurrencyGateConfig{Limit: 0})
	if gate.Limit() != 0 {
		t.Fatalf("expected limit 0, got %d", gate.Limit())
	}

	engine := newGateEngine(gate, func(e *gin.Engine) {
		e.POST("/v1/chat/completions", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
			if rec.Code != http.StatusOK {
				t.Errorf("expected 200, got %d", rec.Code)
			}
		}()
	}
	wg.Wait()

	if inFlight, waiting := gate.Stats(); inFlight != 0 || waiting != 0 {
		t.Fatalf("expected no accounting when disabled, got inFlight=%d waiting=%d", inFlight, waiting)
	}
}

func TestConcurrencyGateLimitsAndQueues(t *testing.T) {
	gate := NewConcurrencyGate(ConcurrencyGateConfig{Limit: 2, WaitTimeout: 5 * time.Second})

	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	engine := newGateEngine(gate, func(e *gin.Engine) {
		e.POST("/v1/messages", func(c *gin.Context) {
			entered <- struct{}{}
			<-release
			c.String(http.StatusOK, "ok")
		})
	})

	codes := make([]int, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
			codes[idx] = rec.Code
		}(i)
	}

	// Exactly two requests may run at once.
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("expected 2 handlers to start, only %d did", i)
		}
	}
	select {
	case <-entered:
		t.Fatal("third handler started while the limit was saturated")
	case <-time.After(150 * time.Millisecond):
	}
	if inFlight, waiting := gate.Stats(); inFlight != 2 || waiting != 1 {
		t.Fatalf("expected inFlight=2 waiting=1, got inFlight=%d waiting=%d", inFlight, waiting)
	}

	// Releasing one slot admits the queued request.
	release <- struct{}{}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("queued request was not admitted after a slot was freed")
	}

	release <- struct{}{}
	release <- struct{}{}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, code)
		}
	}
	if !waitFor(time.Second, func() bool {
		inFlight, waiting := gate.Stats()
		return inFlight == 0 && waiting == 0
	}) {
		inFlight, waiting := gate.Stats()
		t.Fatalf("slots not released: inFlight=%d waiting=%d", inFlight, waiting)
	}
}

func TestConcurrencyGateRejectsWithServiceUnavailable(t *testing.T) {
	gate := NewConcurrencyGate(ConcurrencyGateConfig{Limit: 1, WaitTimeout: 50 * time.Millisecond})

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	engine := newGateEngine(gate, func(e *gin.Engine) {
		e.POST("/v1/chat/completions", func(c *gin.Context) {
			entered <- struct{}{}
			<-release
			c.String(http.StatusOK, "ok")
		})
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	}()
	<-entered

	start := time.Now()
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	waited := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if rec.Code == http.StatusTooManyRequests {
		t.Fatal("the gate must never answer 429")
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("expected Retry-After: 1, got %q", got)
	}
	if waited < 40*time.Millisecond {
		t.Fatalf("expected the request to wait for the timeout, waited %s", waited)
	}

	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if payload.Error.Message == "" {
		t.Fatal("expected a non-empty error message")
	}
	if payload.Error.Type != "server_error" || payload.Error.Code != "server_busy" {
		t.Fatalf("unexpected error envelope: %+v", payload.Error)
	}

	release <- struct{}{}
	wg.Wait()
}

func TestConcurrencyGateSkipsNonAPITraffic(t *testing.T) {
	gate := NewConcurrencyGate(ConcurrencyGateConfig{Limit: 1, WaitTimeout: 50 * time.Millisecond})

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	skipped := func(c *gin.Context) { c.String(http.StatusOK, "skipped") }
	engine := newGateEngine(gate, func(e *gin.Engine) {
		e.POST("/v1/chat/completions", func(c *gin.Context) {
			entered <- struct{}{}
			<-release
			c.String(http.StatusOK, "ok")
		})
		e.GET("/", skipped)
		e.GET("/health", skipped)
		e.GET("/healthz", skipped)
		e.GET("/management.html", skipped)
		e.GET("/v0/management/config", skipped)
		e.GET("/management/config", skipped)
		e.OPTIONS("/v1/chat/completions", skipped)
		e.GET("/v1/responses", skipped)
	})

	// Saturate the single slot.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	}()
	<-entered

	cases := []struct {
		name   string
		method string
		path   string
		header map[string]string
	}{
		{name: "root", method: http.MethodGet, path: "/"},
		{name: "health", method: http.MethodGet, path: "/health"},
		{name: "healthz", method: http.MethodGet, path: "/healthz"},
		{name: "management page", method: http.MethodGet, path: "/management.html"},
		{name: "v0 management", method: http.MethodGet, path: "/v0/management/config"},
		{name: "management", method: http.MethodGet, path: "/management/config"},
		{name: "options preflight", method: http.MethodOptions, path: "/v1/chat/completions"},
		{name: "websocket upgrade", method: http.MethodGet, path: "/v1/responses", header: map[string]string{"Connection": "Upgrade", "Upgrade": "websocket"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			start := time.Now()
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200 for %s, got %d", tc.path, rec.Code)
			}
			if elapsed := time.Since(start); elapsed > 40*time.Millisecond {
				t.Fatalf("skipped path %s waited for a slot (%s)", tc.path, elapsed)
			}
		})
	}

	// The gated request still holds the only slot.
	if inFlight, _ := gate.Stats(); inFlight != 1 {
		t.Fatalf("expected inFlight=1, got %d", inFlight)
	}

	release <- struct{}{}
	wg.Wait()
}

func TestConcurrencyGateClientCancelDuringWait(t *testing.T) {
	gate := NewConcurrencyGate(ConcurrencyGateConfig{Limit: 1, WaitTimeout: 10 * time.Second})

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	engine := newGateEngine(gate, func(e *gin.Engine) {
		e.POST("/v1/messages", func(c *gin.Context) {
			entered <- struct{}{}
			<-release
			c.String(http.StatusOK, "ok")
		})
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	cancelledRec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.ServeHTTP(cancelledRec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx))
	}()

	if !waitFor(2*time.Second, func() bool { _, waiting := gate.Stats(); return waiting == 1 }) {
		t.Fatal("second request never started waiting")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled request did not return")
	}
	if cancelledRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for the cancelled request, got %d", cancelledRec.Code)
	}
	if _, waiting := gate.Stats(); waiting != 0 {
		t.Fatalf("expected waiting=0 after cancellation, got %d", waiting)
	}
	if inFlight, _ := gate.Stats(); inFlight != 1 {
		t.Fatalf("cancelled request must not consume a slot, inFlight=%d", inFlight)
	}

	release <- struct{}{}
	wg.Wait()

	if !waitFor(time.Second, func() bool { inFlight, _ := gate.Stats(); return inFlight == 0 }) {
		t.Fatal("slot was not released after the first request finished")
	}

	// The gate still admits traffic afterwards.
	go func() { release <- struct{}{} }()
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after cancellation, got %d", rec.Code)
	}
}

func TestConcurrencyGateReleasesSlotOnPanic(t *testing.T) {
	gate := NewConcurrencyGate(ConcurrencyGateConfig{Limit: 1, WaitTimeout: 2 * time.Second})

	engine := newGateEngine(gate, func(e *gin.Engine) {
		e.POST("/v1/panic", func(c *gin.Context) { panic("boom") })
		e.POST("/v1/ok", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	})

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/panic", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 from gin.Recovery, got %d", rec.Code)
	}
	if inFlight, waiting := gate.Stats(); inFlight != 0 || waiting != 0 {
		t.Fatalf("panic leaked a slot: inFlight=%d waiting=%d", inFlight, waiting)
	}

	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ok", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after the panic, got %d", rec.Code)
	}
}

func TestConcurrencyGateUpdateResizesLimit(t *testing.T) {
	gate := NewConcurrencyGate(ConcurrencyGateConfig{Limit: 0})
	if gate.Limit() != 0 {
		t.Fatalf("expected the gate to start disabled, got limit %d", gate.Limit())
	}

	gate.Update(ConcurrencyGateConfig{Limit: 3, WaitTimeout: time.Second})
	if gate.Limit() != 3 {
		t.Fatalf("expected limit 3 after update, got %d", gate.Limit())
	}

	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	engine := newGateEngine(gate, func(e *gin.Engine) {
		e.POST("/v1/messages", func(c *gin.Context) {
			entered <- struct{}{}
			<-release
			c.String(http.StatusOK, "ok")
		})
	})

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		}()
	}
	for i := 0; i < 3; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("expected 3 concurrent handlers, only %d started", i)
		}
	}
	for i := 0; i < 3; i++ {
		release <- struct{}{}
	}
	wg.Wait()

	// Disabling the gate turns it back into a pass-through.
	gate.Update(ConcurrencyGateConfig{Limit: 0})
	if gate.Limit() != 0 {
		t.Fatalf("expected limit 0 after disabling, got %d", gate.Limit())
	}
	go func() {
		for i := 0; i < 5; i++ {
			release <- struct{}{}
		}
	}()
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 while disabled, got %d", rec.Code)
		}
	}
}

func TestConcurrencyGateUpdateDoesNotStackActiveSemaphores(t *testing.T) {
	gate := NewConcurrencyGate(ConcurrencyGateConfig{Limit: 1, WaitTimeout: 2 * time.Second})

	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	engine := newGateEngine(gate, func(e *gin.Engine) {
		e.POST("/v1/messages", func(c *gin.Context) {
			entered <- struct{}{}
			<-release
			c.String(http.StatusOK, "ok")
		})
	})

	var first sync.WaitGroup
	first.Add(1)
	go func() {
		defer first.Done()
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("initial request did not enter")
	}

	var queued sync.WaitGroup
	for i := 0; i < 2; i++ {
		queued.Add(1)
		go func() {
			defer queued.Done()
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
		}()
	}
	if !waitFor(time.Second, func() bool {
		_, waiting := gate.Stats()
		return waiting == 2
	}) {
		inFlight, waiting := gate.Stats()
		t.Fatalf("expected both requests to wait before resize, got inFlight=%d waiting=%d", inFlight, waiting)
	}

	gate.Update(ConcurrencyGateConfig{Limit: 2, WaitTimeout: 2 * time.Second})

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("updated limit did not admit one queued request")
	}
	select {
	case <-entered:
		t.Fatal("active requests from the old limit were stacked with the new semaphore")
	case <-time.After(100 * time.Millisecond):
	}

	if inFlight, waiting := gate.Stats(); inFlight != 2 || waiting != 1 {
		t.Fatalf("expected inFlight=2 waiting=1 after resize, got inFlight=%d waiting=%d", inFlight, waiting)
	}

	for i := 0; i < 2; i++ {
		release <- struct{}{}
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("remaining queued request was not admitted after a slot was freed")
	}
	release <- struct{}{}
	first.Wait()
	queued.Wait()
}
