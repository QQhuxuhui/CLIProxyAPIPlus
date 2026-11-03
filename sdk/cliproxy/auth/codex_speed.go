package auth

import (
	"context"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// Codex speed groups an attempt is attributed to. Fast and slow follow the ranking; off covers
// attempts while ranked selection is not running; plugin covers picks made by a plugin scheduler.
const (
	CodexSpeedGroupFast   = "fast"
	CodexSpeedGroupSlow   = "slow"
	CodexSpeedGroupOff    = "off"
	CodexSpeedGroupPlugin = "plugin"
)

// Inactive reasons reported while speed priority is configured on but cannot run.
const (
	CodexSpeedInactiveDisabled     = "disabled"
	CodexSpeedInactivePoolsOff     = "pools_disabled"
	CodexSpeedInactivePoolsRestart = "pools_need_restart"
)

const (
	codexSpeedRingSize         = 32
	codexSpeedShardCount       = 64
	codexSpeedSmallInputTokens = 20000
	codexSpeedEntryTTL         = 24 * time.Hour
	// codexSpeedReadyMinScored is the scored-account floor below which ranked selection waits.
	codexSpeedReadyMinScored = 20
	// codexSpeedBinCount is the number of rank segments reported to the page.
	codexSpeedBinCount = 50
	// codexSpeedRecentGaps and codexSpeedDemoteStreak drive the instant demotion of a fast account.
	codexSpeedRecentGaps   = 3
	codexSpeedDemoteStreak = 2
	// codexSpeedBandPoints is the hysteresis band around the fast-group cut, in percent points.
	codexSpeedBandPoints = 5.0
	// CodexSpeedMaxProbesInFlight bounds concurrent probes; it also caps the effective probe rate.
	CodexSpeedMaxProbesInFlight = 32
)

// CodexSpeedAttempt is one finished upstream Codex attempt as seen by usage reporting. Group is
// the account's group when the attempt started ("" excludes it from every speed statistic).
type CodexSpeedAttempt struct {
	AuthID          string
	Success         bool
	Overload        bool
	Gap             time.Duration
	InputTokens     int64
	ReasoningTokens int64
	HasImage        bool
	TTFT            time.Duration
	Group           string
}

// CodexSpeedAuthInfo describes one Codex OAuth account for the minute recompute and probing.
type CodexSpeedAuthInfo struct {
	ID          string
	AccountKey  string
	Schedulable bool
	// QuotaUsed is the account's used percent, or -1 when unknown; it only orders first probes.
	QuotaUsed float64
	// InFlight and ConcurrencyLimit come from account concurrency (limit 0 when not enforced);
	// RateCount and RateLimit from the pool rate window (limit 0 when not enforced).
	InFlight         int
	ConcurrencyLimit int
	RateCount        int
	RateLimit        int
}

// utilization is max(in-flight/concurrency limit, window requests/RPM limit) over the enforced
// limits; ok is false when neither is enforced.
func (i CodexSpeedAuthInfo) utilization() (float64, bool) {
	util, ok := 0.0, false
	if i.ConcurrencyLimit > 0 {
		util, ok = float64(i.InFlight)/float64(i.ConcurrencyLimit), true
	}
	if i.RateLimit > 0 {
		util, ok = max(util, float64(i.RateCount)/float64(i.RateLimit)), true
	}
	return min(util, 1), ok
}

type codexSpeedSample struct {
	at    time.Time
	gap   time.Duration
	probe bool
}

type codexSpeedAccount struct {
	samples        [codexSpeedRingSize]codexSpeedSample
	count          int
	next           int
	okWeight       float64
	ovWeight       float64
	decayAt        time.Time
	lastSeen       time.Time
	lastProbe      time.Time
	probing        bool
	overloadStreak int
	recentGaps     [codexSpeedRecentGaps]time.Duration
	recentN        int
	demotedUntil   time.Time
}

type codexSpeedShard struct {
	mu       sync.Mutex
	accounts map[string]*codexSpeedAccount
}

// CodexSpeedBin is one rank segment of the page chart.
type CodexSpeedBin struct {
	Accounts int      `json:"accounts"`
	Fast     int      `json:"fast"`
	ScoreMs  *int64   `json:"score_ms"`
	Util     *float64 `json:"util"`
}

// codexSpeedView is the result of the last minute recompute, read on the request path.
type codexSpeedView struct {
	ready     bool
	total     int
	scored    int
	fast      map[string]bool
	score     map[string]time.Duration
	penalized map[string]bool
	// slowMedian is the median score of scored accounts outside the fast group.
	slowMedian    time.Duration
	hasSlowMedian bool
	fastCount     int
	slowCount     int
	unscored      int
	fastUtil      *float64
	slowUtil      *float64
	bins          []CodexSpeedBin
	// probeCycle is the time one probe round over the non-fast accounts takes at the effective rate.
	probeCycle time.Duration
}

func emptyCodexSpeedView() *codexSpeedView {
	return &codexSpeedView{fast: map[string]bool{}, score: map[string]time.Duration{}, penalized: map[string]bool{}}
}

// CodexSpeedTracker measures per-account speed, ranks accounts into a fast and a slow group and
// keeps the hourly statistics.
type CodexSpeedTracker struct {
	nowFunc func() time.Time

	cfg      atomic.Pointer[internalconfig.CodexSpeedPriorityConfig]
	active   atomic.Bool
	inactive atomic.Value
	view     atomic.Pointer[codexSpeedView]
	shards   [codexSpeedShardCount]codexSpeedShard
	// demoted maps an auth ID to the end of its instant-demotion cooldown (time.Time).
	demoted sync.Map

	probeDurMu sync.Mutex
	probeDur   time.Duration

	sessions codexSpeedSessions
	stats    codexSpeedStats
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
	t.view.Store(emptyCodexSpeedView())
	for i := range t.shards {
		t.shards[i].accounts = map[string]*codexSpeedAccount{}
	}
	t.sessions.init()
	t.stats.init(now())
	return t
}

func (t *CodexSpeedTracker) now() time.Time { return t.nowFunc() }

func (t *CodexSpeedTracker) config() internalconfig.CodexSpeedPriorityConfig { return *t.cfg.Load() }

// Active reports whether ranked selection and probing are switched on and can run.
func (t *CodexSpeedTracker) Active() bool { return t != nil && t.active.Load() }

// Ready reports whether enough accounts have a score for ranked selection.
func (t *CodexSpeedTracker) Ready() bool { return t != nil && t.view.Load().ready }

// selecting reports whether picks use the fast/slow ranking.
func (t *CodexSpeedTracker) selecting() bool { return t.Active() && t.Ready() }

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
}

func (t *CodexSpeedTracker) shard(id string) *codexSpeedShard {
	return &t.shards[fnv64(id)%codexSpeedShardCount]
}

func (t *CodexSpeedTracker) account(sh *codexSpeedShard, id string, now time.Time) *codexSpeedAccount {
	a := sh.accounts[id]
	if a == nil {
		a = &codexSpeedAccount{}
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
	copy(a.recentGaps[1:], a.recentGaps[:codexSpeedRecentGaps-1])
	a.recentGaps[0] = s.gap
	a.recentN = min(a.recentN+1, codexSpeedRecentGaps)
}

func halfLifeOf(cfg internalconfig.CodexSpeedPriorityConfig) time.Duration {
	return time.Duration(cfg.ScoreHalfLifeMinutes) * time.Minute
}

// demotedActive reports whether id is inside an instant-demotion cooldown at now.
func (t *CodexSpeedTracker) demotedActive(id string, now time.Time) bool {
	v, ok := t.demoted.Load(id)
	if !ok {
		return false
	}
	until, _ := v.(time.Time)
	return now.Before(until)
}

// inFast is the single effective fast-group test used by selection, probing and statistics.
func (t *CodexSpeedTracker) inFast(id string, now time.Time) bool {
	return t != nil && t.view.Load().fast[id] && !t.demotedActive(id, now)
}

// observeLocked records the outcome on a and demotes a fast account instantly when it overloads
// twice in a row or its last three gaps are all slower than the slow-group median.
func (t *CodexSpeedTracker) observeLocked(a *codexSpeedAccount, id string, success, overload bool, gap time.Duration, sample bool, probe bool, now time.Time, cfg internalconfig.CodexSpeedPriorityConfig) {
	a.decayLocked(now, halfLifeOf(cfg))
	switch {
	case overload:
		a.ovWeight++
		a.overloadStreak++
	case success:
		a.okWeight++
		a.overloadStreak = 0
		if sample && gap > 0 {
			a.addSampleLocked(codexSpeedSample{at: now, gap: gap, probe: probe})
		}
	default:
		return
	}
	if !t.inFast(id, now) {
		return
	}
	view := t.view.Load()
	slowGaps := view.hasSlowMedian && a.recentN == codexSpeedRecentGaps
	for i := 0; slowGaps && i < codexSpeedRecentGaps; i++ {
		slowGaps = a.recentGaps[i] > view.slowMedian
	}
	if a.overloadStreak < codexSpeedDemoteStreak && !slowGaps {
		return
	}
	a.demotedUntil = now.Add(time.Duration(cfg.Probe.IntervalMinutes) * time.Minute)
	t.demoted.Store(id, a.demotedUntil)
	a.overloadStreak, a.recentN = 0, 0
	t.stats.countDemotion(now)
}

// ObserveAttempt records one finished attempt: per-account speed and overload, and hourly stats.
func (t *CodexSpeedTracker) ObserveAttempt(at CodexSpeedAttempt) {
	if t == nil || at.AuthID == "" {
		return
	}
	cfg := t.config()
	now := t.now()
	if at.Success || at.Overload {
		small := at.InputTokens < codexSpeedSmallInputTokens && at.ReasoningTokens <= int64(cfg.MaxReasoningTokens) && !at.HasImage
		sh := t.shard(at.AuthID)
		sh.mu.Lock()
		a := t.account(sh, at.AuthID, now)
		t.observeLocked(a, at.AuthID, at.Success && !at.Overload, at.Overload, at.Gap, small, false, now, cfg)
		sh.mu.Unlock()
	}
	t.stats.observeAttempt(at, now)
}

// CodexSpeedProbeResult is the outcome of one background probe.
type CodexSpeedProbeResult struct {
	AuthID   string
	Success  bool
	Overload bool
	Gap      time.Duration
	// Duration is the probe's wall time; it sizes the effective probe rate.
	Duration time.Duration
}

// ObserveProbe records a background probe.
func (t *CodexSpeedTracker) ObserveProbe(res CodexSpeedProbeResult) {
	if t == nil || res.AuthID == "" {
		return
	}
	cfg := t.config()
	now := t.now()
	sh := t.shard(res.AuthID)
	sh.mu.Lock()
	a := t.account(sh, res.AuthID, now)
	a.probing = false
	t.observeLocked(a, res.AuthID, res.Success, res.Overload, res.Gap, true, true, now, cfg)
	sh.mu.Unlock()
	if res.Duration > 0 {
		t.probeDurMu.Lock()
		if t.probeDur == 0 {
			t.probeDur = res.Duration
		} else {
			t.probeDur = (t.probeDur*9 + res.Duration) / 10
		}
		t.probeDurMu.Unlock()
	}
	t.stats.countProbe(now)
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
	interval := time.Duration(cfg.Probe.IntervalMinutes) * time.Minute
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

// probeWindow is how long a probe sample stays valid: long enough for min samples even when one
// probe round takes longer than the configured interval.
func probeWindow(cfg internalconfig.CodexSpeedPriorityConfig, cycle time.Duration) time.Duration {
	interval := max(time.Duration(cfg.Probe.IntervalMinutes)*time.Minute, cycle)
	return max(time.Duration(cfg.ScoreWindowMinutes)*time.Minute, time.Duration(cfg.ScoreMinSamples+1)*interval)
}

// scoreLocked is the decay-weighted median gap of the valid samples; ok is false with fewer
// than the minimum samples.
func (a *codexSpeedAccount) scoreLocked(cfg internalconfig.CodexSpeedPriorityConfig, now time.Time, probeValid time.Duration) (time.Duration, bool) {
	passive := time.Duration(cfg.ScoreWindowMinutes) * time.Minute
	halfLife := halfLifeOf(cfg)
	type weighted struct {
		gap time.Duration
		w   float64
	}
	var valid []weighted
	for i := 0; i < a.count; i++ {
		s := a.samples[i]
		window := passive
		if s.probe {
			window = probeValid
		}
		age := now.Sub(s.at)
		if age < 0 || age > window {
			continue
		}
		valid = append(valid, weighted{s.gap, math.Pow(0.5, float64(age)/float64(halfLife))})
	}
	if len(valid) < cfg.ScoreMinSamples || len(valid) == 0 {
		return 0, false
	}
	slices.SortFunc(valid, func(x, y weighted) int { return int(x.gap - y.gap) })
	var total float64
	for _, v := range valid {
		total += v.w
	}
	var cum float64
	for _, v := range valid {
		cum += v.w
		if cum >= total/2 {
			return v.gap, true
		}
	}
	return valid[len(valid)-1].gap, true
}

// overloadRateLocked is the decayed overload percentage (0 without any outcome).
func (a *codexSpeedAccount) overloadRateLocked(now time.Time, halfLife time.Duration) float64 {
	a.decayLocked(now, halfLife)
	if mass := a.okWeight + a.ovWeight; mass > 0 {
		return 100 * a.ovWeight / mass
	}
	return 0
}

// codexSpeedFastMembers applies the hysteresis band to the ranked IDs (fastest first): previous
// members stay while inside keep, others enter inside enter, and the group fills up to its
// target by rank while below it. total is the denominator of percent: the number of scored
// accounts, so accounts without a score never inflate the target.
func codexSpeedFastMembers(ranked []string, prev map[string]bool, total int, percent int) map[string]bool {
	x := float64(percent)
	band := min(codexSpeedBandPoints, x/2)
	target := int(math.Round(x / 100 * float64(total)))
	keep := int(math.Ceil((x + band) / 100 * float64(total)))
	enter := int(math.Floor((x - band) / 100 * float64(total)))
	fast := map[string]bool{}
	for rank, id := range ranked {
		if prev[id] && rank < keep {
			fast[id] = true
		}
	}
	for rank, id := range ranked {
		if fast[id] {
			continue
		}
		if rank < enter || (len(fast) < target && rank < keep) {
			fast[id] = true
		}
	}
	return fast
}

// Recompute ranks every schedulable account and publishes the new view. infos lists the current
// Codex OAuth accounts. The fast group is fast-percent of the scored accounts (penalized ones
// included); slow is the rest of the scored accounts, and unscored accounts belong to neither.
func (t *CodexSpeedTracker) Recompute(infos []CodexSpeedAuthInfo) {
	if t == nil {
		return
	}
	cfg := t.config()
	now := t.now()
	prev := t.view.Load()
	probeValid := probeWindow(cfg, prev.probeCycle)
	halfLife := halfLifeOf(cfg)
	schedulable := make(map[string]CodexSpeedAuthInfo, len(infos))
	for _, info := range infos {
		if info.Schedulable {
			schedulable[info.ID] = info
		}
	}
	view := emptyCodexSpeedView()
	view.total = len(schedulable)
	type ranked struct {
		id    string
		score time.Duration
	}
	var rankable []ranked
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		for id, a := range sh.accounts {
			if now.Sub(a.lastSeen) > codexSpeedEntryTTL && !a.probing && now.Sub(a.lastProbe) > codexSpeedEntryTTL {
				delete(sh.accounts, id)
				continue
			}
			if _, ok := schedulable[id]; !ok {
				continue
			}
			// Overload and demotion are recorded with or without a score, so an overloaded account
			// that never collected enough samples still sorts with the penalized ones.
			penalized := a.overloadRateLocked(now, halfLife) > float64(cfg.ScoreOverloadPercent) || now.Before(a.demotedUntil)
			if penalized {
				view.penalized[id] = true
			}
			score, ok := a.scoreLocked(cfg, now, probeValid)
			if !ok {
				continue
			}
			view.score[id] = score
			if penalized {
				continue
			}
			rankable = append(rankable, ranked{id, score})
		}
		sh.mu.Unlock()
	}
	view.scored = len(view.score)
	view.ready = view.scored >= codexSpeedReadyMinScored
	slices.SortFunc(rankable, func(x, y ranked) int {
		if x.score != y.score {
			return int(x.score - y.score)
		}
		return strings.Compare(x.id, y.id)
	})
	order := make([]string, len(rankable))
	for i, r := range rankable {
		order[i] = r.id
	}
	view.fast = codexSpeedFastMembers(order, prev.fast, view.scored, cfg.FastPercent)
	view.fastCount = len(view.fast)
	view.unscored = view.total - view.scored
	view.slowCount = view.scored - view.fastCount

	var slowScores []time.Duration
	for id, score := range view.score {
		if !view.fast[id] {
			slowScores = append(slowScores, score)
		}
	}
	if len(slowScores) > 0 {
		slices.Sort(slowScores)
		view.slowMedian, view.hasSlowMedian = slowScores[len(slowScores)/2], true
	}

	var fastUtil, slowUtil utilAvg
	for id, info := range schedulable {
		if util, ok := info.utilization(); ok {
			if view.fast[id] {
				fastUtil.add(util)
			} else if _, scored := view.score[id]; scored {
				slowUtil.add(util)
			}
		}
	}
	view.fastUtil, view.slowUtil = fastUtil.value(), slowUtil.value()
	view.bins = codexSpeedBins(schedulable, view)
	view.probeCycle = t.probeCycle(cfg, view.total-view.fastCount)

	var toSlow, toFast int64
	if prev.ready {
		for id := range prev.fast {
			if _, ok := schedulable[id]; ok && !view.fast[id] {
				toSlow++
			}
		}
		for id := range view.fast {
			if !prev.fast[id] {
				toFast++
			}
		}
	}
	t.demoted.Range(func(key, value any) bool {
		if until, _ := value.(time.Time); !now.Before(until) {
			t.demoted.CompareAndDelete(key, value)
		}
		return true
	})
	t.view.Store(view)
	t.stats.sampleMinute(now, cfg, t.Active(), view, toSlow, toFast)
}

// probeCycle estimates one probe round over n accounts at the effective probe rate.
func (t *CodexSpeedTracker) probeCycle(cfg internalconfig.CodexSpeedPriorityConfig, n int) time.Duration {
	if n <= 0 {
		return 0
	}
	rate := float64(cfg.Probe.RatePerSecond)
	t.probeDurMu.Lock()
	dur := t.probeDur
	t.probeDurMu.Unlock()
	if dur > 0 {
		rate = min(rate, float64(CodexSpeedMaxProbesInFlight)/dur.Seconds())
	}
	if rate <= 0 {
		return 0
	}
	return time.Duration(float64(n) / rate * float64(time.Second))
}

type utilAvg struct {
	sum float64
	n   int
}

func (u *utilAvg) add(v float64) { u.sum += v; u.n++ }

func (u utilAvg) value() *float64 {
	if u.n == 0 {
		return nil
	}
	v := u.sum / float64(u.n)
	return &v
}

// codexSpeedBins orders every schedulable account by score (unscored last, by ID) and splits
// the order into codexSpeedBinCount segments.
func codexSpeedBins(schedulable map[string]CodexSpeedAuthInfo, view *codexSpeedView) []CodexSpeedBin {
	ids := make([]string, 0, len(schedulable))
	for id := range schedulable {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(x, y string) int {
		sx, okX := view.score[x]
		sy, okY := view.score[y]
		switch {
		case okX && !okY:
			return -1
		case !okX && okY:
			return 1
		case okX && okY && sx != sy:
			return int(sx - sy)
		}
		return strings.Compare(x, y)
	})
	n := len(ids)
	bins := make([]CodexSpeedBin, codexSpeedBinCount)
	for i := range bins {
		lo, hi := i*n/codexSpeedBinCount, (i+1)*n/codexSpeedBinCount
		var scores []time.Duration
		var util utilAvg
		for _, id := range ids[lo:hi] {
			bins[i].Accounts++
			if view.fast[id] {
				bins[i].Fast++
			}
			if s, ok := view.score[id]; ok {
				scores = append(scores, s)
			}
			if u, ok := schedulable[id].utilization(); ok {
				util.add(u)
			}
		}
		if len(scores) > 0 {
			ms := scores[len(scores)/2].Milliseconds()
			bins[i].ScoreMs = &ms
		}
		bins[i].Util = util.value()
	}
	return bins
}

// overflowKey orders non-fast candidates: healthy scored accounts by score, then accounts
// without a score (speed unknown), then penalized or just-demoted accounts, scored ones by score
// and unscored ones last. Overload and demotion are checked before the score.
func (t *CodexSpeedTracker) overflowKey(view *codexSpeedView, id string, now time.Time) (int, time.Duration) {
	score, ok := view.score[id]
	switch {
	case view.penalized[id] || t.demotedActive(id, now):
		if !ok {
			return 2, time.Duration(math.MaxInt64)
		}
		return 2, score
	case !ok:
		return 1, 0
	default:
		return 0, score
	}
}

// pickRanked picks a uniformly random effective fast account when one is among auths, and
// otherwise the best overflow candidate (random among equals). fastOnly returns nil instead of
// overflowing.
func (t *CodexSpeedTracker) pickRanked(auths []*Auth, fastOnly bool) *Auth {
	now := t.now()
	var fast []*Auth
	for _, a := range auths {
		if a != nil && t.inFast(a.ID, now) {
			fast = append(fast, a)
		}
	}
	if len(fast) > 0 {
		return fast[rand.IntN(len(fast))]
	}
	if fastOnly {
		return nil
	}
	view := t.view.Load()
	var best []*Auth
	bestClass, bestScore := math.MaxInt, time.Duration(math.MaxInt64)
	for _, a := range auths {
		if a == nil {
			continue
		}
		class, score := t.overflowKey(view, a.ID, now)
		switch {
		case class < bestClass || (class == bestClass && score < bestScore):
			best, bestClass, bestScore = []*Auth{a}, class, score
		case class == bestClass && score == bestScore:
			best = append(best, a)
		}
	}
	if len(best) == 0 {
		return nil
	}
	return best[rand.IntN(len(best))]
}

// ProbeCandidates returns up to limit schedulable accounts outside the effective fast group that
// are due for a probe: unscored ones first (never probed ordered by quota used ascending), then
// by last probe time.
func (t *CodexSpeedTracker) ProbeCandidates(infos []CodexSpeedAuthInfo, limit int) []string {
	if t == nil || limit <= 0 {
		return nil
	}
	cfg := t.config()
	now := t.now()
	view := t.view.Load()
	interval := time.Duration(cfg.Probe.IntervalMinutes) * time.Minute
	type cand struct {
		id        string
		scored    bool
		lastProbe time.Time
		quota     float64
	}
	var cands []cand
	for _, info := range infos {
		if !info.Schedulable || t.inFast(info.ID, now) {
			continue
		}
		sh := t.shard(info.ID)
		sh.mu.Lock()
		a := sh.accounts[info.ID]
		need, last := true, time.Time{}
		if a != nil {
			last = a.lastProbe
			need = !a.probing && (a.lastProbe.IsZero() || now.Sub(a.lastProbe) >= interval)
		}
		sh.mu.Unlock()
		if !need {
			continue
		}
		quota := info.QuotaUsed
		if quota < 0 {
			quota = math.MaxFloat64
		}
		_, scored := view.score[info.ID]
		cands = append(cands, cand{info.ID, scored, last, quota})
	}
	slices.SortFunc(cands, func(x, y cand) int {
		if x.scored != y.scored {
			if !x.scored {
				return -1
			}
			return 1
		}
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

// codexSpeedAccountKey is the stable upstream account identity.
func codexSpeedAccountKey(a *Auth) string {
	if a == nil {
		return ""
	}
	if id, _ := a.Metadata["account_id"].(string); strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	return a.ID
}

// codexSpeedRequestState marks, per client request, which account a plugin scheduler picked so
// the attempt on exactly that account is attributed to the plugin group.
type codexSpeedRequestState struct {
	mu           sync.Mutex
	pluginAuthID string
}

type codexSpeedStateKey struct{}

func codexSpeedStateFrom(ctx context.Context) *codexSpeedRequestState {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(codexSpeedStateKey{}).(*codexSpeedRequestState)
	return s
}

// withCodexSpeedState attaches a request state when the request may reach Codex.
func withCodexSpeedState(ctx context.Context, providers []string) context.Context {
	if !hasCodexProvider(providers) || codexSpeedStateFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, codexSpeedStateKey{}, &codexSpeedRequestState{})
}

// notePick records the source of the latest pick of this request: the plugin-picked account ID,
// or "" for a built-in pick.
func notePick(ctx context.Context, pluginAuthID string) {
	if s := codexSpeedStateFrom(ctx); s != nil {
		s.mu.Lock()
		s.pluginAuthID = pluginAuthID
		s.mu.Unlock()
	}
}

// GroupAt returns the speed group of an attempt starting on a: "" for accounts that are not
// Codex OAuth (excluded from speed statistics), plugin when a plugin scheduler picked it, off
// while ranked selection is not running, otherwise fast or slow.
func (t *CodexSpeedTracker) GroupAt(ctx context.Context, a *Auth) string {
	if t == nil || !codexSpeedOAuth(a) {
		return ""
	}
	if s := codexSpeedStateFrom(ctx); s != nil {
		s.mu.Lock()
		plugin := s.pluginAuthID != "" && s.pluginAuthID == a.ID
		s.mu.Unlock()
		if plugin {
			return CodexSpeedGroupPlugin
		}
	}
	if !t.selecting() {
		return CodexSpeedGroupOff
	}
	if t.inFast(a.ID, t.now()) {
		return CodexSpeedGroupFast
	}
	return CodexSpeedGroupSlow
}
