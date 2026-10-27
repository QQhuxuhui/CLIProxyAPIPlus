package auth

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// Histogram boundaries are seconds; overflow is reported as the final boundary.
var poolTTFTBounds = [...]float64{.1, .25, .5, 1, 2, 3, 4, 5, 6, 7, 8, 10, 12, 15, 20, 30, 45, 60, 90, 120, 180, 300, 600}

type PoolPoint struct {
	Time           time.Time `json:"time"`
	Attempts       int       `json:"attempts"`
	Capacity       int       `json:"capacity"`
	Success        int       `json:"success"`
	Overload       int       `json:"overload"`
	RateLimits     int       `json:"rate_limits"`
	QuotaExhausted int       `json:"quota_exhausted"`
	OtherFailures  int       `json:"other_failures"`
	FastFailures   int       `json:"fast_failures"`
	TTFTP50        float64   `json:"ttft_p50"`
	TTFTP90        float64   `json:"ttft_p90"`
	histogram      [len(poolTTFTBounds)]int
}
type poolMetrics struct{ points [60]PoolPoint }

func (p *poolMetrics) bucket(now time.Time) *PoolPoint {
	minute := now.Truncate(time.Minute)
	idx := (minute.Unix() / 60) % 60
	if idx < 0 {
		idx += 60
	}
	b := &p.points[idx]
	if !b.Time.Equal(minute) {
		*b = PoolPoint{Time: minute}
	}
	return b
}
func poolPercentile(hist [len(poolTTFTBounds)]int, q float64) float64 {
	total := 0
	for _, n := range hist {
		total += n
	}
	if total == 0 {
		return 0
	}
	rank := int(math.Ceil(float64(total) * q))
	sum := 0
	for i, n := range hist {
		sum += n
		if sum >= rank {
			return poolTTFTBounds[i]
		}
	}
	return 0
}

type PoolLimitBucket struct {
	Limit    int `json:"limit"`
	Accounts int `json:"accounts"`
}

// CodexPoolSnapshot contains aggregate metrics only, without credential identifiers.
type CodexPoolSnapshot struct {
	UpstreamOverloads []CodexOverloadPoint                `json:"upstream_overloads"`
	BootstrapReleases map[string]uint64                   `json:"bootstrap_releases"`
	Config            internalconfig.RequestRateCapConfig `json:"config"`
	HomeMode          bool                                `json:"home_mode"`
	RPMAvailable      bool                                `json:"rpm_available"`
	AlgorithmActive   bool                                `json:"algorithm_active"`
	Schedulable       int                                 `json:"schedulable"`
	Cooling           int                                 `json:"cooling"`
	CoolingReasons    map[string]int                      `json:"cooling_reasons"`
	Disabled          int                                 `json:"disabled"`
	Expired           int                                 `json:"expired"`
	Capacity          int                                 `json:"capacity"`
	RPM               int                                 `json:"rpm"`
	Remaining         int                                 `json:"remaining"`
	EffectiveCapacity float64                             `json:"effective_capacity"`
	Full              int                                 `json:"full"`
	SuccessRate       float64                             `json:"success_rate"`
	TTFTP50           float64                             `json:"ttft_p50"`
	TTFTP90           float64                             `json:"ttft_p90"`
	FastFailures      int                                 `json:"fast_failures"`
	Limits            []PoolLimitBucket                   `json:"limits"`
	Points            []PoolPoint                         `json:"points"`
}

func (m *Manager) CodexPoolSnapshot() CodexPoolSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	auths := m.auths
	r := m.rateCap
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.nowFunc()
	out := CodexPoolSnapshot{UpstreamOverloads: m.codexOverloadSnapshot(), BootstrapReleases: cliproxyexecutor.CodexBootstrapMetrics(), Config: r.config, HomeMode: r.home, RPMAvailable: !r.home, AlgorithmActive: r.config.Enabled && !r.home, CoolingReasons: map[string]int{}, Limits: []PoolLimitBucket{}, Points: make([]PoolPoint, 0, 60)}
	distribution := map[int]int{}
	live := map[string]bool{}
	for _, a := range auths {
		if a.Provider != "codex" {
			continue
		}
		live[a.ID] = true
		count, limit, _ := r.accountLocked(a.ID, now)
		state := r.accounts[a.ID]
		distribution[state.limit]++
		if count >= limit {
			out.Full++
		}
		if a.Disabled || a.Status == StatusDisabled {
			out.Disabled++
			continue
		}
		if expiry, ok := a.AccessTokenExpirationTime(); ok && !expiry.After(now) {
			out.Expired++
			continue
		}
		blocked, reason, next := isAuthBlockedForModel(a, "", now)
		if blocked {
			if reason == blockReasonCooldown || next.After(now) {
				out.Cooling++
				key := a.Quota.ReasonCode
				if key == "" {
					key = a.Quota.Reason
				}
				if key == "" {
					key = "cooldown"
				}
				out.CoolingReasons[key]++
			}
			continue
		}
		out.Schedulable++
		out.Capacity += limit
	}
	for id, a := range r.accounts {
		r.getLocked(id, now)
		out.RPM += len(a.attempts)
		if !live[id] && len(a.attempts) == 0 {
			delete(r.accounts, id)
		}
	}
	// Home owns the pool: a local subset cannot estimate its capacity.
	if out.HomeMode {
		out.Capacity = 0
		out.Schedulable = 0
		out.Full = 0
	}
	out.Remaining = out.Capacity - out.RPM
	r.metrics.bucket(now).Capacity = out.Capacity
	var hist [len(poolTTFTBounds)]int
	successes, total, overload := 0, 0, 0
	for i := 59; i >= 0; i-- {
		minute := now.Truncate(time.Minute).Add(-time.Duration(i) * time.Minute)
		idx := (minute.Unix() / 60) % 60
		if idx < 0 {
			idx += 60
		}
		point := r.metrics.points[idx]
		if !point.Time.Equal(minute) {
			point = PoolPoint{Time: minute}
		}
		point.TTFTP50 = poolPercentile(point.histogram, .5)
		point.TTFTP90 = poolPercentile(point.histogram, .9)
		out.Points = append(out.Points, point)
		out.FastFailures += point.FastFailures
		if i < 5 {
			successes += point.Success
			overload += point.Overload
			total += point.Success + point.Overload + point.RateLimits + point.QuotaExhausted + point.OtherFailures
			for j, n := range point.histogram {
				hist[j] += n
			}
		}
	}
	out.EffectiveCapacity = float64(out.Capacity)
	if total > 0 {
		out.SuccessRate = float64(successes) / float64(total)
		out.EffectiveCapacity *= 1 - float64(overload)/float64(total)
	}
	out.TTFTP50 = poolPercentile(hist, .5)
	out.TTFTP90 = poolPercentile(hist, .9)
	keys := make([]int, 0, len(distribution))
	for n := range distribution {
		keys = append(keys, n)
	}
	sort.Ints(keys)
	for _, n := range keys {
		out.Limits = append(out.Limits, PoolLimitBucket{n, distribution[n]})
	}
	return out
}

func (r *requestRateCap) HandleUsage(ctx context.Context, record usage.Record) {
	if ctx != nil {
		if _, ok := ctx.Value(rateAttemptContextKey{}).(*rateAttemptRef); ok && !cliproxyexecutor.UpstreamAttempted(ctx) {
			return
		}
	}
	if record.Provider != "codex" || record.AdditionalModel || !usage.GenerateEnabled(record.Generate) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.metrics.bucket(r.nowFunc())
	if !record.Failed {
		b.Success++
		if record.TTFT > 0 {
			seconds := record.TTFT.Seconds()
			idx := sort.SearchFloat64s(poolTTFTBounds[:], seconds)
			if idx == len(poolTTFTBounds) {
				idx--
			}
			b.histogram[idx]++
		}
		return
	}
	switch {
	case strings.Contains(strings.ToLower(record.Fail.Body), "usage_limit_reached"):
		b.QuotaExhausted++
	case record.Fail.StatusCode == 429:
		b.RateLimits++
	case record.Fail.StatusCode == 502 || record.Fail.StatusCode == 503:
		b.Overload++
	default:
		b.OtherFailures++
	}
}

type codexPoolUsagePlugin struct {
	ctx context.Context
	cap *requestRateCap
}

func (p codexPoolUsagePlugin) HandleUsage(ctx context.Context, r usage.Record) {
	if p.ctx.Err() == nil {
		p.cap.HandleUsage(ctx, r)
	}
}

// StartCodexPoolMetrics binds collection and minute sampling to the service lifetime.
func (m *Manager) StartCodexPoolMetrics(ctx context.Context) {
	if m == nil {
		return
	}
	usage.RegisterNamedPlugin("codex-pool", codexPoolUsagePlugin{ctx, m.rateCap})
	m.CodexPoolSnapshot()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				snapshot := m.CodexPoolSnapshot()
				limits := make([]int, 0)
				for _, b := range snapshot.Limits {
					for i := 0; i < b.Accounts; i++ {
						limits = append(limits, b.Limit)
					}
				}
				percentile := func(q float64) int {
					if len(limits) == 0 {
						return 0
					}
					return limits[int(math.Ceil(float64(len(limits))*q))-1]
				}
				rateLimits := snapshot.Points[len(snapshot.Points)-2].RateLimits
				log.WithFields(log.Fields{"full_accounts": snapshot.Full, "limit_p10": percentile(.1), "limit_p50": percentile(.5), "limit_p90": percentile(.9), "rate_limits": rateLimits}).Info("codex pool rate capacity")
			}
		}
	}()
}
