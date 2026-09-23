package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// sizedRequest builds a POST whose Content-Length equals size without allocating
// the body up front.
func sizedRequest(size int64) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	req.ContentLength = size
	return req
}

func TestConcurrencyGateBodyBudgetQueuesLargeBodies(t *testing.T) {
	gate := NewConcurrencyGate(ConcurrencyGateConfig{BodyLimit: 10 << 20, WaitTimeout: 5 * time.Second})
	if gate.Limit() != 0 || gate.BodyLimit() != 10<<20 {
		t.Fatalf("unexpected limits: count=%d body=%d", gate.Limit(), gate.BodyLimit())
	}
	release := make(chan struct{})
	engine := newGateEngine(gate, func(e *gin.Engine) {
		e.POST("/v1/chat/completions", func(c *gin.Context) {
			<-release
			c.String(http.StatusOK, "ok")
		})
	})

	done := make(chan int, 3)
	serve := func(size int64) {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, sizedRequest(size))
		done <- rec.Code
	}
	// 6MiB + 3MiB fit; a further 3MiB would exceed 10MiB and must wait.
	go serve(6 << 20)
	go serve(3 << 20)
	if !waitFor(time.Second, func() bool { return gate.InFlightBytes() == 9<<20 }) {
		t.Fatalf("expected 9MiB in flight, got %d", gate.InFlightBytes())
	}
	go serve(3 << 20)
	if !waitFor(time.Second, func() bool { _, waiting := gate.Stats(); return waiting == 1 }) {
		t.Fatal("third request did not queue on the body budget")
	}
	if inFlight, _ := gate.Stats(); inFlight != 2 {
		t.Fatalf("expected 2 admitted, got %d", inFlight)
	}

	// Small requests are not blocked by the queued large one.
	rec := httptest.NewRecorder()
	go func() { engine.ServeHTTP(rec, sizedRequest(512)) }()
	if !waitFor(time.Second, func() bool { inFlight, _ := gate.Stats(); return inFlight == 3 }) {
		t.Fatal("small request was not admitted alongside the queued large one")
	}

	close(release)
	for i := 0; i < 3; i++ {
		select {
		case code := <-done:
			if code != http.StatusOK {
				t.Fatalf("request %d got %d", i, code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for requests")
		}
	}
	if !waitFor(time.Second, func() bool { inFlight, _ := gate.Stats(); return inFlight == 0 && gate.InFlightBytes() == 0 }) {
		t.Fatalf("budget not fully released: inflight bytes %d", gate.InFlightBytes())
	}
}

func TestConcurrencyGateBodyBudgetOversizedAndUnknownLength(t *testing.T) {
	gate := NewConcurrencyGate(ConcurrencyGateConfig{BodyLimit: 1 << 20, WaitTimeout: 5 * time.Second})
	release := make(chan struct{})
	engine := newGateEngine(gate, func(e *gin.Engine) {
		e.POST("/v1/chat/completions", func(c *gin.Context) {
			<-release
			c.String(http.StatusOK, "ok")
		})
	})
	// A body larger than the whole budget is served when the gate is idle; its
	// charge is clamped to the budget.
	go func() { engine.ServeHTTP(httptest.NewRecorder(), sizedRequest(8<<20)) }()
	if !waitFor(time.Second, func() bool { return gate.InFlightBytes() == 1<<20 }) {
		t.Fatal("oversized request was not admitted while idle")
	}
	// An unknown-length body is charged the fixed allowance and must wait now.
	go func() { engine.ServeHTTP(httptest.NewRecorder(), chunkedRequest()) }()
	if !waitFor(time.Second, func() bool { _, waiting := gate.Stats(); return waiting == 1 }) {
		t.Fatal("unknown-length request did not queue behind the oversized one")
	}
	close(release)
	if !waitFor(time.Second, func() bool { inFlight, waiting := gate.Stats(); return inFlight == 0 && waiting == 0 }) {
		t.Fatal("gate did not drain")
	}
	if got, provisional := concurrencyGateRequestCost(httptest.NewRequest(http.MethodPost, "/x", nil)); got != 0 || provisional {
		t.Fatalf("empty body cost = %d provisional=%v, want 0/false", got, provisional)
	}
	if got, provisional := concurrencyGateRequestCost(chunkedRequest()); got != concurrencyGateUnknownBodyCost || !provisional {
		t.Fatalf("unknown length cost = %d provisional=%v", got, provisional)
	}
}

// chunkedRequest builds a POST whose length is unknown at admission (ContentLength -1).
func chunkedRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	return req
}

func TestConcurrencyGateEmptyBodiesAreFree(t *testing.T) {
	gate := NewConcurrencyGate(ConcurrencyGateConfig{BodyLimit: 1 << 20, WaitTimeout: time.Second})
	release := make(chan struct{})
	engine := newGateEngine(gate, func(e *gin.Engine) {
		e.GET("/v1/models", func(c *gin.Context) { <-release; c.String(http.StatusOK, "ok") })
	})
	for i := 0; i < 8; i++ {
		go func() {
			engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		}()
	}
	if !waitFor(time.Second, func() bool { inFlight, _ := gate.Stats(); return inFlight == 8 }) {
		t.Fatal("body-less requests were held back by the body budget")
	}
	if gate.InFlightBytes() != 0 {
		t.Fatalf("empty bodies charged %d bytes", gate.InFlightBytes())
	}
	close(release)
}

func TestConcurrencyGateHugeContentLengthCannotOverflow(t *testing.T) {
	gate := NewConcurrencyGate(ConcurrencyGateConfig{BodyLimit: 4 << 20, WaitTimeout: 50 * time.Millisecond})
	release := make(chan struct{})
	engine := newGateEngine(gate, func(e *gin.Engine) {
		e.POST("/v1/chat/completions", func(c *gin.Context) { <-release; c.String(http.StatusOK, "ok") })
	})
	go func() { engine.ServeHTTP(httptest.NewRecorder(), sizedRequest(1<<20)) }()
	if !waitFor(time.Second, func() bool { return gate.InFlightBytes() == 1<<20 }) {
		t.Fatal("first request not admitted")
	}
	// A claimed length near MaxInt64 must queue (and time out), not wrap past the budget.
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, sizedRequest(1<<62))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("huge Content-Length was admitted alongside another request: %d", rec.Code)
	}
	close(release)
	if !waitFor(time.Second, func() bool { inFlight, _ := gate.Stats(); return inFlight == 0 }) {
		t.Fatal("gate did not drain")
	}
	// Alone, the same request is admitted, charged at most the budget, and released cleanly.
	release = make(chan struct{})
	go func() { engine.ServeHTTP(httptest.NewRecorder(), sizedRequest(1<<62)) }()
	if !waitFor(time.Second, func() bool { return gate.InFlightBytes() == 4<<20 }) {
		t.Fatalf("oversized charge not clamped to the budget: %d", gate.InFlightBytes())
	}
	close(release)
	if !waitFor(time.Second, func() bool { return gate.InFlightBytes() == 0 }) {
		t.Fatalf("in-flight bytes did not return to zero: %d", gate.InFlightBytes())
	}
}

func TestConcurrencyGateTruesUpUnknownLength(t *testing.T) {
	gate := NewConcurrencyGate(ConcurrencyGateConfig{BodyLimit: 32 << 20, WaitTimeout: time.Second})
	release := make(chan struct{})
	engine := newGateEngine(gate, func(e *gin.Engine) {
		e.POST("/v1/chat/completions", func(c *gin.Context) {
			// The handler learns the real size after reading the body.
			ReportRequestBodySize(c, 30<<20)
			ReportRequestBodySize(c, 1) // repeat calls are ignored
			<-release
			c.String(http.StatusOK, "ok")
		})
	})
	go func() { engine.ServeHTTP(httptest.NewRecorder(), chunkedRequest()) }()
	if !waitFor(time.Second, func() bool { return gate.InFlightBytes() == 30<<20 }) {
		t.Fatalf("provisional charge was not trued up: %d", gate.InFlightBytes())
	}
	// A 4MiB request no longer fits next to the 30MiB body.
	go func() { engine.ServeHTTP(httptest.NewRecorder(), sizedRequest(4<<20)) }()
	if !waitFor(time.Second, func() bool { _, waiting := gate.Stats(); return waiting == 1 }) {
		t.Fatal("request was admitted although the trued-up total leaves no room")
	}
	// A Content-Length request is never trued up.
	if !waitFor(time.Second, func() bool { return gate.InFlightBytes() == 30<<20 }) {
		t.Fatalf("unexpected in-flight bytes: %d", gate.InFlightBytes())
	}
	close(release)
	if !waitFor(time.Second, func() bool {
		inFlight, waiting := gate.Stats()
		return inFlight == 0 && waiting == 0 && gate.InFlightBytes() == 0
	}) {
		t.Fatalf("gate did not drain cleanly: bytes=%d", gate.InFlightBytes())
	}
}

func TestConcurrencyGateBodyBudgetRejectsAfterTimeout(t *testing.T) {
	gate := NewConcurrencyGate(ConcurrencyGateConfig{BodyLimit: 1 << 20, WaitTimeout: 50 * time.Millisecond})
	release := make(chan struct{})
	engine := newGateEngine(gate, func(e *gin.Engine) {
		e.POST("/v1/chat/completions", func(c *gin.Context) {
			<-release
			c.String(http.StatusOK, "ok")
		})
	})
	go func() { engine.ServeHTTP(httptest.NewRecorder(), sizedRequest(1<<20)) }()
	if !waitFor(time.Second, func() bool { return gate.InFlightBytes() == 1<<20 }) {
		t.Fatal("first request not admitted")
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, sizedRequest(1))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 after wait timeout, got %d", rec.Code)
	}
	// Hot-reloading the budget to 0 disables it and admits immediately.
	gate.Update(ConcurrencyGateConfig{BodyLimit: 0, WaitTimeout: 50 * time.Millisecond})
	rec = httptest.NewRecorder()
	go func() { engine.ServeHTTP(rec, sizedRequest(4<<20)) }()
	if !waitFor(time.Second, func() bool { inFlight, _ := gate.Stats(); return inFlight == 1 }) {
		t.Fatal("request not admitted after disabling the budget")
	}
	close(release)
}
