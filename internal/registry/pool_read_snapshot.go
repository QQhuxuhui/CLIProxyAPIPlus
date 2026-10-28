package registry

import (
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

type poolClientSnapshot struct {
	provider string
	models   []*ModelInfo
	epoch    uint64
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
	old := r.poolClient(id)
	models, epoch := r.getModelsAndEpochForClientLocked(id)
	next := &poolClientSnapshot{models: models, epoch: epoch, provider: r.clientProviders[id]}
	if r.poolRoutes == nil {
		r.poolRoutes = make(map[string]map[string]*poolRouteAggregate)
	}
	if r.poolDefinitionDirty == nil {
		r.poolDefinitionDirty = make(map[string]struct{})
	}
	for _, info := range old.models {
		r.poolDefinitionDirty[info.ID] = struct{}{}
		groups := r.poolRoutes[info.ID]
		group := groups[old.provider]
		if group != nil {
			group.remove(id, old.provider)
			if len(group.clients) == 0 {
				delete(groups, old.provider)
			}
		}
		if len(groups) == 0 {
			delete(r.poolRoutes, info.ID)
		}
	}
	for _, info := range models {
		r.poolDefinitionDirty[info.ID] = struct{}{}
		groups := r.poolRoutes[info.ID]
		if groups == nil {
			groups = make(map[string]*poolRouteAggregate)
			r.poolRoutes[info.ID] = groups
		}
		group := groups[next.provider]
		if group == nil {
			group = &poolRouteAggregate{clients: make(map[string]*ModelInfo)}
			groups[next.provider] = group
		}
		group.add(id, next.provider, info)
	}
	r.poolClients.Store(id, next)
}

// Aggregates are updated only for the registering client, under the registry lock.
type poolRouteAggregate struct {
	clients                              map[string]*ModelInfo
	supports, falseRoutes, unknownRoutes int
}

func (g *poolRouteAggregate) change(provider string, info *ModelInfo, delta int) {
	if info.SupportsWebSearch {
		g.supports += delta
	}
	v := ResolveResponsesWebSearchCapability([]NativeCapabilityRoute{{Provider: provider, NativeCapabilities: info.NativeCapabilities}})
	if v == nil {
		g.unknownRoutes += delta
	} else if !*v {
		g.falseRoutes += delta
	}
}
func (g *poolRouteAggregate) remove(id, provider string) {
	if old := g.clients[id]; old != nil {
		g.change(provider, old, -1)
		delete(g.clients, id)
	}
}
func (g *poolRouteAggregate) add(id, provider string, info *ModelInfo) {
	g.remove(id, provider)
	g.clients[id] = info
	g.change(provider, info, 1)
}
func (r *ModelRegistry) poolRegistrationUnchangedLocked(id, provider string, ids []string, infos map[string]*ModelInfo) bool {
	if _, ok := r.clientModels[id]; !ok {
		return false
	}
	if provider != r.clientProviders[id] || !slices.Equal(ids, r.clientModels[id]) || !reflect.DeepEqual(infos, r.clientModelInfos[id]) {
		return false
	}
	for _, model := range ids {
		if reg := r.models[model]; reg != nil {
			if _, ok := reg.QuotaExceededClients[id]; ok {
				return false
			}
			if _, ok := reg.SuspendedClients[id]; ok {
				return false
			}
		}
	}
	return true
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
	web := make(map[string]*bool)
	if old := r.poolDirectory.Load(); old != nil {
		view.models = maps.Clone(old.view.models)
		web = maps.Clone(old.webSearch)
	}
	ids := make([]string, 0, len(r.poolDefinitionDirty))
	for id := range r.poolDefinitionDirty {
		ids = append(ids, id)
		entry := r.models[id]
		if entry == nil {
			delete(view.models, id)
			delete(web, id)
			continue
		}
		copyEntry := *entry
		copyEntry.Info = cloneModelInfo(entry.Info)
		copyEntry.Providers = maps.Clone(entry.Providers)
		copyEntry.SuspendedClients = nil
		copyEntry.QuotaExceededClients = nil
		copyEntry.InfoByProvider = make(map[string]*ModelInfo, len(entry.InfoByProvider))
		for provider, info := range entry.InfoByProvider {
			copyEntry.InfoByProvider[provider] = cloneModelInfo(info)
		}
		view.models[id] = &copyEntry
		total, unsupported, unknown := 0, 0, 0
		for _, group := range r.poolRoutes[id] {
			total += len(group.clients)
			unsupported += group.falseRoutes
			unknown += group.unknownRoutes
		}
		delete(web, id)
		if unsupported > 0 {
			web[id] = boolPointer(false)
		} else if total > 0 && unknown == 0 {
			web[id] = boolPointer(true)
		}
	}
	// Catalog enumeration depends on the number of distinct models/providers, never accounts.
	providers := make(map[string][]*ModelInfo)
	for _, groups := range r.poolRoutes {
		for provider, group := range groups {
			for _, info := range group.clients {
				providers[provider] = append(providers[provider], cloneModelInfo(info))
				break
			}
		}
	}
	r.publishPoolAvailabilityLocked(ids)
	r.poolDefinitionDirty = nil
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

// Cache formatted model lists independently of the registry lock. Expiration is
// bounded by the earliest quota deadline, even when no writer changes generation.
type poolAvailableModelsCache struct {
	directory  *poolRegistrySnapshot
	generation uint64
	expiresAt  time.Time
	models     []map[string]any
}

func (r *ModelRegistry) poolAvailableModels(handler string, now time.Time) []map[string]any {
	snap := r.poolDirectory.Load()
	generation := r.poolGeneration.Load()
	if value, ok := r.poolAvailableCache.Load(handler); ok {
		entry := value.(*poolAvailableModelsCache)
		if entry.directory == snap && entry.generation == generation && (entry.expiresAt.IsZero() || now.Before(entry.expiresAt)) {
			return cloneModelMaps(entry.models)
		}
	}
	entry := &poolAvailableModelsCache{directory: snap, generation: generation, models: make([]map[string]any, 0, len(snap.view.models))}
	for id, model := range snap.view.models {
		availability := r.poolModelAvailability(id)
		if availability.available(now) {
			if out := r.convertModelToMap(model.Info, handler); out != nil {
				entry.models = append(entry.models, out)
			}
		}
		next := sort.Search(len(availability.deadlines), func(i int) bool { return availability.deadlines[i].After(now) })
		if next < len(availability.deadlines) {
			at := availability.deadlines[next]
			if entry.expiresAt.IsZero() || at.Before(entry.expiresAt) {
				entry.expiresAt = at
			}
		}
	}
	if r.poolGeneration.Load() == generation && r.poolDirectory.Load() == snap {
		r.poolAvailableCache.Store(handler, entry)
	}
	return cloneModelMaps(entry.models)
}
