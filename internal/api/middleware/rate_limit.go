package middleware

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimiterConfig describes the parameters of the token-bucket limiter for a
// single request. It is intentionally decoupled from internal/config so this
// package has no dependency on the application configuration types.
type RateLimiterConfig struct {
	// RequestsPerSecond is the sustained rate at which tokens refill.
	RequestsPerSecond float64
	// Burst is the maximum number of tokens (requests) a key can accumulate.
	Burst int
}

// tokenBucket tracks the remaining tokens and bookkeeping timestamps for one
// limiter key (e.g. an API key or a client IP).
type tokenBucket struct {
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time
}

const (
	rateLimiterCleanupInterval = 10 * time.Minute
	rateLimiterIdleTTL         = 30 * time.Minute
)

// RateLimiter is an in-memory, per-key token-bucket limiter suitable for a single
// process instance. It is safe for concurrent use.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket

	stopOnce sync.Once
	stopCh   chan struct{}
}

// NewRateLimiter creates a limiter and starts a background goroutine that purges
// idle keys so the bucket map does not grow without bound.
func NewRateLimiter() *RateLimiter {
	rl := &RateLimiter{
		buckets: make(map[string]*tokenBucket),
		stopCh:  make(chan struct{}),
	}
	go rl.cleanupLoop()
	return rl
}

func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rateLimiterCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			rl.purgeStale()
		case <-rl.stopCh:
			return
		}
	}
}

func (rl *RateLimiter) purgeStale() {
	cutoff := time.Now().Add(-rateLimiterIdleTTL)
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for key, b := range rl.buckets {
		if b.lastSeen.Before(cutoff) {
			delete(rl.buckets, key)
		}
	}
}

// Stop terminates the background cleanup goroutine. Safe to call multiple times
// and safe to call on a nil receiver.
func (rl *RateLimiter) Stop() {
	if rl == nil {
		return
	}
	rl.stopOnce.Do(func() { close(rl.stopCh) })
}

// Allow consumes a token for key under the given rate/burst, returning whether the
// request is permitted and, when it is not, a suggested retry-after duration.
func (rl *RateLimiter) Allow(key string, cfg RateLimiterConfig) (bool, time.Duration) {
	rps := cfg.RequestsPerSecond
	if rps <= 0 {
		rps = 5
	}
	burst := cfg.Burst
	if burst <= 0 {
		burst = 20
	}

	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[key]
	if !ok {
		rl.buckets[key] = &tokenBucket{tokens: float64(burst) - 1, lastRefill: now, lastSeen: now}
		return true, 0
	}

	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * rps
		if b.tokens > float64(burst) {
			b.tokens = float64(burst)
		}
		b.lastRefill = now
	}
	b.lastSeen = now

	if b.tokens < 1 {
		wait := time.Duration((1 - b.tokens) / rps * float64(time.Second))
		return false, wait
	}
	b.tokens--
	return true, 0
}

// Middleware returns a gin.HandlerFunc enforcing the limiter. getConfig is called on
// every request so hot-reloaded configuration takes effect immediately; its second
// return value gates the whole check (false means "disabled", i.e. a no-op
// middleware), which is how this stays opt-in/default-disabled. getKey derives the
// limiter key (e.g. the authenticated API key) for the request; when it returns an
// empty string the client IP is used instead.
func (rl *RateLimiter) Middleware(getConfig func() (RateLimiterConfig, bool), getKey func(*gin.Context) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rl == nil || getConfig == nil {
			c.Next()
			return
		}
		cfg, enabled := getConfig()
		if !enabled {
			c.Next()
			return
		}
		key := ""
		if getKey != nil {
			key = getKey(c)
		}
		if key == "" {
			key = "ip:" + c.ClientIP()
		}
		allowed, retryAfter := rl.Allow(key, cfg)
		if !allowed {
			if retryAfter > 0 {
				c.Header("Retry-After", strconv.Itoa(int(retryAfter.Seconds()+1)))
			}
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
			return
		}
		c.Next()
	}
}
