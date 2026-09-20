package auth

import (
	"maps"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type schedulerCandidateEntry struct {
	epoch      uint64
	generation uint64
	candidate  pluginapi.SchedulerAuthCandidate
}

const maxSchedulerCandidateCacheEntries = 4096

// cachedSchedulerCandidates reuses candidate projections keyed by registration
// epoch and generation. Invariant: every code path that changes an auth's Status,
// Attributes or priority must bump Auth.Generation, otherwise plugins see a stale
// projection.
func (m *Manager) cachedSchedulerCandidates(auths []*Auth) []pluginapi.SchedulerAuthCandidate {
	if len(auths) == 0 {
		return nil
	}
	m.schedulerCandidateMu.Lock()
	defer m.schedulerCandidateMu.Unlock()
	if m.schedulerCandidates == nil {
		m.schedulerCandidates = make(map[string]schedulerCandidateEntry)
	}
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		entry, exists := m.schedulerCandidates[auth.ID]
		cacheable := auth.ID != "" && auth.Generation != 0 && auth.RegistrationEpoch != 0
		if cacheable && exists && entry.epoch == auth.RegistrationEpoch && entry.generation == auth.Generation {
			out = append(out, entry.candidate)
			continue
		}
		candidate := pluginapi.SchedulerAuthCandidate{
			ID:         auth.ID,
			Provider:   strings.ToLower(strings.TrimSpace(auth.Provider)),
			Priority:   authPriority(auth),
			Status:     string(auth.Status),
			Attributes: schedulerSafeAttributes(auth.Attributes),
		}
		out = append(out, candidate)
		// Late snapshots may still be selected by their caller, but must not
		// replace a newer cached credential projection.
		if cacheable && (!exists || auth.RegistrationEpoch > entry.epoch || (auth.RegistrationEpoch == entry.epoch && auth.Generation > entry.generation)) {
			if !exists && len(m.schedulerCandidates) >= maxSchedulerCandidateCacheEntries {
				continue
			}
			m.schedulerCandidates[auth.ID] = schedulerCandidateEntry{epoch: auth.RegistrationEpoch, generation: auth.Generation, candidate: candidate}
		}
	}
	return out
}

func cloneSchedulerAttributes(attributes map[string]string) map[string]string {
	return maps.Clone(attributes)
}
