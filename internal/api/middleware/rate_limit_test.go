package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRateLimiterMiddlewareDisabledByDefaultIsNoOp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rl := NewRateLimiter()
	defer rl.Stop()

	engine := gin.New()
	engine.Use(rl.Middleware(
		func() (RateLimiterConfig, bool) { return RateLimiterConfig{}, false },
		func(c *gin.Context) string { return "key:test" },
	))
	engine.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })

	for i := 0; i < 50; i++ {
		rr := httptest.NewRecorder()
		engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ping", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want %d (limiter disabled must never throttle)", i, rr.Code, http.StatusOK)
		}
	}
}

func TestRateLimiterMiddlewareEnforcesBurstThenRejects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rl := NewRateLimiter()
	defer rl.Stop()

	cfg := RateLimiterConfig{RequestsPerSecond: 1, Burst: 3}
	engine := gin.New()
	engine.Use(rl.Middleware(
		func() (RateLimiterConfig, bool) { return cfg, true },
		func(c *gin.Context) string { return "key:shared" },
	))
	engine.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })

	for i := 0; i < cfg.Burst; i++ {
		rr := httptest.NewRecorder()
		engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ping", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("burst request %d: status = %d, want %d", i, rr.Code, http.StatusOK)
		}
	}

	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("request beyond burst: status = %d, want %d; body=%s", rr.Code, http.StatusTooManyRequests, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header on 429 response")
	}
}

func TestRateLimiterMiddlewareKeysAreIndependent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rl := NewRateLimiter()
	defer rl.Stop()

	cfg := RateLimiterConfig{RequestsPerSecond: 1, Burst: 1}
	key := "key:a"
	engine := gin.New()
	engine.Use(rl.Middleware(
		func() (RateLimiterConfig, bool) { return cfg, true },
		func(c *gin.Context) string { return key },
	))
	engine.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })

	// Exhaust the burst for "key:a".
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("first request for key:a: status = %d, want %d", rr.Code, http.StatusOK)
	}
	rr = httptest.NewRecorder()
	engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second request for key:a: status = %d, want %d", rr.Code, http.StatusTooManyRequests)
	}

	// A different key must have its own, unaffected budget.
	key = "key:b"
	rr = httptest.NewRecorder()
	engine.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("first request for key:b: status = %d, want %d", rr.Code, http.StatusOK)
	}
}
