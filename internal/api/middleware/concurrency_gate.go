// Package middleware provides HTTP middleware components for the CLI Proxy API server.
// This file contains the concurrency gate middleware that caps the number of
// in-flight downstream API requests so that a burst of large request bodies cannot
// exhaust process memory.
package middleware

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	// concurrencyGateSlowWaitThreshold is the wait duration above which an admitted
	// request is reported at debug level.
	concurrencyGateSlowWaitThreshold = time.Second
	// concurrencyGateRejectLogInterval rate-limits the rejection warning.
	concurrencyGateRejectLogInterval = 10 * time.Second
	// DefaultConcurrentRequestWaitTimeout is used when the configured wait timeout is
	// empty or invalid.
	DefaultConcurrentRequestWaitTimeout = 60 * time.Second
)

// ConcurrencyGateConfig carries the runtime tunables of the concurrency gate.
type ConcurrencyGateConfig struct {
	// Limit is the maximum number of concurrently served API requests.
	// Values <= 0 disable the gate.
	Limit int
	// WaitTimeout bounds how long a request may wait for a free slot.
	// Values <= 0 fall back to DefaultConcurrentRequestWaitTimeout.
	WaitTimeout time.Duration
}

// ConcurrencyGate limits the number of API requests that are processed at the same
// time. The gate answers 503 (never 429) when it cannot admit a request, because
// downstream callers commonly interpret 429 as an account-level rate limit.
type ConcurrencyGate struct {
	mu            sync.Mutex
	limit         int
	waitTimeout   time.Duration
	changed       chan struct{}
	lastRejectLog atomic.Int64
	inFlight      atomic.Int64
	waiting       atomic.Int64
}

// NewConcurrencyGate creates a gate for the supplied configuration.
func NewConcurrencyGate(cfg ConcurrencyGateConfig) *ConcurrencyGate {
	g := &ConcurrencyGate{changed: make(chan struct{})}
	g.Update(cfg)
	return g
}

// Update applies a new configuration. It is safe to call while requests are running.
// All requests share the same in-flight count, so changing the limit never creates a
// second independent capacity pool. When the limit is lowered below the active count,
// no new requests are admitted until enough active requests finish.
func (g *ConcurrencyGate) Update(cfg ConcurrencyGateConfig) {
	if g == nil {
		return
	}
	waitTimeout := cfg.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = DefaultConcurrentRequestWaitTimeout
	}
	if cfg.Limit <= 0 {
		cfg.Limit = 0
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limit == cfg.Limit && g.waitTimeout == waitTimeout {
		return
	}
	g.limit = cfg.Limit
	g.waitTimeout = waitTimeout
	g.signalChangeLocked()
}

// Limit reports the currently configured limit (0 when disabled).
func (g *ConcurrencyGate) Limit() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}

// Stats reports the number of requests currently holding a slot and the number of
// requests waiting for one.
func (g *ConcurrencyGate) Stats() (inFlight int64, waiting int64) {
	if g == nil {
		return 0, 0
	}
	return g.inFlight.Load(), g.waiting.Load()
}

// Handler returns the Gin middleware enforcing the gate. When the gate is disabled
// the handler returns immediately without touching the request.
func (g *ConcurrencyGate) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if g == nil {
			c.Next()
			return
		}
		if concurrencyGateSkip(c) {
			c.Next()
			return
		}

		g.mu.Lock()
		if g.limit <= 0 {
			g.mu.Unlock()
			c.Next()
			return
		}
		waitTimeout := g.waitTimeout
		if g.inFlight.Load() < int64(g.limit) {
			g.inFlight.Add(1)
			g.mu.Unlock()
			defer g.release()
			c.Next()
			return
		}
		changed := g.changed
		g.waiting.Add(1)
		g.mu.Unlock()

		// Slow path: wait for a slot, honouring both the client context and the
		// configured wait timeout. The timeout applies to slot acquisition only and
		// is never propagated into the request context.
		start := time.Now()
		timer := time.NewTimer(waitTimeout)
		defer timer.Stop()
		var clientGone <-chan struct{}
		if c.Request != nil {
			clientGone = c.Request.Context().Done()
		}
		for {
			select {
			case <-changed:
				g.mu.Lock()
				if g.limit <= 0 {
					g.waiting.Add(-1)
					g.mu.Unlock()
					c.Next()
					return
				}
				if g.inFlight.Load() < int64(g.limit) {
					limit := g.limit
					g.waiting.Add(-1)
					g.inFlight.Add(1)
					g.mu.Unlock()
					if waited := time.Since(start); waited > concurrencyGateSlowWaitThreshold {
						log.Debugf("concurrency gate: request waited %s for a slot (limit %d, path %s)", waited.Round(time.Millisecond), limit, concurrencyGateRequestPath(c))
					}
					defer g.release()
					c.Next()
					return
				}
				changed = g.changed
				g.mu.Unlock()
			case <-clientGone:
				g.waiting.Add(-1)
				g.reject(c, g.Limit(), time.Since(start), "client disconnected")
				return
			case <-timer.C:
				g.waiting.Add(-1)
				g.reject(c, g.Limit(), time.Since(start), "wait timeout")
				return
			}
		}
	}
}

// release returns one unit of shared capacity and wakes queued requests.
func (g *ConcurrencyGate) release() {
	g.mu.Lock()
	g.inFlight.Add(-1)
	g.signalChangeLocked()
	g.mu.Unlock()
}

// signalChangeLocked wakes all waiters so they can compete for newly available
// capacity or observe a hot-reloaded configuration. g.mu must be held.
func (g *ConcurrencyGate) signalChangeLocked() {
	if g.changed != nil {
		close(g.changed)
	}
	g.changed = make(chan struct{})
}

// reject answers 503 with the standard error envelope. 429 is intentionally never
// used here: downstream proxies treat it as an upstream account rate limit.
func (g *ConcurrencyGate) reject(c *gin.Context, limit int, waited time.Duration, reason string) {
	g.logReject(limit, waited, reason, concurrencyGateRequestPath(c))
	c.Header("Retry-After", "1")
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
		"error": gin.H{
			"message": "server is busy: too many concurrent requests, please retry",
			"type":    "server_error",
			"code":    "server_busy",
		},
	})
}

// logReject emits at most one warning per concurrencyGateRejectLogInterval.
func (g *ConcurrencyGate) logReject(limit int, waited time.Duration, reason, path string) {
	now := time.Now().UnixNano()
	last := g.lastRejectLog.Load()
	if last != 0 && now-last < int64(concurrencyGateRejectLogInterval) {
		return
	}
	if !g.lastRejectLog.CompareAndSwap(last, now) {
		return
	}
	inFlight, waiting := g.Stats()
	log.Warnf("concurrency gate: rejected request with 503 (%s after %s, limit %d, in-flight %d, waiting %d, path %s)",
		reason, waited.Round(time.Millisecond), limit, inFlight, waiting, path)
}

// concurrencyGateSkip reports whether the request must bypass the gate entirely.
// Management endpoints, health probes, the root page, CORS preflights and websocket
// upgrades must stay reachable even when the API is saturated.
func concurrencyGateSkip(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return true
	}
	if c.Request.Method == http.MethodOptions {
		return true
	}
	if concurrencyGateIsWebsocketUpgrade(c.Request) {
		return true
	}
	switch path := c.Request.URL.Path; path {
	case "/", "/health", "/healthz", "/management.html", "/quota-monitor.html":
		return true
	case "/v0/management", "/management":
		return true
	default:
		return strings.HasPrefix(path, "/v0/management/") || strings.HasPrefix(path, "/management/")
	}
}

// concurrencyGateIsWebsocketUpgrade reports whether the request asks for a websocket upgrade.
func concurrencyGateIsWebsocketUpgrade(r *http.Request) bool {
	if r == nil {
		return false
	}
	for _, value := range r.Header.Values("Upgrade") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "websocket") {
				return true
			}
		}
	}
	return false
}

// requestPath returns the request path for logging purposes.
func concurrencyGateRequestPath(c *gin.Context) string {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return ""
	}
	return c.Request.URL.Path
}
