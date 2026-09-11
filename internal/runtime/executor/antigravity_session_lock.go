package executor

import (
	"context"
	"sync"
)

const defaultAntigravitySessionLockEntries = 4096

type antigravitySessionLockEntry struct {
	sem  chan struct{}
	refs int
}

type antigravitySessionLockManager struct {
	mu         sync.Mutex
	entries    map[string]*antigravitySessionLockEntry
	maxEntries int
	changed    chan struct{}
}

func newAntigravitySessionLockManager(maxEntries int) *antigravitySessionLockManager {
	if maxEntries <= 0 {
		maxEntries = defaultAntigravitySessionLockEntries
	}
	return &antigravitySessionLockManager{
		entries:    make(map[string]*antigravitySessionLockEntry),
		maxEntries: maxEntries,
		changed:    make(chan struct{}),
	}
}

func (m *antigravitySessionLockManager) Acquire(ctx context.Context, key string) (func(), error) {
	if m == nil || key == "" {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	for {
		m.mu.Lock()
		if m.entries == nil {
			m.entries = make(map[string]*antigravitySessionLockEntry)
		}
		if m.changed == nil {
			m.changed = make(chan struct{})
		}
		if entry := m.entries[key]; entry != nil {
			entry.refs++
			m.mu.Unlock()
			return m.waitForEntry(ctx, key, entry)
		}
		if len(m.entries) < m.maxEntries {
			entry := &antigravitySessionLockEntry{sem: make(chan struct{}, 1), refs: 1}
			entry.sem <- struct{}{}
			<-entry.sem
			m.entries[key] = entry
			m.mu.Unlock()
			return m.releaseFunc(key, entry), nil
		}
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (m *antigravitySessionLockManager) refCount(key string) int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry := m.entries[key]; entry != nil {
		return entry.refs
	}
	return 0
}

func (m *antigravitySessionLockManager) waitForEntry(ctx context.Context, key string, entry *antigravitySessionLockEntry) (func(), error) {
	select {
	case <-entry.sem:
		return m.releaseFunc(key, entry), nil
	case <-ctx.Done():
		m.mu.Lock()
		if current := m.entries[key]; current == entry {
			entry.refs--
			if entry.refs == 0 {
				delete(m.entries, key)
				m.signalChangeLocked()
			}
		}
		m.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (m *antigravitySessionLockManager) releaseFunc(key string, entry *antigravitySessionLockEntry) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.sem <- struct{}{}
			m.mu.Lock()
			if current := m.entries[key]; current == entry {
				entry.refs--
				if entry.refs == 0 {
					delete(m.entries, key)
					m.signalChangeLocked()
				}
			}
			m.mu.Unlock()
		})
	}
}

func (m *antigravitySessionLockManager) signalChangeLocked() {
	if m.changed != nil {
		close(m.changed)
	}
	m.changed = make(chan struct{})
}
