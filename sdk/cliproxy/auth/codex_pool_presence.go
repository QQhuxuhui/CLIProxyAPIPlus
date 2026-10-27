package auth

import (
	"strings"
	"sync/atomic"
)

func (m *Manager) adjustProviderPresence(old, next *Auth) {
	present := func(a *Auth) string {
		if a == nil || a.Disabled || a.Status == StatusDisabled {
			return ""
		}
		return strings.ToLower(strings.TrimSpace(a.Provider))
	}
	previous, current := present(old), present(next)
	if previous == current {
		return
	}
	change := func(provider string, delta int64) {
		if provider == "" {
			return
		}
		value, ok := m.providerPresence.Load(provider)
		if !ok {
			value, _ = m.providerPresence.LoadOrStore(provider, &atomic.Int64{})
		}
		value.(*atomic.Int64).Add(delta)
	}
	change(current, 1)
	change(previous, -1)
}
func (m *Manager) pooledProviderPresent(provider string) bool {
	value, ok := m.providerPresence.Load(strings.ToLower(strings.TrimSpace(provider)))
	return ok && value.(*atomic.Int64).Load() > 0
}
