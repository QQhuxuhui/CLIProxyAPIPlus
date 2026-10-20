package auth

import (
	"strconv"
	"strings"
)

// SetAuthPriorityInMemory rewrites Attributes["priority"] on the runtime copy of an
// auth and re-buckets the scheduler, without persisting to the backing store or
// firing update hooks. It is used by latency tiering, which must never touch the
// auth files on disk.
//
// It returns true when the stored priority actually changed.
func (m *Manager) SetAuthPriorityInMemory(id string, priority int) bool {
	if m == nil || strings.TrimSpace(id) == "" {
		return false
	}
	want := strconv.Itoa(priority)
	m.mu.Lock()
	existing, ok := m.auths[id]
	if !ok || existing == nil {
		m.mu.Unlock()
		return false
	}
	if strings.TrimSpace(existing.Attributes["priority"]) == want {
		m.mu.Unlock()
		return false
	}
	attrs := make(map[string]string, len(existing.Attributes)+1)
	for k, v := range existing.Attributes {
		attrs[k] = v
	}
	attrs["priority"] = want
	existing.Attributes = attrs
	snapshot := existing.Clone()
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	return true
}

// AuthPriorityInMemory returns the current runtime priority of an auth and
// whether the auth exists.
func (m *Manager) AuthPriorityInMemory(id string) (int, bool) {
	if m == nil || strings.TrimSpace(id) == "" {
		return 0, false
	}
	m.mu.RLock()
	existing, ok := m.auths[id]
	if !ok || existing == nil {
		m.mu.RUnlock()
		return 0, false
	}
	prio := authPriority(existing)
	m.mu.RUnlock()
	return prio, true
}

// InvalidateSessionAffinity drops every session binding that points at the auth,
// so the next turn of those sessions is routed by priority again.
func (m *Manager) InvalidateSessionAffinity(authID string) {
	m.invalidateSessionAffinity(authID)
}
