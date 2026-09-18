package auth

import (
	"sync"
	"testing"
	"time"
)

func TestSessionCacheGetOrSetAliasesKeepsFirstBinding(t *testing.T) {
	cache := NewSessionCache(time.Minute)
	defer cache.Stop()

	if got := cache.GetOrSetAliases("auth-a", "session-1", "fallback-1"); got != "auth-a" {
		t.Fatalf("first GetOrSetAliases = %q, want auth-a", got)
	}
	if got := cache.GetOrSetAliases("auth-b", "session-1"); got != "auth-a" {
		t.Fatalf("second GetOrSetAliases = %q, want the existing auth-a binding", got)
	}
	if got, ok := cache.Get("fallback-1"); !ok || got != "auth-a" {
		t.Fatalf("fallback alias = %q/%v, want auth-a bound", got, ok)
	}
}

func TestSessionCacheGetOrSetAliasesConcurrentFirstTurnsAgree(t *testing.T) {
	cache := NewSessionCache(time.Minute)
	defer cache.Stop()

	const workers = 32
	results := make([]string, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = cache.GetOrSetAliases("auth-"+string(rune('a'+i%26)), "session-race")
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 1; i < workers; i++ {
		if results[i] != results[0] {
			t.Fatalf("worker %d bound %q, worker 0 bound %q; concurrent first turns must agree", i, results[i], results[0])
		}
	}
}

func TestSessionCacheGetOrSetAliasesReplacesExpiredBinding(t *testing.T) {
	cache := NewSessionCache(time.Minute)
	defer cache.Stop()

	cache.mu.Lock()
	cache.ensureInitializedLocked()
	cache.setAliasesLocked("auth-old", time.Now().Add(-2*time.Minute), "session-expired")
	cache.mu.Unlock()

	if got := cache.GetOrSetAliases("auth-new", "session-expired"); got != "auth-new" {
		t.Fatalf("GetOrSetAliases over an expired binding = %q, want auth-new", got)
	}
}
