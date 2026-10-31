package auth

import (
	"strings"
	"time"
)

// CodexQuotaObservation is a lightweight per-credential view for quota reporting.
// Signals references the stored snapshot, which is replaced rather than mutated,
// so callers must treat it as read-only.
type CodexQuotaObservation struct {
	ID         string
	Name       string
	AccountID  string
	Plan       string
	IDToken    string
	Disabled   bool
	Invalid    bool
	ObservedAt time.Time
	Signals    map[string]string
}

// CodexQuotaObservations lists codex credentials without cloning them, so a
// periodic reader does not add full-clone work under the manager lock.
func (m *Manager) CodexQuotaObservations() []CodexQuotaObservation {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	out := make([]CodexQuotaObservation, 0, len(m.auths))
	for _, a := range m.auths {
		out = appendCodexQuotaObservation(out, a)
	}
	m.mu.RUnlock()
	if pools := m.codexPools.Load(); pools != nil {
		pools.all.Range(func(_, value any) bool {
			p := value.(*codexAccountPool)
			p.lock()
			for _, a := range p.auths {
				out = appendCodexQuotaObservation(out, a)
			}
			p.mu.Unlock()
			return true
		})
	}
	return out
}

func appendCodexQuotaObservation(out []CodexQuotaObservation, a *Auth) []CodexQuotaObservation {
	if a == nil || !strings.EqualFold(strings.TrimSpace(a.Provider), "codex") {
		return out
	}
	item := CodexQuotaObservation{
		ID:         a.ID,
		Name:       strings.TrimSpace(a.FileName),
		Disabled:   a.Disabled || a.Status == StatusDisabled,
		Invalid:    codexCredentialInvalidated(a),
		ObservedAt: a.Quota.ObservedAt,
		Signals:    a.Quota.Signals,
	}
	if item.Name == "" {
		item.Name = a.ID
	}
	item.Plan = strings.TrimSpace(a.Attributes["plan_type"])
	if a.Metadata != nil {
		item.AccountID, _ = a.Metadata["account_id"].(string)
		item.IDToken, _ = a.Metadata["id_token"].(string)
		if item.Plan == "" {
			item.Plan, _ = a.Metadata["plan_type"].(string)
		}
	}
	return append(out, item)
}
