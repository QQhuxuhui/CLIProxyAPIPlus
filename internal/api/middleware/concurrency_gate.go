// Package middleware provides HTTP middleware components for the CLI Proxy API server.
// This file contains the concurrency gate middleware that caps the number of
// in-flight downstream API requests, and optionally the bytes of request body they
// carry, so that a burst of large request bodies cannot exhaust process memory.
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
	// concurrencyGateUnknownBodyCost is provisionally charged against the body budget
	// for requests without a Content-Length header. It is sized like a large API
	// request because the handler reads the whole body regardless; the charge is
	// corrected to the real size through ReportRequestBodySize once the body is read.
	concurrencyGateUnknownBodyCost int64 = 8 << 20
	// concurrencyGateChargeContextKey stores the request's gateCharge in the Gin
	// context so the body reader can true up a provisional charge.
	concurrencyGateChargeContextKey = "CONCURRENCY_GATE_CHARGE"
	// concurrencyGateContextKey stores the admitting gate in the Gin context.
	concurrencyGateContextKey = "CONCURRENCY_GATE"
)

// gateCharge is the body budget held by one admitted request. provisional marks a
// charge based on the unknown-length allowance rather than a Content-Length.
type gateCharge struct {
	mu          sync.Mutex
	bytes       int64
	provisional bool
}

// ConcurrencyGateConfig carries the runtime tunables of the concurrency gate.
type ConcurrencyGateConfig struct {
	// Limit is the maximum number of concurrently served API requests.
	// Values <= 0 disable the gate.
	Limit int
	// WaitTimeout bounds how long a request may wait for a free slot.
	// Values <= 0 fall back to DefaultConcurrentRequestWaitTimeout.
	WaitTimeout time.Duration
	// BodyLimit is the maximum total Content-Length, in bytes, of concurrently served
	// API requests. The proxy holds several copies of a request body while it waits
	// for upstream, so this bounds memory in a way a request count cannot.
	// Values <= 0 disable the body budget. A request larger than the whole budget is
	// still admitted when nothing else is in flight.
	BodyLimit int64
}

// ConcurrencyGate limits the number of API requests that are processed at the same
// time. The gate answers 503 (never 429) when it cannot admit a request, because
// downstream callers commonly interpret 429 as an account-level rate limit.
type ConcurrencyGate struct {
	mu            sync.Mutex
	limit         int
	bodyLimit     int64
	waitTimeout   time.Duration
	changed       chan struct{}
	lastRejectLog atomic.Int64
	inFlight      atomic.Int64
	inFlightBytes atomic.Int64
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
	if cfg.BodyLimit <= 0 {
		cfg.BodyLimit = 0
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limit == cfg.Limit && g.bodyLimit == cfg.BodyLimit && g.waitTimeout == waitTimeout {
		return
	}
	g.limit = cfg.Limit
	g.bodyLimit = cfg.BodyLimit
	g.waitTimeout = waitTimeout
	g.signalChangeLocked()
}

// BodyLimit reports the configured body budget in bytes (0 when disabled).
func (g *ConcurrencyGate) BodyLimit() int64 {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.bodyLimit
}

// enabledLocked reports whether any limit is configured. g.mu must be held.
func (g *ConcurrencyGate) enabledLocked() bool {
	return g.limit > 0 || g.bodyLimit > 0
}

// clampCostLocked bounds a charge to the budget so arithmetic on the in-flight
// total cannot overflow, whatever Content-Length a client claims. g.mu must be held.
func (g *ConcurrencyGate) clampCostLocked(cost int64) int64 {
	if cost < 0 {
		return 0
	}
	if g.bodyLimit > 0 && cost > g.bodyLimit {
		return g.bodyLimit
	}
	return cost
}

// admitLocked reports whether a request costing cost body bytes fits under both
// limits. A body larger than the whole budget is admitted only when nothing else is
// in flight, so oversized requests are served one at a time instead of never.
// g.mu must be held.
func (g *ConcurrencyGate) admitLocked(cost int64) bool {
	if g.limit > 0 && g.inFlight.Load() >= int64(g.limit) {
		return false
	}
	if g.bodyLimit > 0 {
		used := g.inFlightBytes.Load()
		// Written as a subtraction so a huge cost cannot wrap the comparison.
		if used > 0 && cost > g.bodyLimit-used {
			return false
		}
	}
	return true
}

// acquireLocked charges an admitted request and records the charge on the Gin
// context when it is provisional. g.mu must be held.
func (g *ConcurrencyGate) acquireLocked(c *gin.Context, cost int64, provisional bool) *gateCharge {
	cost = g.clampCostLocked(cost)
	g.inFlight.Add(1)
	g.inFlightBytes.Add(cost)
	charge := &gateCharge{bytes: cost, provisional: provisional}
	if provisional && c != nil {
		c.Set(concurrencyGateChargeContextKey, charge)
	}
	return charge
}

// concurrencyGateRequestCost returns the body bytes charged for a request and
// whether the value is provisional: the Content-Length when the client sent one,
// nothing for an empty body, and the unknown-length allowance otherwise.
func concurrencyGateRequestCost(r *http.Request) (cost int64, provisional bool) {
	switch {
	case r == nil || r.ContentLength < 0:
		return concurrencyGateUnknownBodyCost, true
	case r.ContentLength == 0:
		return 0, false
	default:
		return r.ContentLength, false
	}
}

// ReportRequestBodySize corrects a provisional body charge once the handler has
// read a request whose length was unknown at admission. Later requests then see
// the real in-flight total. It is a no-op for requests admitted by Content-Length,
// requests that bypassed the gate, and repeat calls.
func (g *ConcurrencyGate) ReportRequestBodySize(c *gin.Context, size int64) {
	if g == nil || c == nil {
		return
	}
	value, ok := c.Get(concurrencyGateChargeContextKey)
	if !ok {
		return
	}
	charge, ok := value.(*gateCharge)
	if !ok {
		return
	}
	charge.mu.Lock()
	defer charge.mu.Unlock()
	if !charge.provisional {
		return
	}
	g.mu.Lock()
	actual := g.clampCostLocked(size)
	delta := actual - charge.bytes
	charge.bytes = actual
	charge.provisional = false
	g.inFlightBytes.Add(delta)
	if delta < 0 {
		g.signalChangeLocked()
	}
	g.mu.Unlock()
}

// ReportRequestBodySize forwards to the gate that admitted the request, if any.
// Handlers call it right after reading a body so unknown-length requests are
// charged their real size.
func ReportRequestBodySize(c *gin.Context, size int64) {
	if c == nil {
		return
	}
	value, ok := c.Get(concurrencyGateContextKey)
	if !ok {
		return
	}
	if gate, okGate := value.(*ConcurrencyGate); okGate {
		gate.ReportRequestBodySize(c, size)
	}
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

// InFlightBytes reports the body bytes currently charged to admitted requests.
func (g *ConcurrencyGate) InFlightBytes() int64 {
	if g == nil {
		return 0
	}
	return g.inFlightBytes.Load()
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

		cost, provisional := concurrencyGateRequestCost(c.Request)
		g.mu.Lock()
		if !g.enabledLocked() {
			g.mu.Unlock()
			c.Next()
			return
		}
		waitTimeout := g.waitTimeout
		cost = g.clampCostLocked(cost)
		c.Set(concurrencyGateContextKey, g)
		if g.admitLocked(cost) {
			charge := g.acquireLocked(c, cost, provisional)
			g.mu.Unlock()
			defer g.release(charge)
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
				if !g.enabledLocked() {
					g.waiting.Add(-1)
					g.mu.Unlock()
					c.Next()
					return
				}
				cost = g.clampCostLocked(cost)
				if g.admitLocked(cost) {
					limit := g.limit
					g.waiting.Add(-1)
					charge := g.acquireLocked(c, cost, provisional)
					g.mu.Unlock()
					if waited := time.Since(start); waited > concurrencyGateSlowWaitThreshold {
						log.Debugf("concurrency gate: request waited %s for a slot (limit %d, path %s)", waited.Round(time.Millisecond), limit, concurrencyGateRequestPath(c))
					}
					defer g.release(charge)
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

// release returns the request's slot and body bytes and wakes queued requests.
func (g *ConcurrencyGate) release(charge *gateCharge) {
	charge.mu.Lock()
	charge.provisional = false
	cost := charge.bytes
	charge.bytes = 0
	charge.mu.Unlock()
	g.mu.Lock()
	g.inFlight.Add(-1)
	g.inFlightBytes.Add(-cost)
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
	log.Warnf("concurrency gate: rejected request with 503 (%s after %s, limit %d, in-flight %d, in-flight body %dMiB/%dMiB, waiting %d, path %s)",
		reason, waited.Round(time.Millisecond), limit, inFlight, g.InFlightBytes()>>20, g.BodyLimit()>>20, waiting, path)
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
