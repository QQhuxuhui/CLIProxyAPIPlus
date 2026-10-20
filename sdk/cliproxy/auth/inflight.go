package auth

import (
	"context"
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
