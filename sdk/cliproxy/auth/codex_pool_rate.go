package auth

import (
	"context"
	"sync/atomic"
	"time"

	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

var codexPoolRateSequence atomic.Uint64

func (m *Manager) beginPooledRateAttempt(ctx context.Context, a *Auth) (context.Context, error) {
	guard, err := m.lockAuth(ctx, a.ID)
	if err != nil {
		return ctx, err
	}
	defer guard.unlock()
	current := guard.auths[a.ID]
	if guard.pool == nil || current == nil || current.RegistrationEpoch != a.RegistrationEpoch {
		return ctx, &Error{Code: "auth_unavailable", Message: staleSelectionMessage, Retryable: true}
	}
	r := guard.pool.rate
	now := r.nowFunc()
	count, limit, next := r.accountLocked(a.ID, now)
	if r.config.Enabled && r.config.FailFast && count >= limit {
		return ctx, newPoolCapacityError(next.Sub(now))
	}
	token := r.startLocked(a.ID, now)
	guard.pool.availability.Store(nil)
	ref := &rateAttemptRef{authID: a.ID, epoch: a.RegistrationEpoch}
	ref.token.Store(token)
	ctx = context.WithValue(ctx, rateAttemptContextKey{}, ref)
	ctx = exec.WithUpstreamRequestRetryObserver(ctx, func() {
		g, err := m.lockAuth(context.WithoutCancel(nonNilContext(ctx)), ref.authID)
		if err != nil {
			return
		}
		defer g.unlock()
		current := g.auths[ref.authID]
		if g.pool != nil && current != nil && current.RegistrationEpoch == ref.epoch {
			ref.token.Store(g.pool.rate.startLocked(ref.authID, g.pool.rate.nowFunc()))
			g.pool.availability.Store(nil)
		}
	})
	return ctx, nil
}
func (m *Manager) observePooledRateResult(ctx context.Context, result Result) {
	ref, ok := ctx.Value(rateAttemptContextKey{}).(*rateAttemptRef)
	if !ok || ref.authID != result.AuthID {
		return
	}
	guard, err := m.lockAuth(context.WithoutCancel(ctx), ref.authID)
	if err != nil {
		return
	}
	defer guard.unlock()
	if a := guard.auths[ref.authID]; guard.pool != nil && a != nil && (ref.epoch == 0 || a.RegistrationEpoch == ref.epoch) {
		guard.pool.rate.rejectLocked(ref.authID, ref.token.Load())
		guard.pool.availability.Store(nil)
	}
}
func (m *Manager) finishPooledRateDispatch(ctx context.Context, ref *rateAttemptRef) {
	guard, err := m.lockAuth(context.WithoutCancel(ctx), ref.authID)
	if err != nil {
		return
	}
	defer guard.unlock()
	if a := guard.auths[ref.authID]; guard.pool != nil && a != nil && (ref.epoch == 0 || a.RegistrationEpoch == ref.epoch) {
		guard.pool.rate.refundLocked(ref.authID, ref.token.Load())
		guard.pool.availability.Store(nil)
	}
}
func (m *Manager) pooledRateAvailableLocked(p *codexAccountPool, a *Auth, ctx context.Context) (bool, time.Time) {
	if allow, _ := ctx.Value(poolAllowSaturatedKey{}).(bool); allow && !p.rate.config.FailFast {
		return true, time.Time{}
	}
	if rateBypassed(ctx) || !p.rate.config.Enabled {
		return true, time.Time{}
	}
	count, limit, next := p.rate.accountLocked(a.ID, p.rate.nowFunc())
	return count < limit, next
}
