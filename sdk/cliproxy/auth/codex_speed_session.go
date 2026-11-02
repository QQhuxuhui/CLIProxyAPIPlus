package auth

import (
	"sync"
	"time"
)

// codexSpeedSessionTTL is the session lifetime when no affinity TTL is configured.
const codexSpeedSessionTTL = time.Hour

// codexSpeedSessions remembers which sessions already used their one replacement of a binding
// to an account outside the fast group. Entries slide with use and expire with the session.
type codexSpeedSessions struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

func (s *codexSpeedSessions) init() { s.entries = map[string]time.Time{} }

// rebound reports whether the session already used its replacement.
func (s *codexSpeedSessions) rebound(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.entries[key]
	return ok && now.Before(expires)
}

// markRebound records the replacement and extends the session's expiry.
func (s *codexSpeedSessions) markRebound(key string, ttl time.Duration, now time.Time) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = now.Add(ttl)
}

// prune drops expired sessions.
func (s *codexSpeedSessions) prune(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, expires := range s.entries {
		if !now.Before(expires) {
			delete(s.entries, key)
		}
	}
}
