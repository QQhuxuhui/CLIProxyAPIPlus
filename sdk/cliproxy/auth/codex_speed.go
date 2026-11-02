package auth

import (
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexspeed"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// Codex speed tiers.
const (
	CodexSpeedTierFast    = "fast"
	CodexSpeedTierSlow    = "slow"
	CodexSpeedTierUnknown = "unknown"
)

// Speed-priority cohorts split fast accounts by upstream account so that only one half gets
// the fast weight; comparing their fast->slow rates shows whether priority wears accounts out.
const (
	CodexSpeedCohortBoost = "boost"
	CodexSpeedCohortHold  = "hold"
)

// Inactive reasons reported while speed priority is configured on but cannot run.
const (
	CodexSpeedInactiveDisabled     = "disabled"
	CodexSpeedInactivePoolsOff     = "pools_disabled"
	CodexSpeedInactivePoolsRestart = "pools_need_restart"
)

const (
	codexSpeedRingSize            = 32
	codexSpeedShardCount          = 64
	codexSpeedBaselineWindow      = 15 * time.Minute
	codexSpeedBaselineMinAccounts = 20
	codexSpeedSmallInputTokens    = 20000
	codexSpeedEntryTTL            = 24 * time.Hour
	codexSpeedProbeExpirySoon     = 10 * time.Minute
)

// CodexSpeedAttempt is one finished upstream Codex attempt as seen by usage reporting.
type CodexSpeedAttempt struct {
	AuthID          string
	Success         bool
	Overload        bool
	Gap             time.Duration
	PreCreated      time.Duration
	InputTokens     int64
	ReasoningTokens int64
	HasImage        bool
	TTFT            time.Duration
	Group           string
	Generation      uint64
}

// CodexSpeedAuthInfo describes one Codex OAuth account for the minute recompute and probing.
type CodexSpeedAuthInfo struct {
	ID          string
	AccountKey  string
	Schedulable bool
	// QuotaUsed is the account's used percent, or -1 when unknown; it only orders first probes.
	QuotaUsed float64
}

type codexSpeedSample struct {
	at         time.Time
	gap        time.Duration
	preCreated time.Duration
	probe      bool
}

type codexSpeedAccount struct {
	samples     [codexSpeedRingSize]codexSpeedSample
	count       int
	next        int
	okWeight    float64
	ovWeight    float64
	decayAt     time.Time
	tier        string
	lastKnown   string
	accountKey  string
	lastSeen    time.Time
	lastProbe   time.Time
	probing     bool
	validUntil  time.Time
	sampleCount int
}

type codexSpeedShard struct {
	mu       sync.Mutex
	accounts map[string]*codexSpeedAccount
}

// codexSpeedView is the result of the last minute recompute, read on the request path.
type codexSpeedView struct {
	classify   bool
	baselineMs int64
	hasBase    bool
	tiers      map[string]string
	// boost holds the auth IDs whose upstream account is in the boost cohort, computed from the
	// account key learned at recompute time (selection snapshots do not carry account_id).
	boost  map[string]bool
	counts codexspeed.SpeedTiers
}

// CodexSpeedTracker measures per-account speed and runs the speed-priority A/B.
type CodexSpeedTracker struct {
	nowFunc func() time.Time

	cfg      atomic.Pointer[internalconfig.CodexSpeedPriorityConfig]
	active   atomic.Bool
	inactive atomic.Value
	view     atomic.Pointer[codexSpeedView]
	shards   [codexSpeedShardCount]codexSpeedShard

	ab     codexSpeedAB
	cohort codexSpeedCohort
}

var defaultCodexSpeedTracker = NewCodexSpeedTracker(nil)

// CodexSpeed returns the process-wide tracker.
func CodexSpeed() *CodexSpeedTracker { return defaultCodexSpeedTracker }

// NewCodexSpeedTracker returns a tracker with default (disabled) configuration.
func NewCodexSpeedTracker(now func() time.Time) *CodexSpeedTracker {
	if now == nil {
		now = time.Now
	}
	t := &CodexSpeedTracker{nowFunc: now}
	cfg := internalconfig.CodexSpeedPriorityConfig{}
	cfg.Normalize()
	t.cfg.Store(&cfg)
	t.inactive.Store(CodexSpeedInactiveDisabled)
	t.view.Store(&codexSpeedView{tiers: map[string]string{}, boost: map[string]bool{}})
	for i := range t.shards {
		t.shards[i].accounts = map[string]*codexSpeedAccount{}
	}
	t.ab.init(now())
	return t
}

func (t *CodexSpeedTracker) now() time.Time { return t.nowFunc() }

func (t *CodexSpeedTracker) config() internalconfig.CodexSpeedPriorityConfig { return *t.cfg.Load() }

// Active reports whether weighting, probing and rebinding run.
func (t *CodexSpeedTracker) Active() bool { return t != nil && t.active.Load() }

// InactiveReason reports why speed priority is not active ("" when active).
func (t *CodexSpeedTracker) InactiveReason() string {
	if t.Active() {
		return ""
	}
	reason, _ := t.inactive.Load().(string)
	return reason
}

// SetConfig applies a normalized config and the pool runtime state. poolsConfigured is the
// config switch; poolsRunning is whether the manager actually runs pools.
func (t *CodexSpeedTracker) SetConfig(cfg internalconfig.CodexSpeedPriorityConfig, poolsConfigured, poolsRunning bool) {
	if t == nil {
		return
	}
	cfg.Normalize()
	t.cfg.Store(&cfg)
	reason := ""
	switch {
	case !cfg.Enable:
		reason = CodexSpeedInactiveDisabled
	case !poolsConfigured:
		reason = CodexSpeedInactivePoolsOff
	case !poolsRunning:
		reason = CodexSpeedInactivePoolsRestart
	}
	t.inactive.Store(reason)
	t.active.Store(reason == "")
	t.ab.applyConfig(cfg, t.now())
}

func (t *CodexSpeedTracker) shard(id string) *codexSpeedShard {
	return &t.shards[fnv64(id)%codexSpeedShardCount]
}

func (t *CodexSpeedTracker) account(sh *codexSpeedShard, id string, now time.Time) *codexSpeedAccount {
	a := sh.accounts[id]
	if a == nil {
		a = &codexSpeedAccount{tier: CodexSpeedTierUnknown, accountKey: id}
		sh.accounts[id] = a
	}
	a.lastSeen = now
	return a
}

// decayLocked ages the overload/success weights to now.
func (a *codexSpeedAccount) decayLocked(now time.Time, halfLife time.Duration) {
	if !a.decayAt.IsZero() && now.After(a.decayAt) && halfLife > 0 {
		f := math.Pow(0.5, float64(now.Sub(a.decayAt))/float64(halfLife))
		a.okWeight *= f
		a.ovWeight *= f
	}
	a.decayAt = now
}

func (a *codexSpeedAccount) addSampleLocked(s codexSpeedSample) {
	a.samples[a.next] = s
	a.next = (a.next + 1) % codexSpeedRingSize
	a.count = min(a.count+1, codexSpeedRingSize)
}

// ObserveAttempt records one finished attempt: per-account speed and overload, and A/B stats.
func (t *CodexSpeedTracker) ObserveAttempt(at CodexSpeedAttempt) {
	if t == nil || at.AuthID == "" {
		return
	}
	cfg := t.config()
	now := t.now()
	if at.Success || at.Overload {
		sh := t.shard(at.AuthID)
		sh.mu.Lock()
		a := t.account(sh, at.AuthID, now)
		a.decayLocked(now, time.Duration(cfg.HalfLifeMinutes)*time.Minute)
		if at.Overload {
			a.ovWeight++
		} else {
			a.okWeight++
			if at.Gap > 0 && at.InputTokens < codexSpeedSmallInputTokens && at.ReasoningTokens <= int64(cfg.MaxReasoningTokens) && !at.HasImage {
				a.addSampleLocked(codexSpeedSample{at: now, gap: at.Gap, preCreated: at.PreCreated})
			}
		}
		sh.mu.Unlock()
	}
	t.ab.observeAttempt(at, now)
}

// CodexSpeedProbeResult is the outcome of one active probe.
type CodexSpeedProbeResult struct {
	AuthID   string
	Success  bool
	Overload bool
	Gap      time.Duration
}

// ObserveProbe records an active probe. Probe results never enter the A/B stats.
func (t *CodexSpeedTracker) ObserveProbe(res CodexSpeedProbeResult) {
	if t == nil || res.AuthID == "" {
		return
	}
	cfg := t.config()
	now := t.now()
	sh := t.shard(res.AuthID)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	a := t.account(sh, res.AuthID, now)
	a.probing = false
	a.decayLocked(now, time.Duration(cfg.HalfLifeMinutes)*time.Minute)
	switch {
	case res.Overload:
		a.ovWeight++
	case res.Success:
		a.okWeight++
		if res.Gap > 0 {
			a.addSampleLocked(codexSpeedSample{at: now, gap: res.Gap, probe: true})
		}
	}
	t.ab.countProbe()
}

// ReserveProbe marks an account as probing before the network call; it fails when the account
// is already being probed or was probed within the configured interval.
func (t *CodexSpeedTracker) ReserveProbe(authID string) bool {
	if t == nil || authID == "" {
		return false
	}
	cfg := t.config()
	now := t.now()
	sh := t.shard(authID)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	a := t.account(sh, authID, now)
	interval := time.Duration(cfg.Probe.PerAccountIntervalMinutes) * time.Minute
	if a.probing || (!a.lastProbe.IsZero() && now.Sub(a.lastProbe) < interval) {
		return false
	}
	a.probing = true
	a.lastProbe = now
	return true
}

// CancelProbe clears a probe reservation whose request never started.
func (t *CodexSpeedTracker) CancelProbe(authID string, lastProbe time.Time) {
	if t == nil {
		return
	}
	sh := t.shard(authID)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if a := sh.accounts[authID]; a != nil {
		a.probing = false
		a.lastProbe = lastProbe
	}
}

// LastProbe returns the account's last probe start.
func (t *CodexSpeedTracker) LastProbe(authID string) time.Time {
	sh := t.shard(authID)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if a := sh.accounts[authID]; a != nil {
		return a.lastProbe
	}
	return time.Time{}
}

// classifyLocked computes the account's tier from valid samples at now.
func (a *codexSpeedAccount) classifyLocked(cfg internalconfig.CodexSpeedPriorityConfig, now time.Time) (string, time.Time, int) {
	passiveWindow := time.Duration(cfg.SampleWindowMinutes) * time.Minute
	probeWindow := time.Duration(cfg.MinSamples*cfg.Probe.PerAccountIntervalMinutes) * time.Minute
	halfLife := time.Duration(cfg.HalfLifeMinutes) * time.Minute
	type weighted struct {
		gap time.Duration
		w   float64
	}
	var valid []weighted
	var expiries []time.Time
	for i := 0; i < a.count; i++ {
		s := a.samples[i]
		window := passiveWindow
		if s.probe {
			window = probeWindow
		}
		age := now.Sub(s.at)
		if age < 0 || age > window {
			continue
		}
		valid = append(valid, weighted{s.gap, math.Pow(0.5, float64(age)/float64(halfLife))})
		expiries = append(expiries, s.at.Add(window))
	}
	if len(valid) < cfg.MinSamples {
		return CodexSpeedTierUnknown, time.Time{}, len(valid)
	}
	// The tier stays valid until fewer than MinSamples samples remain.
	slices.SortFunc(expiries, func(x, y time.Time) int { return y.Compare(x) })
	validUntil := expiries[cfg.MinSamples-1]
	slices.SortFunc(valid, func(x, y weighted) int { return int(x.gap - y.gap) })
	var total float64
	for _, v := range valid {
		total += v.w
	}
	var cum float64
	median := valid[len(valid)-1].gap
	for _, v := range valid {
		cum += v.w
		if cum >= total/2 {
			median = v.gap
			break
		}
	}
	a.decayLocked(now, halfLife)
	rate := 0.0
	if mass := a.okWeight + a.ovWeight; mass > 0 {
		rate = 100 * a.ovWeight / mass
	}
	fastGap := time.Duration(cfg.FastGapMs) * time.Millisecond
	switch {
	case median >= fastGap || rate >= float64(cfg.SlowOverloadPercent):
		return CodexSpeedTierSlow, validUntil, len(valid)
	case rate < float64(cfg.FastMaxOverloadPercent):
		return CodexSpeedTierFast, validUntil, len(valid)
	default:
		return CodexSpeedTierUnknown, time.Time{}, len(valid)
	}
}

// codexSpeedAccountKey is the stable upstream account identity: one cohort per ChatGPT account.
func codexSpeedAccountKey(a *Auth) string {
	if a == nil {
		return ""
	}
	if id, _ := a.Metadata["account_id"].(string); strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	return a.ID
}

func codexSpeedCohortOf(key string) string {
	if fnv64("codex-speed-cohort:"+key)%2 == 0 {
		return CodexSpeedCohortBoost
	}
	return CodexSpeedCohortHold
}

// Recompute reclassifies every account, refreshes the pool baseline and returns the tier
// transitions. infos lists the current Codex OAuth accounts (nil keeps known entries).
func (t *CodexSpeedTracker) Recompute(infos []CodexSpeedAuthInfo) []codexspeed.Transition {
	if t == nil {
		return nil
	}
	cfg := t.config()
	now := t.now()
	keys := make(map[string]string, len(infos))
	for _, info := range infos {
		keys[info.ID] = info.AccountKey
	}
	view := &codexSpeedView{tiers: map[string]string{}, boost: map[string]bool{}}
	var accountMedians []time.Duration
	var transitions []codexspeed.Transition
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		for id, a := range sh.accounts {
			if key, ok := keys[id]; ok && key != "" {
				a.accountKey = key
			}
			if now.Sub(a.lastSeen) > codexSpeedEntryTTL && !a.probing && now.Sub(a.lastProbe) > codexSpeedEntryTTL {
				delete(sh.accounts, id)
				continue
			}
			tier, validUntil, n := a.classifyLocked(cfg, now)
			a.tier, a.validUntil, a.sampleCount = tier, validUntil, n
			if med, ok := a.recentMedianLocked(now); ok {
				accountMedians = append(accountMedians, med)
			}
			cohort := codexSpeedCohortOf(a.accountKey)
			if cohort == CodexSpeedCohortBoost {
				view.boost[id] = true
			}
			switch tier {
			case CodexSpeedTierFast:
				view.counts.Fast++
				if cohort == CodexSpeedCohortBoost {
					view.counts.BoostFast++
				} else {
					view.counts.HoldFast++
				}
				view.tiers[id] = tier
			case CodexSpeedTierSlow:
				view.counts.Slow++
				view.tiers[id] = tier
			default:
				view.counts.Unknown++
			}
			if a.lastKnown != "" {
				// Record the carried-over tier in today's set before any change, so a transition
				// on the first recompute after midnight still counts in today's denominator.
				t.cohort.observe(now, a.accountKey, cohort, a.lastKnown)
			}
			if tier != CodexSpeedTierUnknown {
				if a.lastKnown != "" && a.lastKnown != tier {
					transitions = append(transitions, codexspeed.Transition{Time: now, AccountKey: a.accountKey, AuthID: id, From: a.lastKnown, To: tier, Cohort: cohort})
				}
				a.lastKnown = tier
			}
			if a.lastKnown != "" {
				t.cohort.observe(now, a.accountKey, cohort, a.lastKnown)
			}
		}
		sh.mu.Unlock()
	}
	for _, tr := range transitions {
		t.cohort.transition(now, tr)
	}
	if len(accountMedians) >= codexSpeedBaselineMinAccounts {
		slices.Sort(accountMedians)
		view.baselineMs = accountMedians[len(accountMedians)/2].Milliseconds()
		view.hasBase = true
		view.classify = view.baselineMs >= int64(cfg.BaselineMinMs)
	}
	t.view.Store(view)
	return transitions
}

// recentMedianLocked is the account's median gap over the baseline window.
func (a *codexSpeedAccount) recentMedianLocked(now time.Time) (time.Duration, bool) {
	var gaps []time.Duration
	for i := 0; i < a.count; i++ {
		s := a.samples[i]
		if age := now.Sub(s.at); age >= 0 && age <= codexSpeedBaselineWindow {
			gaps = append(gaps, s.gap)
		}
	}
	if len(gaps) == 0 {
		return 0, false
	}
	slices.Sort(gaps)
	return gaps[len(gaps)/2], true
}

// Classifying reports whether tiers apply (pool baseline at or above baseline-min-ms).
func (t *CodexSpeedTracker) Classifying() bool {
	return t != nil && t.view.Load().classify
}

// TierOf returns the effective tier: unknown while not classifying.
func (t *CodexSpeedTracker) TierOf(authID string) string {
	if t == nil {
		return CodexSpeedTierUnknown
	}
	v := t.view.Load()
	if !v.classify {
		return CodexSpeedTierUnknown
	}
	if tier, ok := v.tiers[authID]; ok {
		return tier
	}
	return CodexSpeedTierUnknown
}

// weightFor is the treatment selection weight of a: hold-cohort fast accounts get the unknown weight.
func (t *CodexSpeedTracker) weightFor(a *Auth, cfg internalconfig.CodexSpeedPriorityConfig) int64 {
	base := authWeight(a)
	if base <= 0 {
		return 0
	}
	tierWeight := int64(cfg.Weights.Unknown)
	switch t.TierOf(a.ID) {
	case CodexSpeedTierFast:
		if t.view.Load().boost[a.ID] {
			tierWeight = int64(cfg.Weights.Fast)
		}
	case CodexSpeedTierSlow:
		tierWeight = int64(cfg.Weights.Slow)
	}
	return saturatingMulInt64(base, tierWeight)
}

func saturatingMulInt64(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

// ProbeCandidates returns up to limit schedulable accounts that need a probe: unknown or soon
// expiring, not probed within the interval and not in flight. Never-probed accounts come
// first, ordered by quota used ascending; the rest by last probe time.
func (t *CodexSpeedTracker) ProbeCandidates(infos []CodexSpeedAuthInfo, limit int) []string {
	if t == nil || limit <= 0 {
		return nil
	}
	cfg := t.config()
	now := t.now()
	interval := time.Duration(cfg.Probe.PerAccountIntervalMinutes) * time.Minute
	type cand struct {
		id        string
		lastProbe time.Time
		quota     float64
	}
	var cands []cand
	for _, info := range infos {
		if !info.Schedulable {
			continue
		}
		sh := t.shard(info.ID)
		sh.mu.Lock()
		a := sh.accounts[info.ID]
		need, last := true, time.Time{}
		if a != nil {
			last = a.lastProbe
			soon := a.tier != CodexSpeedTierUnknown && !a.validUntil.IsZero() && a.validUntil.Sub(now) <= codexSpeedProbeExpirySoon
			need = (a.tier == CodexSpeedTierUnknown || soon) && !a.probing && (a.lastProbe.IsZero() || now.Sub(a.lastProbe) >= interval)
		}
		sh.mu.Unlock()
		if need {
			quota := info.QuotaUsed
			if quota < 0 {
				quota = math.MaxFloat64
			}
			cands = append(cands, cand{info.ID, last, quota})
		}
	}
	slices.SortFunc(cands, func(x, y cand) int {
		if x.lastProbe.IsZero() != y.lastProbe.IsZero() {
			if x.lastProbe.IsZero() {
				return -1
			}
			return 1
		}
		if x.lastProbe.IsZero() {
			switch {
			case x.quota < y.quota:
				return -1
			case x.quota > y.quota:
				return 1
			}
			return strings.Compare(x.id, y.id)
		}
		return x.lastProbe.Compare(y.lastProbe)
	})
	out := make([]string, 0, min(limit, len(cands)))
	for _, c := range cands[:min(limit, len(cands))] {
		out = append(out, c.id)
	}
	return out
}

// preCreatedByTier returns the median pre-created wait of fast and slow accounts' recent samples.
func (t *CodexSpeedTracker) preCreatedByTier(since time.Time) (fast, slow *int64) {
	var f, s []time.Duration
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		for _, a := range sh.accounts {
			for j := 0; j < a.count; j++ {
				sample := a.samples[j]
				if sample.probe || sample.at.Before(since) || sample.preCreated <= 0 {
					continue
				}
				switch a.tier {
				case CodexSpeedTierFast:
					f = append(f, sample.preCreated)
				case CodexSpeedTierSlow:
					s = append(s, sample.preCreated)
				}
			}
		}
		sh.mu.Unlock()
	}
	med := func(v []time.Duration) *int64 {
		if len(v) == 0 {
			return nil
		}
		slices.Sort(v)
		ms := v[len(v)/2].Milliseconds()
		return &ms
	}
	return med(f), med(s)
}
