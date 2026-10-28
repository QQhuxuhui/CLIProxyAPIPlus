package auth

import (
	"context"
	"sort"
	"time"
)

type codexSlotMove struct {
	slot           uint16
	source, target *codexAccountPool
}
type codexPoolLoad struct {
	pool    *codexAccountPool
	count   int
	slots   []uint16
	weights map[uint16]int
}

func (pools *codexPoolSet) loadsLocked() []codexPoolLoad {
	var loads []codexPoolLoad
	for _, p := range pools.table.Load().pools {
		p.lock()
		load := codexPoolLoad{pool: p, count: len(p.auths), weights: make(map[uint16]int)}
		for slot := range codexSlotCount {
			if p.owns(uint16(slot)) {
				load.slots = append(load.slots, uint16(slot))
			}
		}
		for id := range p.auths {
			load.weights[codexSlot(id)]++
		}
		p.mu.Unlock()
		loads = append(loads, load)
	}
	return loads
}
func (pools *codexPoolSet) addPoolLocked() *codexAccountPool {
	pools.nextID++
	p := newCodexAccountPool(pools.nextID)
	cfg := pools.manager.runtimeConfigSnapshot()
	p.rate.configure(cfg.ProviderRequestRateCap["codex"])
	old := pools.table.Load()
	next := *old
	next.version++
	next.pools = append(append([]*codexAccountPool(nil), old.pools...), p)
	pools.all.Store(p.id, p)
	pools.table.Store(&next)
	return p
}
func (pools *codexPoolSet) planRebalanceLocked() bool {
	cfg := pools.config.Load()
	loads := pools.loadsLocked()
	sort.Slice(loads, func(i, j int) bool { return loads[i].count > loads[j].count })
	for _, load := range loads {
		if load.count <= cfg.MaxAccounts {
			continue
		}
		unsplittable := false
		for _, weight := range load.weights {
			if weight > cfg.MaxAccounts {
				unsplittable = true
				break
			}
		}
		if unsplittable || len(load.slots) < 2 {
			continue
		}
		target := pools.addPoolLocked()
		moved := 0
		for _, slot := range load.slots {
			if moved >= load.count/2 {
				break
			}
			pools.pending = append(pools.pending, codexSlotMove{slot, load.pool, target})
			moved += load.weights[slot]
		}
		if len(pools.pending) > 0 {
			return true
		}
	}
	if len(loads) <= 1 {
		return false
	}
	for i := len(loads) - 1; i >= 0; i-- {
		source := loads[i]
		if source.count >= cfg.MinAccounts {
			continue
		}
		for j := len(loads) - 1; j >= 0; j-- {
			target := loads[j]
			if source.pool == target.pool || source.count+target.count > cfg.MaxAccounts {
				continue
			}
			for _, slot := range source.slots {
				pools.pending = append(pools.pending, codexSlotMove{slot, source.pool, target.pool})
			}
			return len(pools.pending) > 0
		}
		// Try distributing all source slots across several existing pools before
		// retaining a small pool by donation.
		slots := append([]uint16(nil), source.slots...)
		sort.Slice(slots, func(i, j int) bool { return source.weights[slots[i]] > source.weights[slots[j]] })
		counts := make(map[*codexAccountPool]int)
		for _, load := range loads {
			counts[load.pool] = load.count
		}
		var spread []codexSlotMove
		for _, slot := range slots {
			var target *codexAccountPool
			for _, load := range loads {
				if load.pool == source.pool || counts[load.pool]+source.weights[slot] > cfg.MaxAccounts {
					continue
				}
				if target == nil || counts[load.pool] < counts[target] {
					target = load.pool
				}
			}
			if target == nil {
				spread = nil
				break
			}
			counts[target] += source.weights[slot]
			spread = append(spread, codexSlotMove{slot, source.pool, target})
		}
		if len(spread) == len(slots) && len(spread) > 0 {
			pools.pending = append(pools.pending, spread...)
			return true
		}
		// If no whole-pool merge fits, donate slots from a larger neighbour. This
		// avoids oscillation and handles a small pool beside an almost-full pool.
		for _, donor := range loads {
			if donor.pool == source.pool {
				continue
			}
			for _, slot := range donor.slots {
				weight := donor.weights[slot]
				if weight > 0 && source.count+weight <= cfg.MaxAccounts && donor.count-weight >= cfg.MinAccounts {
					pools.pending = append(pools.pending, codexSlotMove{slot, donor.pool, source.pool})
					return true
				}
			}
		}
	}
	return false
}
func (pools *codexPoolSet) retireEmptyPoolsLocked() {
	table := pools.table.Load()
	active := make([]*codexAccountPool, 0, len(table.pools))
	for _, p := range table.pools {
		p.lock()
		hasSlots := false
		for _, word := range p.owned {
			if word != 0 {
				hasSlots = true
				break
			}
		}
		if !hasSlots && len(p.auths) == 0 {
			p.retired = true
			if p.retiredAt.IsZero() {
				p.retiredAt = time.Now()
			}
		} else {
			active = append(active, p)
		}
		retired := p.retired
		p.mu.Unlock()
		if retired {
			b := &p.results
			b.mu.Lock()
			if !b.stopped {
				b.stopped = true
				b.enabled = false
				if b.queue != nil {
					close(b.queue)
				}
			}
			b.mu.Unlock()
		}
	}
	if len(active) != len(table.pools) {
		next := *table
		next.version++
		next.pools = active
		pools.table.Store(&next)
	}
}
func (pools *codexPoolSet) rebalance(ctx context.Context) error {
	pools.topology.Lock()
	defer pools.topology.Unlock()
	budget := pools.config.Load().MigrationSlotsPerTick
	for budget > 0 {
		if err := nonNilContext(ctx).Err(); err != nil {
			return err
		}
		if len(pools.pending) == 0 && !pools.planRebalanceLocked() {
			break
		}
		move := pools.pending[0]
		pools.pending = pools.pending[1:]
		if pools.table.Load().owners[move.slot] != move.source {
			continue
		}
		if err := pools.moveSlotLocked(ctx, move.slot, move.target); err != nil {
			return err
		}
		budget--
		pools.retireEmptyPoolsLocked()
	}
	return nil
}
