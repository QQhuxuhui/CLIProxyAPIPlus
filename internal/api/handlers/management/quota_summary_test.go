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

	// by_model: sorted by provider then model asc; 7 rows
	// antigravity: m-cool, m-expired, m-far, m-indef, m-ok; claude: c1, c2
	if len(s.ByModel) != 7 {
		t.Fatalf("by_model rows = %d, want 7: %+v", len(s.ByModel), s.ByModel)
	}
	bm := map[string]modelSummary{}
	for _, m := range s.ByModel {
		bm[m.Provider+"/"+m.Model] = m
	}
	mc := bm["antigravity/m-cool"]
	if mc.Total != 1 || mc.Available != 0 || mc.Cooling != 1 || mc.UnknownRecovery != 0 ||
		mc.NextRecoverAt == nil || !mc.NextRecoverAt.Equal(future6h) ||
		mc.LastRecoverAt == nil || !mc.LastRecoverAt.Equal(future6h) {
		t.Errorf("m-cool = %+v, want cooling1 next=last=future6h", mc)
	}
	mi := bm["antigravity/m-indef"]
	if mi.Cooling != 1 || mi.UnknownRecovery != 1 || mi.NextRecoverAt != nil || mi.LastRecoverAt != nil {
		t.Errorf("m-indef = %+v, want cooling1 unknown1 no recover times", mi)
	}
	mo := bm["antigravity/m-ok"]
	if mo.Available != 1 || mo.Cooling != 0 || mo.NextRecoverAt != nil {
		t.Errorf("m-ok = %+v, want available1", mo)
	}
	c1 := bm["claude/c1"]
	if c1.Total != 1 || c1.Disabled != 1 || c1.Available != 0 {
		t.Errorf("c1 = %+v, want disabled1", c1)
	}
	if s.ByModel[0].Provider != "antigravity" || s.ByModel[0].Model != "m-cool" ||
		s.ByModel[6].Provider != "claude" || s.ByModel[6].Model != "c2" {
		t.Errorf("by_model order wrong: first=%s/%s last=%s/%s",
			s.ByModel[0].Provider, s.ByModel[0].Model, s.ByModel[6].Provider, s.ByModel[6].Model)
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

func TestBuildQuotaSummary_ByModelMinMaxUnknown(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	t2h := now.Add(2 * time.Hour)
	t50h := now.Add(50 * time.Hour)
	mkCooling := func(at time.Time) *coreauth.ModelState {
		return &coreauth.ModelState{Unavailable: true, NextRetryAfter: at,
			Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: at}}
	}
	auths := []*coreauth.Auth{
		{ID: "b1", Index: "1", Provider: "p", ModelStates: map[string]*coreauth.ModelState{"m": mkCooling(t2h)}},
		{ID: "b2", Index: "2", Provider: "p", ModelStates: map[string]*coreauth.ModelState{"m": mkCooling(t50h)}},
		{ID: "b3", Index: "3", Provider: "p", ModelStates: map[string]*coreauth.ModelState{
			"m": {Quota: coreauth.QuotaState{Exceeded: true}}, // indefinite
		}},
	}
	lister := func(id string) []*registry.ModelInfo { return []*registry.ModelInfo{{ID: "m"}} }
	s := buildQuotaSummary(auths, lister, now)
	if len(s.ByModel) != 1 {
		t.Fatalf("by_model rows = %d, want 1", len(s.ByModel))
	}
	m := s.ByModel[0]
	if m.Total != 3 || m.Cooling != 3 || m.Available != 0 || m.UnknownRecovery != 1 {
		t.Fatalf("m = %+v, want total3 cooling3 unknown1", m)
	}
	if m.NextRecoverAt == nil || !m.NextRecoverAt.Equal(t2h) {
		t.Errorf("NextRecoverAt = %v, want %v (min)", m.NextRecoverAt, t2h)
	}
	if m.LastRecoverAt == nil || !m.LastRecoverAt.Equal(t50h) {
		t.Errorf("LastRecoverAt = %v, want %v (max known)", m.LastRecoverAt, t50h)
	}
}
