package management

import (
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestBuildQuotaSummary(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	future6h := now.Add(6 * time.Hour)
	future90h := now.Add(90 * time.Hour)
	past := now.Add(-time.Hour)

	auths := []*coreauth.Auth{
		// A1 antigravity: m-ok available, m-cool cooling(6h future), m-indef indefinite quota
		{ID: "a1", Index: "1", Provider: "antigravity", ModelStates: map[string]*coreauth.ModelState{
			"m-cool":  {Unavailable: true, NextRetryAfter: future6h, Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: future6h}},
			"m-indef": {Quota: coreauth.QuotaState{Exceeded: true}}, // NextRecoverAt zero -> indefinite
		}},
		// A2 antigravity: m-expired(过期不计 cooling -> available), m-far cooling(90h)
		{ID: "a2", Index: "2", Provider: "antigravity", ModelStates: map[string]*coreauth.ModelState{
			"m-expired": {Unavailable: true, NextRetryAfter: past, Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: past}},
			"m-far":     {NextRetryAfter: future90h, Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: future90h}},
		}},
		// A3 disabled account
		{ID: "a3", Index: "3", Provider: "claude", Disabled: true},
		// A4 zero registered models -> counts in accounts.total only
		{ID: "a4", Index: "4", Provider: "gemini"},
	}
	models := map[string][]*registry.ModelInfo{
		"a1": {{ID: "m-ok"}, {ID: "m-cool"}, {ID: "m-indef"}},
		"a2": {{ID: "m-expired"}, {ID: "m-far"}},
		"a3": {{ID: "c1"}, {ID: "c2"}},
		"a4": nil,
	}
	lister := func(id string) []*registry.ModelInfo { return models[id] }

	s := buildQuotaSummary(auths, lister, now)

	// pairs: a1=3 (m-ok avail, m-cool cool, m-indef cool), a2=2 (m-expired avail, m-far cool), a3=2 disabled
	if s.Pairs.Total != 7 || s.Pairs.Available != 2 || s.Pairs.Cooling != 3 || s.Pairs.Disabled != 2 {
		t.Fatalf("pairs = %+v, want total7 avail2 cool3 disabled2", s.Pairs)
	}
	// accounts: total4; a1 has m-ok avail -> available; a2 has m-expired avail -> available; a3 disabled; a4 no models -> none
	if s.Accounts.Total != 4 || s.Accounts.Available != 2 || s.Accounts.Cooling != 0 || s.Accounts.Disabled != 1 {
		t.Fatalf("accounts = %+v, want total4 avail2 cool0 disabled1", s.Accounts)
	}
	// distribution: m-cool(6h)->"1-6h"? 6h is not <6h so falls in 6-24h boundary — see bucketOf (<6h => 1-6h). 6h -> 6-24h.
	got := map[string]int{}
	for _, b := range s.CooldownDistribution {
		got[b.Bucket] = b.Count
	}
	if len(s.CooldownDistribution) != 6 {
		t.Fatalf("distribution buckets = %d, want 6", len(s.CooldownDistribution))
	}
	if got["6-24h"] != 1 || got[">72h"] != 1 || got["unknown"] != 1 {
		t.Errorf("distribution = %v, want 6-24h:1 >72h:1 unknown:1", got)
	}
	// soonest_recovery: earliest known future recoverAt = m-cool @ future6h on a1
	if s.SoonestRecovery == nil || !s.SoonestRecovery.RecoverAt.Equal(future6h) ||
		s.SoonestRecovery.Model != "m-cool" || s.SoonestRecovery.AuthIndex != "1" {
		t.Errorf("soonest = %+v, want m-cool @ future6h idx1", s.SoonestRecovery)
	}
	// by_provider sorted: antigravity, claude, gemini
	if len(s.ByProvider) != 3 || s.ByProvider[0].Provider != "antigravity" || s.ByProvider[2].Provider != "gemini" {
		t.Errorf("by_provider = %+v", s.ByProvider)
	}
}

func TestBuildQuotaSummary_NoCooling_SoonestNull(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	auths := []*coreauth.Auth{{ID: "a1", Index: "1", Provider: "x"}}
	lister := func(id string) []*registry.ModelInfo { return []*registry.ModelInfo{{ID: "m1"}} }
	s := buildQuotaSummary(auths, lister, now)
	if s.SoonestRecovery != nil {
		t.Errorf("soonest = %+v, want nil", s.SoonestRecovery)
	}
	if s.Pairs.Available != 1 || s.Pairs.Cooling != 0 {
		t.Errorf("pairs = %+v", s.Pairs)
	}
}
