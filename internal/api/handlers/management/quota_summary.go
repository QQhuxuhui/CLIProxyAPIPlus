package management

import (
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type quotaCounts struct {
	Total     int `json:"total"`
	Available int `json:"available"`
	Cooling   int `json:"cooling"`
	Disabled  int `json:"disabled"`
}

type providerSummary struct {
	Provider  string `json:"provider"`
	Total     int    `json:"total"`
	Available int    `json:"available"`
	Cooling   int    `json:"cooling"`
	Disabled  int    `json:"disabled"`
}

type distributionBucket struct {
	Bucket string `json:"bucket"`
	Count  int    `json:"count"`
}

type soonestRecovery struct {
	Provider  string    `json:"provider"`
	AuthIndex string    `json:"auth_index"`
	Model     string    `json:"model"`
	RecoverAt time.Time `json:"recover_at"`
}

// QuotaSummary is the aggregated account×model availability snapshot.
type QuotaSummary struct {
	Pairs                quotaCounts          `json:"pairs"`
	Accounts             quotaCounts          `json:"accounts"`
	ByProvider           []providerSummary    `json:"by_provider"`
	CooldownDistribution []distributionBucket `json:"cooldown_distribution"`
	SoonestRecovery      *soonestRecovery     `json:"soonest_recovery"`
}

var cooldownBuckets = []string{"<1h", "1-6h", "6-24h", "24-72h", ">72h", "unknown"}

func bucketOf(d time.Duration) string {
	switch {
	case d < time.Hour:
		return "<1h"
	case d < 6*time.Hour:
		return "1-6h"
	case d < 24*time.Hour:
		return "6-24h"
	case d < 72*time.Hour:
		return "24-72h"
	default:
		return ">72h"
	}
}

// buildQuotaSummary aggregates per-(account,model) availability. modelsForClient
// supplies the registered model list for an auth id (registry.GetModelsForClient
// in production; a fake in tests). Cooling classification mirrors
// buildModelStatesEntry (recovery-aware).
func buildQuotaSummary(auths []*coreauth.Auth, modelsForClient func(clientID string) []*registry.ModelInfo, now time.Time) QuotaSummary {
	dist := make(map[string]int, len(cooldownBuckets))
	for _, b := range cooldownBuckets {
		dist[b] = 0
	}
	var pairs, accounts quotaCounts
	provOrder := make([]string, 0)
	provAgg := make(map[string]*providerSummary)
	var soonest *soonestRecovery

	for _, auth := range auths {
		if auth == nil {
			continue
		}
		accounts.Total++
		provider := strings.TrimSpace(auth.Provider)
		ps := provAgg[provider]
		if ps == nil {
			ps = &providerSummary{Provider: provider}
			provAgg[provider] = ps
			provOrder = append(provOrder, provider)
		}

		acctAvailable, acctCooling := 0, 0
		for _, mi := range modelsForClient(auth.ID) {
			if mi == nil {
				continue
			}
			model := mi.ID
			pairs.Total++
			ps.Total++

			if auth.Disabled {
				pairs.Disabled++
				ps.Disabled++
				continue
			}

			state := auth.ModelStates[model]
			cooling := false
			if state != nil {
				cooldownActive := !state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(now)
				unavailableActive := state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now))
				quotaActive := state.Quota.Exceeded && (state.Quota.NextRecoverAt.IsZero() || state.Quota.NextRecoverAt.After(now))
				cooling = cooldownActive || unavailableActive || quotaActive
			}
			if !cooling {
				pairs.Available++
				ps.Available++
				acctAvailable++
				continue
			}

			pairs.Cooling++
			ps.Cooling++
			acctCooling++

			// recoverAt: indefinite quota -> unknown; else max of known future times.
			indefiniteQuota := state.Quota.Exceeded && state.Quota.NextRecoverAt.IsZero()
			var recoverAt time.Time
			if !indefiniteQuota {
				if state.NextRetryAfter.After(now) {
					recoverAt = state.NextRetryAfter
				}
				if state.Quota.Exceeded && state.Quota.NextRecoverAt.After(now) && state.Quota.NextRecoverAt.After(recoverAt) {
					recoverAt = state.Quota.NextRecoverAt
				}
			}
			if indefiniteQuota || recoverAt.IsZero() {
				dist["unknown"]++
				continue
			}
			dist[bucketOf(recoverAt.Sub(now))]++
			if soonest == nil || recoverAt.Before(soonest.RecoverAt) {
				soonest = &soonestRecovery{Provider: provider, AuthIndex: auth.EnsureIndex(), Model: model, RecoverAt: recoverAt}
			}
		}

		if auth.Disabled {
			accounts.Disabled++
		} else if acctAvailable > 0 {
			accounts.Available++
		} else if acctCooling > 0 {
			accounts.Cooling++
		}
	}

	distList := make([]distributionBucket, 0, len(cooldownBuckets))
	for _, b := range cooldownBuckets {
		distList = append(distList, distributionBucket{Bucket: b, Count: dist[b]})
	}
	provList := make([]providerSummary, 0, len(provAgg))
	for _, name := range provOrder {
		provList = append(provList, *provAgg[name])
	}
	sort.Slice(provList, func(i, j int) bool { return provList[i].Provider < provList[j].Provider })

	return QuotaSummary{
		Pairs:                pairs,
		Accounts:             accounts,
		ByProvider:           provList,
		CooldownDistribution: distList,
		SoonestRecovery:      soonest,
	}
}
