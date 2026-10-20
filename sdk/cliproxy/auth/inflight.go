package auth

import (
	"context"
	"strings"
	"sync"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// inFlightCounter tracks how many upstream executions each credential is serving right now.
// It exists for observability (the management page) and is independent of Home-mode
// credential concurrency accounting.
type inFlightCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *inFlightCounter) add(id string, delta int) {
	if id == "" {
		return
	}
	c.mu.Lock()
	if c.counts == nil {
		c.counts = make(map[string]int)
	}
	next := c.counts[id] + delta
	if next <= 0 {
		delete(c.counts, id)
	} else {
		c.counts[id] = next
	}
	c.mu.Unlock()
}

func (c *inFlightCounter) get(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[id]
}

func (c *inFlightCounter) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.counts))
	for id, n := range c.counts {
		out[id] = n
	}
	return out
}

var (
	providerInFlightLimitMu sync.RWMutex
	providerInFlightLimits  map[string]int
)

// SetProviderInFlightLimit configures how many requests one credential of a provider may serve
// at the same time (keyed by lower-cased provider name). 0 or a missing provider means unlimited.
func SetProviderInFlightLimit(limits map[string]int) {
	normalized := make(map[string]int, len(limits))
	for provider, limit := range limits {
		key := strings.ToLower(strings.TrimSpace(provider))
		if key == "" || limit <= 0 {
			continue
		}
		normalized[key] = limit
	}
	providerInFlightLimitMu.Lock()
	providerInFlightLimits = normalized
	providerInFlightLimitMu.Unlock()
}

// inFlightLimitsConfigured reports whether any provider has an in-flight cap.
func inFlightLimitsConfigured() bool {
	providerInFlightLimitMu.RLock()
	defer providerInFlightLimitMu.RUnlock()
	return len(providerInFlightLimits) > 0
}

// providerInFlightLimitFor returns the in-flight cap for the provider, 0 when unlimited.
func providerInFlightLimitFor(provider string) int {
	providerInFlightLimitMu.RLock()
	defer providerInFlightLimitMu.RUnlock()
	if len(providerInFlightLimits) == 0 {
		return 0
	}
	return providerInFlightLimits[strings.ToLower(strings.TrimSpace(provider))]
}

// saturated reports whether the credential has reached the in-flight cap of its provider.
func (c *inFlightCounter) saturated(auth *Auth) bool {
	if auth == nil {
		return false
	}
	limit := providerInFlightLimitFor(auth.Provider)
	if limit <= 0 {
		return false
	}
	return c.get(auth.ID) >= limit
}

// dropSaturatedAuths removes credentials at their in-flight cap from the priority buckets and
// reports how many were dropped. The input map is left untouched.
func (c *inFlightCounter) dropSaturatedAuths(buckets map[int][]*Auth) (map[int][]*Auth, int) {
	if !inFlightLimitsConfigured() {
		return buckets, 0
	}
	dropped := 0
	out := make(map[int][]*Auth, len(buckets))
	for priority, bucket := range buckets {
		kept := make([]*Auth, 0, len(bucket))
		for _, auth := range bucket {
			if c.saturated(auth) {
				dropped++
				continue
			}
			kept = append(kept, auth)
		}
		if len(kept) > 0 {
			out[priority] = kept
		}
	}
	return out, dropped
}

// beginInFlight marks one execution in progress for the credential and returns a release
// function that is safe to call more than once.
func (m *Manager) beginInFlight(id string) func() {
	if m == nil || id == "" {
		return func() {}
	}
	m.inFlight.add(id, 1)
	var once sync.Once
	return func() { once.Do(func() { m.inFlight.add(id, -1) }) }
}

// InFlight returns the number of executions the credential is currently serving.
func (m *Manager) InFlight(id string) int {
	if m == nil {
		return 0
	}
	return m.inFlight.get(id)
}

// InFlightLimit returns the in-flight cap that applies to the credential, 0 when unlimited.
func (m *Manager) InFlightLimit(id string) int {
	if m == nil || id == "" {
		return 0
	}
	m.mu.RLock()
	auth := m.auths[id]
	m.mu.RUnlock()
	if auth == nil {
		return 0
	}
	return providerInFlightLimitFor(auth.Provider)
}

// InFlightSnapshot returns a copy of the per-credential in-flight counts.
func (m *Manager) InFlightSnapshot() map[string]int {
	if m == nil {
		return map[string]int{}
	}
	return m.inFlight.snapshot()
}

// wrapInFlightStream forwards the upstream chunks and calls release once the stream closes,
// errors out, or the caller's context ends.
func wrapInFlightStream(ctx context.Context, result *cliproxyexecutor.StreamResult, release func()) *cliproxyexecutor.StreamResult {
	if result == nil || result.Chunks == nil || release == nil {
		if release != nil {
			release()
		}
		return result
	}
	if ctx == nil {
		ctx = context.Background()
	}
	src := result.Chunks
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer release()
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-src:
				if !ok {
					return
				}
				select {
				case out <- chunk:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	wrapped := *result
	wrapped.Chunks = out
	return &wrapped
}
