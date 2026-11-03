package auth

import (
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCodexPoolSnapshotSeparatesInvalidAccounts(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Unix(1800000000, 0)
	m.rateCap.nowFunc = func() time.Time { return now }
	expired := map[string]any{"access_token": "tok", "expired": now.Add(-time.Hour).Format(time.RFC3339)}
	unauthorized := &Error{Code: "unauthorized", HTTPStatus: 401}
	m.auths["ok"] = &Auth{ID: "ok", Provider: "codex", Status: StatusActive}
	m.auths["off"] = &Auth{ID: "off", Provider: "codex", Status: StatusDisabled, Disabled: true}
	m.auths["invalidated"] = &Auth{ID: "invalidated", Provider: "codex", Status: StatusError, AuthFailures: codexAuthFailureThreshold}
	m.auths["invalidated-expired"] = &Auth{ID: "invalidated-expired", Provider: "codex", Status: StatusError, AuthFailures: codexAuthFailureThreshold, Metadata: expired}
	m.auths["expired"] = &Auth{ID: "expired", Provider: "codex", Status: StatusActive, Metadata: expired}
	m.auths["401-expired"] = &Auth{ID: "401-expired", Provider: "codex", Status: StatusError, Unavailable: true, LastError: unauthorized, Metadata: expired}
	m.auths["401"] = &Auth{ID: "401", Provider: "codex", Status: StatusError, Unavailable: true, LastError: unauthorized}
	m.auths["quota"] = &Auth{ID: "quota", Provider: "codex", Status: StatusActive, Unavailable: true, NextRetryAfter: now.Add(time.Hour),
		Quota: QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour)}}

	s := m.CodexPoolSnapshot()
	// The credential check runs before expiry; plain expiry (also with a 401 below the
	// invalidation threshold) stays expired; a 401 without a recovery time is invalid.
	if s.Schedulable != 1 || s.Disabled != 1 || s.Invalid != 3 || s.Expired != 2 || s.Cooling != 1 {
		t.Fatalf("snapshot: schedulable=%d disabled=%d invalid=%d expired=%d cooling=%d", s.Schedulable, s.Disabled, s.Invalid, s.Expired, s.Cooling)
	}
	if total := s.Schedulable + s.Disabled + s.Invalid + s.Expired + s.Cooling; total != len(m.auths) {
		t.Fatalf("classes sum to %d, want %d", total, len(m.auths))
	}
	if s.CoolingReasons["credential_quota"] != 1 || len(s.CoolingReasons) != 1 {
		t.Fatalf("cooling reasons: %v", s.CoolingReasons)
	}
	if s.Capacity != s.Schedulable*s.Config.InitialPerMinute {
		t.Fatalf("invalid accounts must not add capacity: capacity=%d", s.Capacity)
	}
}

func TestRateCapRecentCountsRejectedWithoutChangingState(t *testing.T) {
	r := newRequestRateCap()
	now := time.Unix(1800000000, 0)
	r.nowFunc = func() time.Time { return now }
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.getLocked("a", now)
	a.attempts = []rateAttempt{
		{id: 1, at: now.Add(-90 * time.Second)},
		{id: 2, at: now.Add(-30 * time.Second), rejected: true},
		{id: 3, at: now.Add(-time.Second)},
	}
	limit, probeAt := a.limit, a.probeAt
	if got := r.recentLocked("a", now.Add(10*time.Minute)); got != 0 {
		t.Fatalf("old attempts counted: %d", got)
	}
	if got := r.recentLocked("a", now); got != 2 {
		t.Fatalf("recent = %d, want 2 (rejected upstream 429 counts, >60s does not)", got)
	}
	if len(a.attempts) != 3 || a.limit != limit || !a.probeAt.Equal(probeAt) {
		t.Fatal("recentLocked must not prune attempts or adapt the limit")
	}
	if got := r.recentLocked("missing", now); got != 0 || r.accounts["missing"] != nil {
		t.Fatalf("missing account: got %d, created=%v", got, r.accounts["missing"] != nil)
	}
	r.refundLocked("a", 3)
	if got := r.recentLocked("a", now); got != 1 {
		t.Fatalf("refunded attempt counted: %d", got)
	}
}

func TestCodexSpeedSnapshotWithUseFillsLiveBins(t *testing.T) {
	tr, _ := newTestSpeedTracker(t, func(c *internalconfig.CodexSpeedPriorityConfig) { c.FastPercent = 10 })
	ids := rankPool(tr, "use", 100)
	view := tr.view.Load()
	if len(view.binIDs) != codexSpeedBinCount {
		t.Fatalf("bin members: %d", len(view.binIDs))
	}
	for i, members := range view.binIDs {
		if len(members) != view.bins[i].Accounts {
			t.Fatalf("bin %d: %d members vs %d accounts", i, len(members), view.bins[i].Accounts)
		}
	}
	// Bin 0 holds the two fastest accounts, the last bin the two slowest.
	use := map[string]CodexAccountUse{
		ids[0]:  {Requests: 3},
		ids[1]:  {InFlight: 1},
		ids[98]: {Requests: 1},
		"gone":  {Requests: 5},
	}
	s := tr.SnapshotWithUse(use)
	first, last, mid := s.Bins[0], s.Bins[codexSpeedBinCount-1], s.Bins[10]
	if first.Used != 2 || first.Requests != 3 || first.UsedShare == nil || *first.UsedShare != 1 {
		t.Fatalf("first bin: %+v", first)
	}
	if last.Used != 1 || last.Requests != 1 || *last.UsedShare != 0.5 {
		t.Fatalf("last bin: %+v", last)
	}
	if mid.Used != 0 || mid.UsedShare == nil || *mid.UsedShare != 0 {
		t.Fatalf("idle bin: %+v", mid)
	}
	// 10 fast members, 2 of them used; 90 slow, 1 used.
	if s.Fast != 10 || s.FastUsedShare == nil || *s.FastUsedShare != 0.2 {
		t.Fatalf("fast used share: fast=%d share=%v", s.Fast, s.FastUsedShare)
	}
	if s.SlowUsedShare == nil || *s.SlowUsedShare != 1.0/90 {
		t.Fatalf("slow used share: %v", s.SlowUsedShare)
	}
	if view.bins[0].Used != 0 || view.bins[0].UsedShare != nil {
		t.Fatal("shared view bins were modified")
	}

	plain := tr.SnapshotWithUse(nil)
	if plain.FastUsedShare != nil || plain.Bins[0].UsedShare != nil || plain.Bins[0].Used != 0 {
		t.Fatalf("nil use must match Snapshot: %+v", plain.Bins[0])
	}
}

func TestCodexSpeedSnapshotWithUseReadsOneView(t *testing.T) {
	tr, _ := newTestSpeedTracker(t, nil)
	rankPool(tr, "old", 100)
	old := tr.view.Load()
	ids := rankPool(tr, "new", 40)
	use := map[string]CodexAccountUse{}
	for _, id := range ids {
		use[id] = CodexAccountUse{Requests: 1}
	}
	// A recompute replaced the view: counts, members and use must all describe the new one.
	s := tr.SnapshotWithUse(use)
	if old == tr.view.Load() {
		t.Fatal("setup: view not replaced")
	}
	accounts, used := 0, 0
	for i, b := range s.Bins {
		if b.Used > b.Accounts {
			t.Fatalf("bin %d: used %d > accounts %d", i, b.Used, b.Accounts)
		}
		accounts += b.Accounts
		used += b.Used
	}
	if s.Total != 40 || accounts != 40 || used != 40 {
		t.Fatalf("total=%d accounts=%d used=%d, want 40 each", s.Total, accounts, used)
	}
}

func TestCodexRecentUseWithoutPools(t *testing.T) {
	m := NewManager(nil, nil, nil)
	if got := m.CodexRecentUse(); got != nil {
		t.Fatalf("no pools: %v", got)
	}
	var nilManager *Manager
	if got := nilManager.CodexRecentUse(); got != nil {
		t.Fatalf("nil manager: %v", got)
	}
}
