package auth

import (
	"context"
	"errors"
	"hash/fnv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

const codexSlotCount = 1024

func codexSlot(key string) uint16 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return uint16(h.Sum32() % codexSlotCount)
}

type codexBindingKey struct{ session, model string }
type codexPoolBinding struct {
	authID  string
	epoch   uint64
	expires time.Time
}
type codexAccountPool struct {
	availability   atomic.Pointer[codexAvailabilityHint]
	fullFallback   atomic.Uint64
	dirtySince     map[string]time.Time
	inflightSince  map[string]time.Time
	lockWaits      atomic.Uint64
	retiredAt      time.Time
	fastFailures   atomic.Uint64
	projectionDue  map[string]time.Time
	catalogDirty   atomic.Bool
	selectorSource Selector
	priorities     atomic.Pointer[map[string]int]
	bindingOrder   []codexBindingKey
	bindingCursor  int
	id             uint64
	mu             sync.Mutex
	owned          [codexSlotCount / 64]uint64
	retired        bool
	auths          map[string]*Auth
	epochs         map[string]uint64
	bindings       map[codexBindingKey]codexPoolBinding
	dirty          map[string]struct{}
	rate           *requestRateCap
	results        successResultBatcher
	selector       Selector
	lockWait       atomic.Int64
	fallback       atomic.Uint64
}

func (p *codexAccountPool) owns(slot uint16) bool {
	return p.owned[slot/64]&(uint64(1)<<(slot%64)) != 0
}
func (p *codexAccountPool) setOwned(slot uint16, owned bool) {
	if owned {
		p.owned[slot/64] |= uint64(1) << (slot % 64)
	} else {
		p.owned[slot/64] &= ^(uint64(1) << (slot % 64))
	}
}
func newCodexAccountPool(id uint64) *codexAccountPool {
	r := newRequestRateCap()
	r.pooled = true
	return &codexAccountPool{id: id, auths: make(map[string]*Auth), epochs: make(map[string]uint64), bindings: make(map[codexBindingKey]codexPoolBinding), dirty: make(map[string]struct{}), rate: r, projectionDue: make(map[string]time.Time), dirtySince: make(map[string]time.Time), inflightSince: make(map[string]time.Time)}
}

type codexPoolTable struct {
	version uint64
	owners  [codexSlotCount]*codexAccountPool
	pools   []*codexAccountPool
}
type codexPoolSet struct {
	projectionRetry sync.Map
	config          atomic.Pointer[internalconfig.CodexPoolsConfig]
	publishing      chan struct{}
	cooldownDirty   atomic.Bool
	pending         []codexSlotMove
	all             sync.Map
	fastFailures    atomic.Uint64
	manager         *Manager
	owners          *codexOwnerIndex
	table           atomic.Pointer[codexPoolTable]
	side            *codexAccountPool
	topology        sync.Mutex
	nextID          uint64
}

// initializeCodexPools is called before account loading. Enabling/disabling this
// storage layout on a running manager requires restart; ordinary config reloads
// continue to update the active pools without changing the layout.
func (m *Manager) initializeCodexPools(count int) {
	if m.codexPools.Load() != nil {
		return
	}
	count = max(1, min(codexSlotCount, count))
	pools := &codexPoolSet{manager: m, owners: newCodexOwnerIndex(), side: newCodexAccountPool(0), nextID: uint64(count)}
	pools.publishing = make(chan struct{}, 1)
	pools.publishing <- struct{}{}
	table := &codexPoolTable{version: 1, pools: make([]*codexAccountPool, count)}
	for i := range table.pools {
		table.pools[i] = newCodexAccountPool(uint64(i + 1))
	}
	for slot := range codexSlotCount {
		p := table.pools[slot%count]
		table.owners[slot] = p
		p.setOwned(uint16(slot), true)
	}
	pools.all.Store(uint64(0), pools.side)
	for _, p := range table.pools {
		pools.all.Store(p.id, p)
	}
	defaults := internalconfig.CodexPoolsConfig{}.WithDefaults()
	pools.config.Store(&defaults)
	pools.table.Store(table)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.auths) != 0 || len(m.authEpochs) != 0 {
		return
	}
	if m.codexPools.CompareAndSwap(nil, pools) {
		registry.GetGlobalRegistry().EnablePoolReads()
	}
}
func codexAuthOwnerKind(a *Auth) codexOwnerKind {
	if a == nil {
		return codexOwnerMissing
	}
	if a.Provider != "codex" {
		return codexOwnerLegacy
	}
	if IsConfigAPIKeyAuth(a) {
		return codexOwnerSide
	}
	return codexOwnerPooled
}

type authStorageGuard struct {
	auths   map[string]*Auth
	epochs  map[string]uint64
	pool    *codexAccountPool
	release func()
}

func (g authStorageGuard) unlock() {
	if g.release != nil {
		g.release()
	}
}
func (m *Manager) lockAuthCategory(ctx context.Context, id string, kind codexOwnerKind) (authStorageGuard, error) {
	pools := m.codexPools.Load()
	if pools == nil || kind == codexOwnerLegacy {
		m.mu.Lock()
		return authStorageGuard{auths: m.auths, epochs: m.authEpochs, release: m.mu.Unlock}, nil
	}
	if kind == codexOwnerMissing {
		return authStorageGuard{}, errCodexOwnerMissing
	}
	if ctx == nil {
		ctx = context.Background()
	}
	slot := codexSlot(id)
	for {
		if err := ctx.Err(); err != nil {
			return authStorageGuard{}, err
		}
		p := pools.side
		if kind == codexOwnerPooled {
			p = pools.table.Load().owners[slot]
		}
		p.lock()
		if kind == codexOwnerPooled && (p.retired || !p.owns(slot)) {
			p.mu.Unlock()
			continue
		}
		return authStorageGuard{auths: p.auths, epochs: p.epochs, pool: p, release: p.mu.Unlock}, nil
	}
}
func (m *Manager) lockAuth(ctx context.Context, id string) (authStorageGuard, error) {
	pools := m.codexPools.Load()
	if pools == nil {
		return m.lockAuthCategory(ctx, id, codexOwnerLegacy)
	}
	for {
		lease, err := pools.owners.resolve(ctx, id)
		if err != nil {
			return authStorageGuard{}, err
		}
		guard, err := m.lockAuthCategory(ctx, id, lease.kind)
		if err != nil {
			return authStorageGuard{}, err
		}
		if !pools.owners.valid(id, lease) {
			guard.unlock()
			continue
		}
		return guard, nil
	}
}
func (m *Manager) registerPooledStorage(ctx context.Context, a *Auth) (*Auth, error) {
	tx, err := m.codexPools.Load().owners.begin(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	defer tx.abort()
	saved, err := m.registerPooledStorageTransition(ctx, a, tx)
	if err == nil {
		m.queueRefreshReschedule(a.ID)
	}
	return saved, err
}
func (m *Manager) registerPooledStorageTransition(ctx context.Context, a *Auth, tx *codexOwnerTransition) (*Auth, error) {
	a.Provider = strings.ToLower(strings.TrimSpace(a.Provider))
	epoch := max(a.RegistrationEpoch, tx.previous.authEpoch)
	target := codexAuthOwnerKind(a)
	var previous *Auth
	var window *rateAccount
	if tx.previous.kind != codexOwnerMissing {
		old, errLock := m.lockAuthCategory(ctx, a.ID, tx.previous.kind)
		if errLock != nil {
			return nil, errLock
		}
		epoch = max(epoch, old.epochs[a.ID])
		if existing := old.auths[a.ID]; existing != nil {
			epoch = max(epoch, existing.RegistrationEpoch)
			previous = existing.Clone()
		}
		if target != tx.previous.kind {
			window = m.takeRateWindowLocked(old, a.ID)
			delete(old.auths, a.ID)
			if old.pool != nil {
				old.pool.clearProjectionLocked(a.ID)
			}
		}
		old.unlock()
	}
	// Once removed from its original category, installation must finish even if the
	// caller disconnects. This is an in-memory transition with no external I/O.
	guard, err := m.lockAuthCategory(context.WithoutCancel(nonNilContext(ctx)), a.ID, target)
	if err != nil {
		return nil, err
	}
	a.RegistrationEpoch = epoch + 1
	a.Generation = 1
	guard.epochs[a.ID] = a.RegistrationEpoch
	snapshot := a.Clone()
	if guard.pool != nil {
		guard.pool.clearProjectionLocked(a.ID)
	}
	guard.auths[a.ID] = snapshot.Clone()
	if window != nil {
		m.putRateWindowLocked(guard, a.ID, window)
	}
	m.adjustProviderPresence(previous, snapshot)
	if guard.pool != nil {
		guard.pool.markDirtyLocked(a.ID)
		guard.pool.catalogDirty.Store(true)
	}
	guard.unlock()
	registry.GetGlobalRegistry().TrackClientAuthEpoch(a.ID, a.RegistrationEpoch)
	tx.authEpoch = a.RegistrationEpoch
	tx.publish(target)
	return snapshot, nil
}
func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// moveSlot is the only path holding two pool locks. The coordinator serializes
// table publication; ordinary operations never acquire the coordinator lock.
func (pools *codexPoolSet) moveSlot(ctx context.Context, slot uint16, target *codexAccountPool) error {
	if int(slot) >= codexSlotCount || target == nil {
		return errors.New("invalid codex slot destination")
	}
	pools.topology.Lock()
	defer pools.topology.Unlock()
	return pools.moveSlotLocked(ctx, slot, target)
}
func (pools *codexPoolSet) moveSlotLocked(ctx context.Context, slot uint16, target *codexAccountPool) error {

	if err := nonNilContext(ctx).Err(); err != nil {
		return err
	}
	table := pools.table.Load()
	source := table.owners[slot]
	if source == target {
		return nil
	}
	found := false
	for _, p := range table.pools {
		if p == target {
			found = true
			break
		}
	}
	if !found {
		return errors.New("codex destination is not in current topology")
	}
	first, second := source, target
	if first.id > second.id {
		first, second = second, first
	}
	first.lock()
	second.lock()
	defer first.mu.Unlock()
	defer second.mu.Unlock()
	if target.retired {
		return errors.New("codex destination retired")
	}
	for id, a := range source.auths {
		if codexSlot(id) != slot {
			continue
		}
		target.auths[id] = a
		if since, ok := source.dirtySince[id]; ok {
			target.dirtySince[id] = since
			delete(source.dirtySince, id)
		}
		if since, ok := source.inflightSince[id]; ok {
			target.inflightSince[id] = since
			delete(source.inflightSince, id)
		}
		if due, ok := source.projectionDue[id]; ok {
			target.projectionDue[id] = due
			delete(source.projectionDue, id)
		}
		delete(source.auths, id)
		if window := source.rate.accounts[id]; window != nil {
			target.rate.accounts[id] = window
			delete(source.rate.accounts, id)
		}
		if _, dirty := source.dirty[id]; dirty {
			target.markDirtyAtLocked(id, target.dirtySince[id])
			delete(source.dirty, id)
		}
	}
	for id, epoch := range source.epochs {
		if codexSlot(id) == slot {
			target.epochs[id] = max(target.epochs[id], epoch)
			delete(source.epochs, id)
		}
	}
	for key, binding := range source.bindings {
		if codexSlot(key.session) == slot {
			target.putBindingLocked(key, binding)
			delete(source.bindings, key)
		}
	}
	source.setOwned(slot, false)
	target.setOwned(slot, true)
	source.availability.Store(nil)
	target.availability.Store(nil)
	source.catalogDirty.Store(true)
	target.catalogDirty.Store(true)
	next := *table
	next.version++
	next.owners[slot] = target
	pools.table.Store(&next)
	return nil
}
func (m *Manager) pooledAuthSnapshots() []*Auth {
	pools := m.codexPools.Load()
	if pools == nil {
		return nil
	}
	table := pools.table.Load()
	var out []*Auth
	for _, p := range append(append([]*codexAccountPool{}, table.pools...), pools.side) {
		p.lock()
		for _, a := range p.auths {
			out = append(out, a.Clone())
		}
		p.mu.Unlock()
	}
	return out
}

func (m *Manager) removePooledStorage(ctx context.Context, id string, tx *codexOwnerTransition) (*Auth, uint64, error) {
	if tx.previous.kind == codexOwnerMissing {
		tx.publish(codexOwnerMissing)
		return nil, 0, nil
	}
	guard, err := m.lockAuthCategory(ctx, id, tx.previous.kind)
	if err != nil {
		return nil, 0, err
	}
	a := guard.auths[id]
	if a == nil {
		guard.unlock()
		tx.publish(codexOwnerMissing)
		return nil, 0, nil
	}
	snapshot := a.Clone()
	epoch := max(guard.epochs[id], a.RegistrationEpoch, tx.previous.authEpoch) + 1
	tx.authEpoch = epoch
	guard.epochs[id] = epoch
	delete(guard.auths, id)
	m.adjustProviderPresence(snapshot, nil)
	if guard.pool != nil {
		delete(guard.pool.rate.accounts, id)
		guard.pool.clearProjectionLocked(id)
	}
	guard.unlock()
	registry.GetGlobalRegistry().TrackClientAuthEpoch(id, epoch)
	tx.publish(codexOwnerMissing)
	return snapshot, epoch, nil
}
func (m *Manager) loadPooledStorage(ctx context.Context) error {
	store := m.shared.Load().store
	if store == nil {
		return nil
	}
	items, err := store.List(ctx)
	if err != nil {
		return err
	}
	pooledCount := 0
	for _, a := range items {
		if a != nil && codexAuthOwnerKind(a) == codexOwnerPooled {
			pooledCount++
		}
	}
	target := max(1, m.codexPools.Load().config.Load().MaxAccounts/2)
	m.codexPools.Load().seedEmptyTopology((pooledCount + target - 1) / target)
	previous := m.snapshotAuths()
	live := make(map[string]bool, len(items))
	for _, a := range items {
		if a == nil || a.ID == "" {
			continue
		}
		NormalizeCredentialMetadata(a.Metadata)
		if ValidateAuthWeight(a) != nil {
			continue
		}
		a.EnsureIndex()
		if _, err := m.registerPooledStorage(ctx, a); err != nil {
			return err
		}
		live[a.ID] = true
	}
	for _, a := range previous {
		if !live[a.ID] {
			m.Remove(ctx, a.ID)
		}
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	m.syncScheduler()
	return nil
}

func (m *Manager) markPoolProjectionDirty(id string) {
	guard, err := m.lockAuth(context.Background(), id)
	if err != nil {
		return
	}
	if guard.pool != nil {
		guard.pool.markDirtyLocked(id)
		guard.pool.catalogDirty.Store(true)
	}
	guard.unlock()
}

func (p *codexAccountPool) lock() {
	if p.mu.TryLock() {
		return
	}
	started := time.Now()
	p.mu.Lock()
	p.lockWait.Add(int64(time.Since(started)))
	p.lockWaits.Add(1)
}

// Initial file loading knows the population before any pooled account is visible.
func (pools *codexPoolSet) seedEmptyTopology(count int) {
	if count <= 1 {
		return
	}
	pools.topology.Lock()
	defer pools.topology.Unlock()
	old := pools.table.Load()
	for _, p := range old.pools {
		p.lock()
		empty := len(p.auths) == 0 && len(p.epochs) == 0 && len(p.bindings) == 0
		p.mu.Unlock()
		if !empty {
			return
		}
	}
	next := &codexPoolTable{version: old.version + 1, pools: make([]*codexAccountPool, min(count, codexSlotCount))}
	for i := range next.pools {
		pools.nextID++
		p := newCodexAccountPool(pools.nextID)
		p.rate.configure(pools.manager.runtimeConfigSnapshot().ProviderRequestRateCap["codex"])
		next.pools[i] = p
		pools.all.Store(p.id, p)
	}
	for slot := range codexSlotCount {
		p := next.pools[slot%len(next.pools)]
		p.setOwned(uint16(slot), true)
		next.owners[slot] = p
	}
	for _, p := range old.pools {
		p.lock()
		p.retired = true
		p.retiredAt = time.Now()
		p.owned = [codexSlotCount / 64]uint64{}
		p.mu.Unlock()
	}
	pools.table.Store(next)
}

func (m *Manager) takeRateWindowLocked(guard authStorageGuard, id string) *rateAccount {
	if guard.pool != nil {
		window := guard.pool.rate.accounts[id]
		delete(guard.pool.rate.accounts, id)
		delete(guard.pool.projectionDue, id)
		return window
	}
	m.rateCap.mu.Lock()
	defer m.rateCap.mu.Unlock()
	window := m.rateCap.accounts[id]
	delete(m.rateCap.accounts, id)
	return window
}
func (m *Manager) putRateWindowLocked(guard authStorageGuard, id string, window *rateAccount) {
	if guard.pool != nil {
		guard.pool.rate.accounts[id] = window
		return
	}
	m.rateCap.mu.Lock()
	m.rateCap.accounts[id] = window
	m.rateCap.mu.Unlock()
}

// Full-state replacement stores must not interpret a migration-time omission as
// a deletion. Retry after any topology or lifecycle publication, then deduplicate.
func (m *Manager) persistenceAuthSnapshot(ctx context.Context) ([]*Auth, error) {
	pools := m.codexPools.Load()
	if pools == nil {
		return m.snapshotAuths(), nil
	}
	for {
		if err := nonNilContext(ctx).Err(); err != nil {
			return nil, err
		}
		topology := pools.table.Load()
		owners := pools.owners.snapshot.Load()
		snapshots := m.snapshotAuths()
		if pools.table.Load() != topology || pools.owners.snapshot.Load() != owners {
			continue
		}
		moving := false
		for id, entry := range owners.entries {
			if entry.moving {
				if _, err := pools.owners.resolve(ctx, id); err != nil && nonNilContext(ctx).Err() != nil {
					return nil, ctx.Err()
				}
				moving = true
				break
			}
		}
		if moving {
			continue
		}
		byID := make(map[string]*Auth, len(snapshots))
		for _, a := range snapshots {
			old := byID[a.ID]
			if old == nil || a.RegistrationEpoch > old.RegistrationEpoch || (a.RegistrationEpoch == old.RegistrationEpoch && a.Generation > old.Generation) {
				byID[a.ID] = a
			}
		}
		out := make([]*Auth, 0, len(byID))
		for _, a := range byID {
			out = append(out, a)
		}
		return out, nil
	}
}

// Read guards preserve the legacy RWMutex read path while consulting the owner
// index first, including deletion and cross-category migration races.
func (m *Manager) lockAuthRead(ctx context.Context, id string) (authStorageGuard, error) {
	pools := m.codexPools.Load()
	if pools == nil {
		m.mu.RLock()
		return authStorageGuard{auths: m.auths, epochs: m.authEpochs, release: m.mu.RUnlock}, nil
	}
	for {
		lease, err := pools.owners.resolve(ctx, id)
		if err != nil {
			return authStorageGuard{}, err
		}
		var guard authStorageGuard
		if lease.kind == codexOwnerLegacy {
			m.mu.RLock()
			guard = authStorageGuard{auths: m.auths, epochs: m.authEpochs, release: m.mu.RUnlock}
		} else {
			guard, err = m.lockAuthCategory(ctx, id, lease.kind)
			if err != nil {
				return authStorageGuard{}, err
			}
		}
		if !pools.owners.valid(id, lease) {
			guard.unlock()
			continue
		}
		return guard, nil
	}
}
