package management

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
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

type modelSummary struct {
	Provider        string     `json:"provider"`
	Model           string     `json:"model"`
	Total           int        `json:"total"`
	Available       int        `json:"available"`
	Cooling         int        `json:"cooling"`
	Disabled        int        `json:"disabled"`
	NextRecoverAt   *time.Time `json:"next_recover_at,omitempty"` // min known future recoverAt among cooling pairs
	LastRecoverAt   *time.Time `json:"last_recover_at,omitempty"` // max known future recoverAt (full-capacity ETA)
	UnknownRecovery int        `json:"unknown_recovery"`          // cooling pairs with indefinite/unknown recovery
}

// QuotaSummary is the aggregated account×model availability snapshot.
type QuotaSummary struct {
	Pairs                quotaCounts          `json:"pairs"`
	Accounts             quotaCounts          `json:"accounts"`
	ByProvider           []providerSummary    `json:"by_provider"`
	ByModel              []modelSummary       `json:"by_model"`
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
// in production; a fake in tests). Cooling classification mirrors the router's
// blocking predicate (isAuthBlockedForModel in sdk/cliproxy/auth/selector.go),
// so the dashboard never reports capacity the router disagrees with. The
// detail endpoints (model_states, see auth_files.go/buildModelStatesEntry)
// intentionally show a broader set of error rows the router may still use.
func buildQuotaSummary(auths []*coreauth.Auth, modelsForClient func(clientID string) []*registry.ModelInfo, now time.Time) QuotaSummary {
	dist := make(map[string]int, len(cooldownBuckets))
	for _, b := range cooldownBuckets {
		dist[b] = 0
	}
	var pairs, accounts quotaCounts
	provOrder := make([]string, 0)
	provAgg := make(map[string]*providerSummary)
	modelAgg := make(map[string]*modelSummary)
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

		authDisabled := auth.Disabled || auth.Status == coreauth.StatusDisabled
		acctAvailable, acctCooling := 0, 0
		for _, mi := range modelsForClient(auth.ID) {
			if mi == nil {
				continue
			}
			model := mi.ID
			pairs.Total++
			ps.Total++

			mkey := provider + "\x00" + model
			msum := modelAgg[mkey]
			if msum == nil {
				msum = &modelSummary{Provider: provider, Model: model}
				modelAgg[mkey] = msum
			}
			msum.Total++

			if authDisabled {
				pairs.Disabled++
				ps.Disabled++
				msum.Disabled++
				continue
			}

			state := auth.ModelStates[model]
			if state != nil && state.Status == coreauth.StatusDisabled {
				pairs.Disabled++
				ps.Disabled++
				msum.Disabled++
				continue
			}
			// Cooling mirrors the router's blocking predicate (isAuthBlockedForModel):
			// a pair is unroutable only while Unavailable with a future NextRetryAfter.
			// States the router still uses (zero NextRetryAfter under disable-cooling,
			// elapsed cooldowns, indefinite quota) count as available so the dashboard
			// never reports capacity the router disagrees with.
			cooling := state != nil && state.Unavailable && state.NextRetryAfter.After(now)
			if !cooling {
				pairs.Available++
				ps.Available++
				msum.Available++
				acctAvailable++
				continue
			}

			pairs.Cooling++
			ps.Cooling++
			msum.Cooling++
			acctCooling++

			// cooling implies state.Unavailable with a future NextRetryAfter (see predicate
			// above), so recoverAt is always known; fold in a later quota recovery time if any.
			recoverAt := state.NextRetryAfter
			if state.Quota.Exceeded && state.Quota.NextRecoverAt.After(recoverAt) {
				recoverAt = state.Quota.NextRecoverAt
			}
			dist[bucketOf(recoverAt.Sub(now))]++
			if msum.NextRecoverAt == nil || recoverAt.Before(*msum.NextRecoverAt) {
				tNext := recoverAt
				msum.NextRecoverAt = &tNext
			}
			if msum.LastRecoverAt == nil || recoverAt.After(*msum.LastRecoverAt) {
				tLast := recoverAt
				msum.LastRecoverAt = &tLast
			}
			if soonest == nil || recoverAt.Before(soonest.RecoverAt) {
				soonest = &soonestRecovery{Provider: provider, AuthIndex: auth.EnsureIndex(), Model: model, RecoverAt: recoverAt}
			}
		}

		if authDisabled {
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

	modelList := make([]modelSummary, 0, len(modelAgg))
	for _, m := range modelAgg {
		modelList = append(modelList, *m)
	}
	sort.Slice(modelList, func(i, j int) bool {
		if modelList[i].Provider != modelList[j].Provider {
			return modelList[i].Provider < modelList[j].Provider
		}
		return modelList[i].Model < modelList[j].Model
	})

	return QuotaSummary{
		Pairs:                pairs,
		Accounts:             accounts,
		ByProvider:           provList,
		ByModel:              modelList,
		CooldownDistribution: distList,
		SoonestRecovery:      soonest,
	}
}

// GetQuotaSummary returns the aggregated account×model availability snapshot.
func (h *Handler) GetQuotaSummary(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	summary := buildQuotaSummary(h.authManager.List(), registry.GetGlobalRegistry().GetModelsForClient, time.Now())
	c.JSON(http.StatusOK, summary)
}
