package middleware

import (
	"sync"
	"time"
)

const (
	// authLockoutMaxFailures is the number of consecutive failed authentication
	// attempts from a single IP that triggers a ban.
	authLockoutMaxFailures = 5
	// authLockoutBanDuration is how long a banned IP stays locked out.
	authLockoutBanDuration = 30 * time.Minute
	// authLockoutCleanupInterval controls how often stale IP entries are purged.
	authLockoutCleanupInterval = 10 * time.Minute
	// authLockoutIdleTTL is how long an IP entry can be idle (and not actively
	// banned) before the purge goroutine drops it.
	authLockoutIdleTTL = 30 * time.Minute
)

// lockoutEntry tracks failed-authentication bookkeeping for a single client IP.
type lockoutEntry struct {
	count        int
	blockedUntil time.Time
	lastActivity time.Time
}

// AuthLockout is an in-memory, per-IP failed-authentication lockout for the data
// plane. It mirrors the management API's proven brute-force lockout: after
// maxFailures consecutive invalid-credential responses from one IP, that IP is
// banned for banDuration. It is always-on (only ever punishes repeated INVALID
// credentials, so correct clients are never affected) and safe for concurrent
// use. A background goroutine purges stale entries so the map stays bounded.
type AuthLockout struct {
	mu       sync.Mutex
	attempts map[string]*lockoutEntry

	maxFailures int
	banDuration time.Duration

	stopOnce sync.Once
	stopCh   chan struct{}
}

// NewAuthLockout creates a lockout and starts its background purge goroutine.
func NewAuthLockout() *AuthLockout {
	al := &AuthLockout{
		attempts:    make(map[string]*lockoutEntry),
		maxFailures: authLockoutMaxFailures,
		banDuration: authLockoutBanDuration,
		stopCh:      make(chan struct{}),
	}
	go al.cleanupLoop()
	return al
}

func (al *AuthLockout) cleanupLoop() {
	ticker := time.NewTicker(authLockoutCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			al.purgeStale()
		case <-al.stopCh:
			return
		}
	}
}

// purgeStale drops entries that are idle beyond the TTL and whose ban (if any)
// has already expired, keeping the attempts map bounded.
func (al *AuthLockout) purgeStale() {
	if al == nil {
		return
	}
	now := time.Now()
	al.mu.Lock()
	defer al.mu.Unlock()
	for ip, e := range al.attempts {
		// Keep entries whose ban is still active.
		if !e.blockedUntil.IsZero() && now.Before(e.blockedUntil) {
			continue
		}
		if now.Sub(e.lastActivity) > authLockoutIdleTTL {
			delete(al.attempts, ip)
		}
	}
}

// Stop terminates the background purge goroutine. Safe to call multiple times
// and safe to call on a nil receiver.
func (al *AuthLockout) Stop() {
	if al == nil {
		return
	}
	al.stopOnce.Do(func() { close(al.stopCh) })
}

// Banned reports whether ip is currently locked out and, if so, the remaining
// ban duration. An expired ban is cleared as a side effect so the caller starts
// fresh. Safe on a nil receiver.
func (al *AuthLockout) Banned(ip string) (bool, time.Duration) {
	if al == nil {
		return false, 0
	}
	now := time.Now()
	al.mu.Lock()
	defer al.mu.Unlock()
	e := al.attempts[ip]
	if e == nil || e.blockedUntil.IsZero() {
		return false, 0
	}
	if now.Before(e.blockedUntil) {
		return true, e.blockedUntil.Sub(now)
	}
	// Ban expired; reset state so the client is no longer throttled.
	e.blockedUntil = time.Time{}
	e.count = 0
	return false, 0
}

// RecordFailure registers one failed authentication for ip. Reaching maxFailures
// bans the IP for banDuration and resets the running count. Safe on a nil
// receiver.
func (al *AuthLockout) RecordFailure(ip string) {
	if al == nil {
		return
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	e := al.attempts[ip]
	if e == nil {
		e = &lockoutEntry{}
		al.attempts[ip] = e
	}
	e.count++
	e.lastActivity = time.Now()
	if e.count >= al.maxFailures {
		e.blockedUntil = time.Now().Add(al.banDuration)
		e.count = 0
	}
}

// RecordSuccess clears the failure count and any ban for ip. Safe on a nil
// receiver.
func (al *AuthLockout) RecordSuccess(ip string) {
	if al == nil {
		return
	}
	al.mu.Lock()
	defer al.mu.Unlock()
	if e := al.attempts[ip]; e != nil {
		e.count = 0
		e.blockedUntil = time.Time{}
		e.lastActivity = time.Now()
	}
}
