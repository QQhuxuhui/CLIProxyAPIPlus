package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func recoveringAuth(id, model string, now time.Time) *Auth {
	return &Auth{ID: id, ModelStates: map[string]*ModelState{
		model: {Unavailable: true, NextRetryAfter: now.Add(-time.Second), Quota: QuotaState{Exceeded: true, NextRecoverAt: now.Add(-time.Second)}},
	}}
}

func TestCooldownProbeKey(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if _, ok := cooldownProbeKey(recoveringAuth("a", "gemini-3-pro", now), "gemini-3-pro", now); !ok {
		t.Fatal("expired cooldown must count as recovering")
	}
	if _, ok := cooldownProbeKey(recoveringAuth("a", "gemini-3-pro", now), "gemini-3-flash", now); ok {
		t.Fatal("another model's cooldown must not gate this model")
	}
	healthy := &Auth{ID: "b", ModelStates: map[string]*ModelState{"gemini-3-pro": {}}}
	if _, ok := cooldownProbeKey(healthy, "gemini-3-pro", now); ok {
		t.Fatal("healthy credential must not be gated")
	}
	if _, ok := cooldownProbeKey(&Auth{ID: "c"}, "gemini-3-pro", now); ok {
		t.Fatal("credential without model states must not be gated")
	}
}

func TestCooldownProbeLeaseIsExclusiveAndExpires(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	previousNow := cooldownProbeNow
	cooldownProbeNow = func() time.Time { return now }
	t.Cleanup(func() { cooldownProbeNow = previousNow })

	key := "lease-test|gemini-3-pro"
	t.Cleanup(func() { cooldownProbeLeases.Delete(key) })
	first, second := &cooldownProbe{}, &cooldownProbe{}
	if !first.acquire(key) {
		t.Fatal("first request must get the probe")
	}
	if second.acquire(key) {
		t.Fatal("second request must not get a live probe")
	}
	first.release()
	if !second.acquire(key) {
		t.Fatal("probe must be available after release")
	}
	// A lease that is never released stops blocking once it expires.
	now = now.Add(cooldownProbeLease + time.Second)
	third := &cooldownProbe{}
	if !third.acquire(key) {
		t.Fatal("expired lease must be replaceable")
	}
	// The request that outlived its lease must not release its successor's lease.
	second.release()
	if (&cooldownProbe{}).acquire(key) {
		t.Fatal("stale release removed the lease held by a newer request")
	}
	third.release()
	var missing *cooldownProbe
	missing.release()
}

func TestPickNextMixedProbedRotatesThenFallsBack(t *testing.T) {
	SetCooldownProbeGate(true)
	t.Cleanup(func() { SetCooldownProbeGate(false) })

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&customStreamMockExecutor{identifier: "codex"})
	model := "probe-gate-model"
	past := time.Now().Add(-time.Minute)
	recoveringID, healthyID := "probe-gate-recovering", "probe-gate-healthy"
	for _, candidate := range []*Auth{
		{ID: recoveringID, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "9"}, ModelStates: map[string]*ModelState{
			model: {Unavailable: true, NextRetryAfter: past, Quota: QuotaState{Exceeded: true, NextRecoverAt: past}},
		}},
		{ID: healthyID, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": "1"}},
	} {
		registry.GetGlobalRegistry().RegisterClient(candidate.ID, "codex", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
		if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	pick := func(probe *cooldownProbe, tried map[string]struct{}) string {
		t.Helper()
		selected, _, _, errPick := manager.pickNextMixedProbed(context.Background(), []string{"codex"}, model, cliproxyexecutor.Options{}, tried, probe)
		if errPick != nil {
			t.Fatalf("pickNextMixedProbed() error = %v", errPick)
		}
		return selected.ID
	}

	prober := &cooldownProbe{}
	t.Cleanup(prober.release)
	if got := pick(prober, map[string]struct{}{}); got != recoveringID {
		t.Fatalf("first request picked %s, want the recovering credential", got)
	}

	// While the probe is in flight, other requests rotate to the healthy credential.
	other := &cooldownProbe{}
	t.Cleanup(other.release)
	tried := map[string]struct{}{}
	if got := pick(other, tried); got != healthyID {
		t.Fatalf("concurrent request picked %s, want the healthy credential", got)
	}
	// When nothing else is left, the skipped credential is served instead of failing.
	tried[healthyID] = struct{}{}
	if got := pick(other, tried); got != recoveringID {
		t.Fatalf("fallback picked %s, want the skipped credential", got)
	}

	// Once the probe is released the credential is pickable again.
	prober.release()
	third := &cooldownProbe{}
	t.Cleanup(third.release)
	if got := pick(third, map[string]struct{}{}); got != recoveringID {
		t.Fatalf("after release picked %s, want the recovering credential", got)
	}
}

func TestPickNextMixedProbedDisabledIsPassThrough(t *testing.T) {
	SetCooldownProbeGate(false)
	probe := &cooldownProbe{}
	if probe.acquire("passthrough|m"); probe.held == "" {
		t.Fatal("setup: lease not held")
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	_, _, _, errPick := manager.pickNextMixedProbed(context.Background(), []string{"codex"}, "none", cliproxyexecutor.Options{}, map[string]struct{}{}, probe)
	if errPick == nil {
		t.Fatal("expected pick error with no credentials")
	}
	if probe.held != "" {
		t.Fatal("previous lease must be released even when the gate is off")
	}
}

func TestPickNextMixedProbedGatesPrefixedRouteModel(t *testing.T) {
	SetCooldownProbeGate(true)
	t.Cleanup(func() { SetCooldownProbeGate(false) })

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&customStreamMockExecutor{identifier: "codex"})
	model, routeModel := "probe-prefix-model", "team/probe-prefix-model"
	past := time.Now().Add(-time.Minute)
	candidate := &Auth{ID: "probe-prefix-auth", Provider: "codex", Prefix: "team", Status: StatusActive, ModelStates: map[string]*ModelState{
		model: {Unavailable: true, NextRetryAfter: past, Quota: QuotaState{Exceeded: true, NextRecoverAt: past}},
	}}
	registry.GetGlobalRegistry().RegisterClient(candidate.ID, "codex", []*registry.ModelInfo{{ID: routeModel}, {ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
	if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
		t.Fatal(errRegister)
	}

	prober := &cooldownProbe{}
	t.Cleanup(prober.release)
	if _, _, _, errPick := manager.pickNextMixedProbed(context.Background(), []string{"codex"}, routeModel, cliproxyexecutor.Options{}, map[string]struct{}{}, prober); errPick != nil {
		t.Fatalf("pickNextMixedProbed() error = %v", errPick)
	}
	if prober.held == "" {
		t.Fatal("prefixed route model bypassed the probe gate")
	}
}
