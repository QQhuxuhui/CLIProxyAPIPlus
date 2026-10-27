package auth

import (
	"context"
	"time"
)

type codexAvailabilityHint struct {
	potential int
	until     time.Time
	capacity  bool
}

// Model-specific failures and unknown recovery conditions stay conservative: a
// hint may cause an extra scan, but must never hide a usable credential.
func (p *codexAccountPool) refreshAvailabilityHintLocked() {
	now := p.rate.nowFunc()
	hint := &codexAvailabilityHint{}
	for _, a := range p.auths {
		if a.Disabled || a.Status == StatusDisabled || hasUnauthorizedAuthFailure(a) {
			hint.potential++
			continue
		}
		if expiry, ok := a.AccessTokenExpirationTime(); ok && !expiry.After(now) {
			hint.potential++
			continue
		}
		var next time.Time
		if a.Quota.Exceeded && a.Quota.Reason == "credential_quota" && a.Quota.NextRecoverAt.After(now) {
			next = a.Quota.NextRecoverAt
		}
		if p.rate.config.Enabled && p.rate.config.FailFast {
			count, limit, release := p.rate.accountLocked(a.ID, now)
			if count >= limit && release.After(next) {
				next = release
			}
			hint.capacity = true
		}
		if next.IsZero() {
			hint.potential++
			continue
		}
		if hint.until.IsZero() || next.Before(hint.until) {
			hint.until = next
		}
	}
	p.availability.Store(hint)
}
func (p *codexAccountPool) markDirtyLocked(id string) { p.markDirtyAtLocked(id, time.Now()) }
func (p *codexAccountPool) markDirtyAtLocked(id string, at time.Time) {
	if at.IsZero() {
		at = time.Now()
	}
	if _, ok := p.dirty[id]; !ok {
		p.dirtySince[id] = at
	} else if previous := p.dirtySince[id]; previous.IsZero() || at.Before(previous) {
		p.dirtySince[id] = at
	}
	p.dirty[id] = struct{}{}
	p.availability.Store(nil)
}
func (p *codexAccountPool) clearProjectionLocked(id string) {
	delete(p.dirty, id)
	delete(p.dirtySince, id)
	delete(p.inflightSince, id)
	delete(p.projectionDue, id)
	p.availability.Store(nil)
}

type codexProjectionRetry struct {
	epoch uint64
	since time.Time
}

func (pools *codexPoolSet) queueProjectionRetry(id string, epoch uint64, since time.Time) {
	if previous, ok := pools.projectionRetry.Load(id); ok {
		old := previous.(codexProjectionRetry)
		if old.epoch > epoch {
			return
		}
		if old.epoch == epoch && (since.IsZero() || old.since.Before(since)) {
			since = old.since
		}
	}
	pools.projectionRetry.Store(id, codexProjectionRetry{epoch: epoch, since: since})
}
func (m *Manager) projectionCompleted(ctx context.Context, id string, epoch uint64) {
	guard, err := m.lockAuth(ctx, id)
	if err != nil {
		return
	}
	defer guard.unlock()
	if a := guard.auths[id]; guard.pool != nil && a != nil && a.RegistrationEpoch == epoch {
		delete(guard.pool.inflightSince, id)
	}
}
