package auth

import "time"

// codexRecentUseWindow is how far back an attempt counts as recent use.
const codexRecentUseWindow = time.Minute

// CodexAccountUse is one account's live use: attempts dispatched in the last minute (including
// upstream-429'd ones and speed probes) and requests currently in flight.
type CodexAccountUse struct {
	Requests int
	InFlight int
}

func (u CodexAccountUse) used() bool { return u.Requests > 0 || u.InFlight > 0 }

func (u CodexAccountUse) share() float64 {
	if u.used() {
		return 1
	}
	return 0
}

// recentLocked counts the account's attempts newer than now-window without touching the account
// state: unlike getLocked it neither prunes nor adapts the limit, so reading never changes
// scheduling. Rejected attempts reached upstream (429) and count; refunded ones are already gone.
func (r *requestRateCap) recentLocked(id string, now time.Time) int {
	a := r.accounts[id]
	if a == nil {
		return 0
	}
	from := now.Add(-codexRecentUseWindow)
	n := 0
	for _, p := range a.attempts {
		if p.at.After(from) {
			n++
		}
	}
	return n
}

// CodexRecentUse returns live use per pooled Codex account, or nil without pools or in Home mode.
// The concurrency snapshot is taken before any pool lock, and each pool is locked only while
// its accounts are read.
func (m *Manager) CodexRecentUse() map[string]CodexAccountUse {
	if m == nil || m.HomeEnabled() {
		return nil
	}
	pools := m.codexPools.Load()
	if pools == nil {
		return nil
	}
	concurrency := m.CodexAccountConcurrencyView()
	out := make(map[string]CodexAccountUse)
	for _, p := range pools.table.Load().pools {
		p.lock()
		now := time.Now()
		if p.rate != nil {
			now = p.rate.nowFunc()
		}
		for id, a := range p.auths {
			var u CodexAccountUse
			if p.rate != nil {
				u.Requests = p.rate.recentLocked(id, now)
			}
			if entry, ok := concurrency.Entry(a); ok {
				u.InFlight = entry.InFlight
			}
			if u.used() {
				out[id] = u
			}
		}
		p.mu.Unlock()
	}
	return out
}
