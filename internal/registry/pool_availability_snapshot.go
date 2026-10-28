package registry

import (
	"sort"
	"strings"
	"time"
)

type poolAvailability struct {
	count, suspended, otherSuspended, quotaSuspended int
	deadlines                                        []time.Time
	providers                                        map[string]*poolAvailability
}

func (a *poolAvailability) activeQuota(now time.Time) int {
	return len(a.deadlines) - sort.Search(len(a.deadlines), func(i int) bool { return a.deadlines[i].After(now) })
}
func (a *poolAvailability) modelCount(now time.Time) int {
	return max(0, a.count-a.suspended-a.activeQuota(now))
}
func (a *poolAvailability) available(now time.Time) bool {
	active := a.activeQuota(now)
	return a.count-active-a.otherSuspended > 0 || (a.count > 0 && (active > 0 || a.quotaSuspended > 0) && a.otherSuspended == 0)
}
func (r *ModelRegistry) publishPoolAvailabilityLocked(ids []string) {
	if !r.poolReads.Load() {
		return
	}
	if r.poolAvailabilityBatch > 0 {
		if r.poolAvailabilityDirty == nil {
			r.poolAvailabilityDirty = make(map[string]struct{})
		}
		for _, id := range ids {
			id = strings.TrimSpace(id)
			r.poolAvailabilityDirty[id] = struct{}{}
		}
		return
	}
	for _, id := range ids {
		reg := r.models[id]
		if reg == nil {
			r.poolAvailability.Delete(id)
			continue
		}
		a := &poolAvailability{count: reg.Count, providers: make(map[string]*poolAvailability)}
		for p, count := range reg.Providers {
			a.providers[p] = &poolAvailability{count: count}
		}
		for client, deadline := range reg.QuotaExceededClients {
			if deadline == nil {
				continue
			}
			a.deadlines = append(a.deadlines, *deadline)
			if p := a.providers[r.clientProviders[client]]; p != nil {
				p.deadlines = append(p.deadlines, *deadline)
			}
		}
		for client, reason := range reg.SuspendedClients {
			a.suspended++
			if strings.EqualFold(reason, "quota") {
				a.quotaSuspended++
			} else {
				a.otherSuspended++
			}
			if p := a.providers[r.clientProviders[client]]; p != nil {
				p.suspended++
				if strings.EqualFold(reason, "quota") {
					p.quotaSuspended++
				} else {
					p.otherSuspended++
				}
			}
		}
		sort.Slice(a.deadlines, func(i, j int) bool { return a.deadlines[i].Before(a.deadlines[j]) })
		for _, p := range a.providers {
			sort.Slice(p.deadlines, func(i, j int) bool { return p.deadlines[i].Before(p.deadlines[j]) })
		}
		r.poolAvailability.Store(id, a)
	}
	if len(ids) > 0 {
		r.poolGeneration.Add(1)
	}
}
func (r *ModelRegistry) poolModelAvailability(id string) *poolAvailability {
	if value, ok := r.poolAvailability.Load(id); ok {
		return value.(*poolAvailability)
	}
	return &poolAvailability{}
}
func (r *ModelRegistry) flushPoolAvailabilityLocked() {
	ids := make([]string, 0, len(r.poolAvailabilityDirty))
	for id := range r.poolAvailabilityDirty {
		ids = append(ids, id)
	}
	r.poolAvailabilityDirty = nil
	r.publishPoolAvailabilityLocked(ids)
}
func (r *ModelRegistry) publishPoolDefinitionLocked() {
	if !r.poolReads.Load() {
		return
	}
	r.poolReadDirty = true
	r.publishPoolDirectoryLocked()
}
