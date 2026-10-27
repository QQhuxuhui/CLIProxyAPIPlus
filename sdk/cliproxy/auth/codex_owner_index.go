package auth

import (
	"context"
	"errors"
	"maps"
	"sync"
	"sync/atomic"
)

// The category index is separate from slot routing: moving a slot never rewrites
// this map. A pooled owner must additionally validate its slot under the pool lock.
type codexOwnerKind uint8

const (
	codexOwnerMissing codexOwnerKind = iota
	codexOwnerPooled
	codexOwnerSide
	codexOwnerLegacy
)

var errCodexOwnerMissing = errors.New("credential ownership is not registered")

type codexOwnerLease struct {
	kind    codexOwnerKind
	version uint64
}
type codexOwnerEntry struct {
	lease  codexOwnerLease
	moving bool
	done   <-chan struct{}
}
type codexOwnerSnapshot struct{ entries map[string]codexOwnerEntry }

// codexOwnerIndex publishes immutable category snapshots. The publication lock
// is never held while waiting for a lifecycle gate, a container, or a reader.
// Tombstones and lifecycle gates survive deletion to prevent same-ID ABA races.
type codexOwnerIndex struct {
	publication sync.Mutex
	snapshot    atomic.Pointer[codexOwnerSnapshot]
	lifecycle   sync.Map // auth ID -> buffered channel acting as a cancellable gate
}

func newCodexOwnerIndex() *codexOwnerIndex {
	index := &codexOwnerIndex{}
	index.snapshot.Store(&codexOwnerSnapshot{entries: make(map[string]codexOwnerEntry)})
	return index
}
func (index *codexOwnerIndex) entry(id string) codexOwnerEntry {
	snapshot := index.snapshot.Load()
	if snapshot == nil {
		return codexOwnerEntry{}
	}
	return snapshot.entries[id]
}
func (index *codexOwnerIndex) publish(id string, entry codexOwnerEntry) {
	index.publication.Lock()
	defer index.publication.Unlock()
	entries := make(map[string]codexOwnerEntry)
	if current := index.snapshot.Load(); current != nil {
		entries = maps.Clone(current.entries)
	}
	entries[id] = entry
	index.snapshot.Store(&codexOwnerSnapshot{entries: entries})
}
func (index *codexOwnerIndex) gate(id string) chan struct{} {
	if existing, ok := index.lifecycle.Load(id); ok {
		return existing.(chan struct{})
	}
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	actual, _ := index.lifecycle.LoadOrStore(id, gate)
	return actual.(chan struct{})
}

// begin serializes the complete lifecycle operation for this ID. Its caller must
// commit after installing the target state, or restore the original storage and
// abort. No publication lock is held when begin returns.
func (index *codexOwnerIndex) begin(ctx context.Context, id string) (*codexOwnerTransition, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, errCodexOwnerMissing
	}
	gate := index.gate(id)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-gate:
	}
	if err := ctx.Err(); err != nil {
		gate <- struct{}{}
		return nil, err
	}
	previous := index.entry(id)
	done := make(chan struct{})
	tx := &codexOwnerTransition{index: index, id: id, previous: previous.lease, version: previous.lease.version + 1, done: done, gate: gate}
	index.publish(id, codexOwnerEntry{lease: codexOwnerLease{kind: previous.lease.kind, version: tx.version}, moving: true, done: done})
	return tx, nil
}

// resolve waits without holding any lock. Call valid after acquiring the resolved
// container lock, then validate slot ownership separately for pooled credentials.
func (index *codexOwnerIndex) resolve(ctx context.Context, id string) (codexOwnerLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if err := ctx.Err(); err != nil {
			return codexOwnerLease{}, err
		}
		current := index.entry(id)
		if current.moving {
			select {
			case <-ctx.Done():
				return codexOwnerLease{}, ctx.Err()
			case <-current.done:
			}
			continue
		}
		if current.lease.kind == codexOwnerMissing {
			return codexOwnerLease{}, errCodexOwnerMissing
		}
		return current.lease, nil
	}
}
func (index *codexOwnerIndex) valid(id string, lease codexOwnerLease) bool {
	current := index.entry(id)
	return !current.moving && lease.kind != codexOwnerMissing && current.lease == lease
}

type codexOwnerTransition struct {
	index    *codexOwnerIndex
	id       string
	previous codexOwnerLease
	version  uint64
	done     chan struct{}
	gate     chan struct{}
	finished atomic.Bool
}

// commit publishes a stable destination before waking readers. A missing
// destination is a versioned deletion tombstone, not removal of the index entry.
func (tx *codexOwnerTransition) commit(kind codexOwnerKind) bool {
	if !tx.finished.CompareAndSwap(false, true) {
		return false
	}
	tx.index.publish(tx.id, codexOwnerEntry{lease: codexOwnerLease{kind: kind, version: tx.version + 1}})
	close(tx.done)
	tx.gate <- struct{}{}
	return true
}

// abort is idempotent. Restore any moved container data before invoking it.
func (tx *codexOwnerTransition) abort() { tx.commit(tx.previous.kind) }
