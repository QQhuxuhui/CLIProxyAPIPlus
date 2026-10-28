package auth

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestCodexPoolMetricsWindowAndHistogram(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Unix(1800000000, 0)
	m.rateCap.nowFunc = func() time.Time { return now }
	m.auths["a"] = &Auth{ID: "a", Provider: "codex", Status: StatusActive}
	m.rateCap.start("a")
	for _, r := range []usage.Record{
		{Provider: "codex", TTFT: 6100 * time.Millisecond},
		{Provider: "codex", TTFT: 9 * time.Second},
		{Provider: "codex", Failed: true, Fail: usage.Failure{StatusCode: 502}},
		{Provider: "codex", Failed: true, Fail: usage.Failure{StatusCode: 429}},
		{Provider: "codex", Failed: true, Fail: usage.Failure{StatusCode: 429, Body: `{"error":{"type":"usage_limit_reached"}}`}},
		{Provider: "codex", Failed: true, Fail: usage.Failure{StatusCode: 400}},
		{Provider: "claude", TTFT: time.Second},
		{Provider: "codex", AdditionalModel: true, TTFT: time.Second},
	} {
		m.rateCap.HandleUsage(context.Background(), r)
	}
	snapshot := m.CodexPoolSnapshot()
	if snapshot.RPM != 1 || snapshot.Capacity != 20 || snapshot.Remaining != 19 || snapshot.Schedulable != 1 {
		t.Fatalf("capacity: %+v", snapshot)
	}
	if snapshot.TTFTP50 != 7 || snapshot.TTFTP90 != 10 || snapshot.SuccessRate != float64(2)/6 {
		t.Fatalf("outcomes: %+v", snapshot)
	}
	if len(snapshot.Points) != 60 {
		t.Fatalf("points=%d", len(snapshot.Points))
	}
	last := snapshot.Points[59]
	if last.Success != 2 || last.Overload != 1 || last.RateLimits != 1 || last.QuotaExhausted != 1 || last.OtherFailures != 1 {
		t.Fatalf("buckets: %+v", last)
	}
	now = now.Add(time.Minute)
	if snapshot = m.CodexPoolSnapshot(); snapshot.RPM != 0 {
		t.Fatalf("expired RPM=%d", snapshot.RPM)
	}
	now = now.Add(5 * time.Minute)
	if snapshot = m.CodexPoolSnapshot(); snapshot.TTFTP50 != 0 || snapshot.SuccessRate != 0 {
		t.Fatal("five-minute outcome window did not expire")
	}
	now = now.Add(time.Hour)
	snapshot = m.CodexPoolSnapshot()
	for _, p := range snapshot.Points {
		if p.Success != 0 || p.Attempts != 0 {
			t.Fatal("hour window retained old counters")
		}
	}
}

func TestRateCapMixedProviderAndHome(t *testing.T) {
	r := newRequestRateCap()
	r.configure(internalconfig.RequestRateCapConfig{Enabled: true, FailFast: true, InitialPerMinute: 1, MinPerMinute: 1})
	a := &Auth{ID: "a", Provider: "codex"}
	b := &Auth{ID: "b", Provider: "claude"}
	r.start(a.ID)
	allowed, err := r.filter([]rateCandidate{{auth: a}, {auth: b}})
	if err != nil || allowed[a.ID] || !allowed[b.ID] {
		t.Fatalf("mixed provider fallback: %v %v", allowed, err)
	}
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}, ProviderRequestRateCap: map[string]internalconfig.RequestRateCapConfig{"codex": {Enabled: true, FailFast: true}}})
	snapshot := m.CodexPoolSnapshot()
	if !snapshot.HomeMode || snapshot.AlgorithmActive || snapshot.RPMAvailable {
		t.Fatalf("Home must be explicit: %+v", snapshot)
	}
}

func TestCodexPoolSnapshotCapacityScopes(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Unix(1800000000, 0)
	m.rateCap.nowFunc = func() time.Time { return now }
	m.auths["busy"] = &Auth{ID: "busy", Provider: "codex", Status: StatusActive}
	m.auths["limited"] = &Auth{ID: "limited", Provider: "codex", Status: StatusActive, Unavailable: true, NextRetryAfter: now.Add(30 * time.Second),
		Quota: QuotaState{Exceeded: true, ReasonCode: "RATE_LIMIT_EXCEEDED", NextRecoverAt: now.Add(30 * time.Second)}}
	m.auths["exhausted"] = &Auth{ID: "exhausted", Provider: "codex", Status: StatusActive, Unavailable: true, NextRetryAfter: now.Add(time.Hour),
		Quota: QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour)}}
	for i := 0; i < 5; i++ {
		m.rateCap.start("busy")
	}
	for i := 0; i < 20; i++ {
		m.rateCap.start("limited")
	}
	for i := 0; i < 3; i++ {
		m.rateCap.start("exhausted")
	}
	s := m.CodexPoolSnapshot()
	// The rate-limited account keeps its capacity but has no room; attempts on the
	// quota-exhausted account count toward RPM only.
	if s.Schedulable != 1 || s.Capacity != 40 || s.Full != 1 || s.Remaining != 15 || s.RPM != 28 {
		t.Fatalf("snapshot: schedulable=%d capacity=%d full=%d remaining=%d rpm=%d", s.Schedulable, s.Capacity, s.Full, s.Remaining, s.RPM)
	}
}
