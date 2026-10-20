package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func inFlightLimitTestAuth(id string, priority string) *Auth {
	return &Auth{ID: id, Provider: "codex", Status: StatusActive, Attributes: map[string]string{"priority": priority}}
}

func TestSetProviderInFlightLimitNormalizes(t *testing.T) {
	defer SetProviderInFlightLimit(nil)
	SetProviderInFlightLimit(map[string]int{" Codex ": 4, "gemini": 0, "": 3})
	if got := providerInFlightLimitFor("codex"); got != 4 {
		t.Fatalf("codex limit = %d, want 4", got)
	}
	if got := providerInFlightLimitFor("gemini"); got != 0 {
		t.Fatalf("zero limit must mean unlimited, got %d", got)
	}
	if !inFlightLimitsConfigured() {
		t.Fatal("limits should be configured")
	}
	SetProviderInFlightLimit(nil)
	if inFlightLimitsConfigured() {
		t.Fatal("limits should be cleared")
	}
}

func TestAvailableAuthsSpillToNextTierWhenFastTierSaturated(t *testing.T) {
	defer SetProviderInFlightLimit(nil)
	SetProviderInFlightLimit(map[string]int{"codex": 2})
	m := &Manager{}
	fast := inFlightLimitTestAuth("fast", "2")
	normal := inFlightLimitTestAuth("normal", "1")
	auths := []*Auth{fast, normal}
	now := time.Now()

	got, err := m.availableAuthsForRouteModel(auths, "codex", "gpt-5", now)
	if err != nil || len(got) != 1 || got[0].ID != "fast" {
		t.Fatalf("idle fast tier must be served first: got=%v err=%v", authIDs(got), err)
	}

	m.beginInFlight("fast")
	got, err = m.availableAuthsForRouteModel(auths, "codex", "gpt-5", now)
	if err != nil || len(got) != 1 || got[0].ID != "fast" {
		t.Fatalf("below the cap the fast tier stays: got=%v err=%v", authIDs(got), err)
	}

	m.beginInFlight("fast")
	got, err = m.availableAuthsForRouteModel(auths, "codex", "gpt-5", now)
	if err != nil || len(got) != 1 || got[0].ID != "normal" {
		t.Fatalf("saturated fast tier must spill to normal: got=%v err=%v", authIDs(got), err)
	}

	m.beginInFlight("normal")
	m.beginInFlight("normal")
	got, err = m.availableAuthsForRouteModel(auths, "codex", "gpt-5", now)
	if err != nil || len(got) != 1 || got[0].ID != "fast" {
		t.Fatalf("when everything is saturated the cap is waived and the top tier is returned: got=%v err=%v", authIDs(got), err)
	}

	gotAll, err := m.availableAuthsForRouteModelAcrossPriorities(auths, "codex", "gpt-5", now)
	if err != nil || len(gotAll) != 2 {
		t.Fatalf("across priorities with everything saturated must return everything: got=%v err=%v", authIDs(gotAll), err)
	}
}

func TestAvailableAuthsIgnoreCapForOtherProviders(t *testing.T) {
	defer SetProviderInFlightLimit(nil)
	SetProviderInFlightLimit(map[string]int{"codex": 1})
	m := &Manager{}
	gem := &Auth{ID: "gem", Provider: "gemini", Status: StatusActive, Attributes: map[string]string{"priority": "2"}}
	other := &Auth{ID: "other", Provider: "gemini", Status: StatusActive, Attributes: map[string]string{"priority": "1"}}
	m.beginInFlight("gem")
	m.beginInFlight("gem")
	got, err := m.availableAuthsForRouteModel([]*Auth{gem, other}, "gemini", "gemini-pro", time.Now())
	if err != nil || len(got) != 1 || got[0].ID != "gem" {
		t.Fatalf("providers without a cap are unaffected: got=%v err=%v", authIDs(got), err)
	}
	if m.InFlightLimit("gem") != 0 {
		t.Fatalf("InFlightLimit for an unknown credential must be 0")
	}
}

func TestSchedulerFastPathSkipsSaturatedCredential(t *testing.T) {
	defer SetProviderInFlightLimit(nil)
	SetProviderInFlightLimit(map[string]int{"codex": 1})
	m := &Manager{}
	m.scheduler = newAuthScheduler(&RoundRobinSelector{})
	m.scheduler.inFlight = &m.inFlight
	a := inFlightLimitTestAuth("a", "2")
	b := inFlightLimitTestAuth("b", "1")
	m.scheduler.upsertAuth(a)
	m.scheduler.upsertAuth(b)
	m.beginInFlight("a")

	picked, err := m.scheduler.pickSingle(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
	if err != nil || picked == nil || picked.ID != "b" {
		t.Fatalf("saturated a must be skipped for b: picked=%v err=%v", pickedID(picked), err)
	}
	m.beginInFlight("b")
	picked, err = m.scheduler.pickSingle(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
	if err != nil || picked == nil || picked.ID != "a" {
		t.Fatalf("all saturated waives the cap and returns the top tier: picked=%v err=%v", pickedID(picked), err)
	}
}

func authIDs(auths []*Auth) []string {
	out := make([]string, 0, len(auths))
	for _, a := range auths {
		out = append(out, a.ID)
	}
	return out
}

func pickedID(a *Auth) string {
	if a == nil {
		return "<nil>"
	}
	return a.ID
}
