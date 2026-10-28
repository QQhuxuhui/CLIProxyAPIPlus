package auth

import "maps"

// Published only by configuration/lifecycle setters under m.mu. Pool request
// paths read this immutable snapshot without acquiring the legacy account lock.
type managerSharedSnapshot struct {
	executors       map[string]ProviderExecutor
	selector        Selector
	pluginScheduler PluginScheduler
	rtProvider      RoundTripperProvider
	store           Store
	cooldownStore   CooldownStateStore
	refreshLoop     *authAutoRefreshLoop
}

func (m *Manager) publishSharedLocked() {
	m.shared.Store(&managerSharedSnapshot{executors: maps.Clone(m.executors), selector: m.selector, pluginScheduler: m.pluginScheduler, rtProvider: m.rtProvider, store: m.store, cooldownStore: m.cooldownStore, refreshLoop: m.refreshLoop})
}
func (m *Manager) cooldownStoreSnapshot() CooldownStateStore {
	if m.codexPools.Load() != nil {
		return m.shared.Load().cooldownStore
	}
	return m.cooldownStore
}
func (m *Manager) poolOwnsAuth(id string) bool {
	pools := m.codexPools.Load()
	if pools == nil {
		return false
	}
	kind := pools.owners.entry(id).lease.kind
	return kind == codexOwnerPooled || kind == codexOwnerSide
}
func (m *Manager) refreshLoopSnapshot() *authAutoRefreshLoop {
	if m.codexPools.Load() != nil {
		return m.shared.Load().refreshLoop
	}
	m.mu.RLock()
	loop := m.refreshLoop
	m.mu.RUnlock()
	return loop
}
