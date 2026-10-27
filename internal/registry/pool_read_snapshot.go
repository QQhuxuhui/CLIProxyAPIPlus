package registry

import (
	"maps"
	"strings"
	"sync"
)

type poolClientSnapshot struct {
	models []*ModelInfo
	epoch  uint64
}
type poolRegistrySnapshot struct {
	view       *ModelRegistry
	providers  map[string][]*ModelInfo
	webSearch  map[string]*bool
	generation uint64
}

// EnablePoolReads activates immutable read views before pooled requests start.
// Client support lists are published independently, avoiding an O(clients) copy
// for each single-client registration. The shared directory supports batching.
func (r *ModelRegistry) EnablePoolReads() {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.poolReads.Load() {
		return
	}
	r.poolReads.Store(true)
	r.poolGeneration.Store(r.generation)
	for id := range r.clientEpochs {
		r.publishPoolClientLocked(id)
	}
	r.publishPoolDirectoryLocked()
}
func (r *ModelRegistry) publishPoolClientLocked(id string) {
	if !r.poolReads.Load() {
		return
	}
	models, epoch := r.getModelsAndEpochForClientLocked(id)
	r.poolClients.Store(id, &poolClientSnapshot{models: models, epoch: epoch})
}
func (r *ModelRegistry) poolClient(id string) *poolClientSnapshot {
	if value, ok := r.poolClients.Load(id); ok {
		return value.(*poolClientSnapshot)
	}
	return &poolClientSnapshot{}
}
func (r *ModelRegistry) publishPoolDirectoryLocked() {
	if !r.poolReads.Load() {
		return
	}
	if !r.poolReadDirty && r.poolDirectory.Load() != nil {
		return
	}
	if r.poolReadBatch > 0 {
		r.poolReadDirty = true
		return
	}
	view := &ModelRegistry{models: make(map[string]*ModelRegistration)}
	for id, entry := range r.models {
		if entry == nil {
			continue
		}
		copyEntry := *entry
		copyEntry.Info = cloneModelInfo(entry.Info)
		copyEntry.Providers = maps.Clone(entry.Providers)
		copyEntry.SuspendedClients = nil
		copyEntry.InfoByProvider = make(map[string]*ModelInfo, len(entry.InfoByProvider))
		for p, info := range entry.InfoByProvider {
			copyEntry.InfoByProvider[p] = cloneModelInfo(info)
		}
		copyEntry.QuotaExceededClients = nil
		view.models[id] = &copyEntry
	}
	providers := make(map[string][]*ModelInfo)
	seen := make(map[string]map[string]bool)
	for client, ids := range r.clientModels {
		provider := r.clientProviders[client]
		if seen[provider] == nil {
			seen[provider] = make(map[string]bool)
		}
		for _, id := range ids {
			if seen[provider][id] {
				continue
			}
			seen[provider][id] = true
			info := r.clientModelInfos[client][id]
			if info == nil {
				if reg := r.models[id]; reg != nil {
					info = reg.Info
				}
			}
			if info != nil {
				providers[provider] = append(providers[provider], cloneModelInfo(info))
			}
		}
	}
	ids := make([]string, 0, len(r.models))
	for id := range r.models {
		ids = append(ids, id)
	}
	r.publishPoolAvailabilityLocked(ids)
	routes := make(map[string][]NativeCapabilityRoute)
	for client, models := range r.clientModels {
		for _, id := range models {
			var native *NativeCapabilities
			if info := r.clientModelInfos[client][id]; info != nil {
				native = info.NativeCapabilities
			}
			routes[id] = append(routes[id], NativeCapabilityRoute{Provider: r.clientProviders[client], NativeCapabilities: native})
		}
	}
	web := make(map[string]*bool, len(routes))
	for id, list := range routes {
		web[id] = ResolveResponsesWebSearchCapability(list)
	}
	if old := r.poolDirectory.Load(); old != nil {
		for id := range old.view.models {
			if r.models[id] == nil {
				r.poolAvailability.Delete(id)
			}
		}
	}
	r.poolAvailabilityDirty = nil
	r.poolDirectory.Store(&poolRegistrySnapshot{view: view, providers: providers, webSearch: web, generation: r.generation})
	r.poolGeneration.Add(1)
	r.poolReadDirty = false
}

// BeginPoolRegistrationBatch coalesces shared directory publication while keeping
// per-client epoch/support snapshots current for registration reconciliation.
func (r *ModelRegistry) BeginPoolRegistrationBatch() func() {
	if !r.poolReads.Load() {
		return func() {}
	}
	r.mutex.Lock()
	r.poolReadBatch++
	r.mutex.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mutex.Lock()
			defer r.mutex.Unlock()
			r.poolReadBatch--
			if r.poolReadBatch == 0 {
				r.publishPoolDirectoryLocked()
				r.flushPoolAvailabilityLocked()
			}
		})
	}
}

type ClientProjectionUpdate struct {
	AuthEpoch         uint64
	ClientID          string
	Epoch, Generation uint64
	Projections       []ClientModelProjection
}

// ApplyClientModelProjectionBatch takes the registry write lock once per publish
// cycle. An empty batch refreshes deadline-sensitive read views as time advances.
func (r *ModelRegistry) ApplyClientModelProjectionBatch(updates []ClientProjectionUpdate) []bool {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.poolAvailabilityBatch++
	applied := make([]bool, len(updates))
	for i, u := range updates {
		if u.AuthEpoch != 0 && r.clientAuthEpochs[u.ClientID] != u.AuthEpoch {
			continue
		}
		applied[i] = r.applyClientModelProjectionsLocked(strings.TrimSpace(u.ClientID), u.Epoch, u.Generation, u.Projections)
	}
	r.poolAvailabilityBatch--
	r.publishPoolDirectoryLocked()
	r.flushPoolAvailabilityLocked()
	return applied
}

// ClientModelIDs copies only identifiers for cold per-pool catalog maintenance.
func (r *ModelRegistry) ClientModelIDs(id string) []string {
	var models []*ModelInfo
	if r.poolReads.Load() {
		models = r.poolClient(id).models
	} else {
		models, _ = r.GetModelsAndEpochForClient(id)
	}
	ids := make([]string, 0, len(models))
	for _, info := range models {
		if info != nil {
			ids = append(ids, info.ID)
		}
	}
	return ids
}

// TrackClientAuthEpoch fences delayed projection tasks across auth replacement.
// Called during serialized cold lifecycle transitions, outside container locks.
// A fresh manager may start a new local epoch sequence after the previous owner stops.
func (r *ModelRegistry) TrackClientAuthEpoch(id string, epoch uint64) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.clientAuthEpochs == nil {
		r.clientAuthEpochs = make(map[string]uint64)
	}
	if epoch != r.clientAuthEpochs[id] {
		r.clientAuthEpochs[id] = epoch
		if r.clientGenerations != nil {
			r.clientGenerations[id] = uint64(0)
		}
	}
}
