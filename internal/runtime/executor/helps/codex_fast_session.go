package helps

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexspeed"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// Codex fast Session-Id groups.
const (
	// CodexFastSessionGroupOff means the request was not considered (feature disabled, empty
	// pool, no original identity, or outside the current runtime share) and is not counted in
	// the group stats.
	CodexFastSessionGroupOff = "off"
	// CodexFastSessionGroupFast means the Session-Id header was remapped onto the pool.
	CodexFastSessionGroupFast = "fast"
	// CodexFastSessionGroupControl means the request was outside the canary share and kept
	// its original Session-Id; it is the baseline for the fast group.
	CodexFastSessionGroupControl = "control"
)

const (
	codexFastSessionCanarySalt = "cli-proxy-api:codex:fast-session:canary:"
	codexFastSessionHRWSalt    = "cli-proxy-api:codex:fast-session:hrw:"
	// codexFastSessionSmallInputTokens bounds the "small request" TTFT samples.
	codexFastSessionSmallInputTokens = 20000
	// codexFastSessionBucketSamples caps the TTFT samples kept per group per minute.
	codexFastSessionBucketSamples = 256
	codexFastSessionMaxEvents     = 50
)

// CodexFastSessionPool holds the current fast Session-Id values, the per-group stats, the
// rollback state, and recent pool events. It is safe for concurrent use.
type CodexFastSessionPool struct {
	nowFunc func() time.Time

	mu              sync.RWMutex
	enabled         bool
	percent         int
	rollbackEnabled bool
	epoch           atomic.Uint64
	seed            []string
	seedGen         uint64
	members         []string
	// adaptivePercent is the runtime fast share, at most the configured cap. It only moves
	// through EvaluateRollback and resets to the cap on a config change or restart.
	adaptivePercent int
	lastAdjustAt    time.Time
	rollback        CodexFastSessionRollbackStatus
	events          []CodexFastSessionEvent
	lastRollback    CodexFastSessionRollbackCheck

	statsMu sync.Mutex
	stats   map[string]*codexFastSessionGroupStats
	// hours accumulates per-hour group stats for the persisted history, keyed by the hour start.
	hours map[time.Time]*codexFastSessionHourAcc

	prober atomic.Pointer[CodexFastSessionProber]
}

type codexFastSessionBucket struct {
	minute   time.Time
	requests int64
	failures int64
	ttft     []time.Duration
}

// codexFastSessionHourAcc holds one hour of group stats for a single epoch. A configuration
// change inside the hour restarts the hour under the new epoch, matching the rolling windows.
type codexFastSessionHourAcc struct {
	epoch   uint64
	fast    codexspeed.GroupHour
	control codexspeed.GroupHour
}

func (a *codexFastSessionHourAcc) group(name string) *codexspeed.GroupHour {
	switch name {
	case CodexFastSessionGroupFast:
		return &a.fast
	case CodexFastSessionGroupControl:
		return &a.control
	}
	return nil
}

type codexFastSessionGroupStats struct {
	requests int64
	failures int64
	// buckets back the reported rolling window.
	buckets [60]codexFastSessionBucket
	// decision holds the same requests and failures for EvaluateRollback, which clears it on
	// each adjustment so that buckets keep their rolling-window meaning.
	decision [60]codexFastSessionBucket
}

// CodexFastSessionGroupSnapshot is a read-only view of one group's stats. Window fields cover
// the last WindowMinutes minutes; TTFT only counts successful small requests.
type CodexFastSessionGroupSnapshot struct {
	TotalRequests     int64   `json:"total_requests"`
	TotalFailures     int64   `json:"total_failures"`
	WindowMinutes     int     `json:"window_minutes"`
	WindowRequests    int64   `json:"window_requests"`
	WindowFailures    int64   `json:"window_failures"`
	WindowFailureRate float64 `json:"window_failure_rate"`
	SmallTTFTSamples  int     `json:"small_ttft_samples"`
	SmallTTFTP50Ms    int64   `json:"small_ttft_p50_ms"`
	SmallTTFTP90Ms    int64   `json:"small_ttft_p90_ms"`
}

// CodexFastSessionEvent records a pool change.
type CodexFastSessionEvent struct {
	Time   time.Time `json:"time"`
	Action string    `json:"action"`
	Value  string    `json:"value,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// CodexFastSessionRollbackStatus reports the runtime rollback state.
type CodexFastSessionRollbackStatus struct {
	Disabled    bool      `json:"disabled"`
	Reason      string    `json:"reason,omitempty"`
	DisabledAt  time.Time `json:"disabled_at,omitzero"`
	ReenabledAt time.Time `json:"reenabled_at,omitzero"`
}

// CodexFastSessionRollbackCheck reports the last rollback evaluation.
type CodexFastSessionRollbackCheck struct {
	Time    time.Time `json:"time,omitzero"`
	Outcome string    `json:"outcome,omitempty"`
}

// CodexFastSessionSnapshot is a read-only view of the pool for status reporting.
type CodexFastSessionSnapshot struct {
	Enable           bool                                     `json:"enable"`
	Percent          int                                      `json:"percent"`
	EffectivePercent int                                      `json:"effective_percent"`
	Seed             []string                                 `json:"seed"`
	Members          []string                                 `json:"members"`
	Groups           map[string]CodexFastSessionGroupSnapshot `json:"groups"`
	Rollback         CodexFastSessionRollbackStatus           `json:"rollback"`
	RollbackCheck    CodexFastSessionRollbackCheck            `json:"rollback_check"`
	Events           []CodexFastSessionEvent                  `json:"events"`
	Prober           *CodexFastSessionProbeStatus             `json:"prober,omitempty"`
}

var defaultCodexFastSessionPool = NewCodexFastSessionPool()

// NewCodexFastSessionPool returns an empty, disabled pool.
func NewCodexFastSessionPool() *CodexFastSessionPool {
	// seedGen starts at 1 so that 0 can mean "no generation check".
	return &CodexFastSessionPool{nowFunc: time.Now, seedGen: 1, stats: map[string]*codexFastSessionGroupStats{
		CodexFastSessionGroupFast:    {},
		CodexFastSessionGroupControl: {},
	}}
}

// CodexFastSessions returns the process-wide pool used by the Codex executor.
func CodexFastSessions() *CodexFastSessionPool { return defaultCodexFastSessionPool }

func (p *CodexFastSessionPool) now() time.Time {
	if p.nowFunc == nil {
		return time.Now()
	}
	return p.nowFunc()
}

// ApplyConfig syncs enable/percent and, when the configured seed changed since the last call,
// replaces the pool members with the new seed. Members added or removed at runtime survive
// until the seed itself changes. Changes to enable/percent/seed/rollback.enable reset the
// runtime share to the configured cap and start a new evaluation generation.
func (p *CodexFastSessionPool) ApplyConfig(cfg config.CodexFastSessionIDsConfig) {
	if p == nil {
		return
	}
	cfg.Normalize()
	p.mu.RLock()
	unchanged := p.enabled == cfg.Enable && p.percent == cfg.Percent && p.rollbackEnabled == cfg.Rollback.Enable && slices.Equal(p.seed, cfg.Seed)
	p.mu.RUnlock()
	if unchanged {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.enabled == cfg.Enable && p.percent == cfg.Percent && p.rollbackEnabled == cfg.Rollback.Enable && slices.Equal(p.seed, cfg.Seed) {
		return
	}
	p.enabled = cfg.Enable
	p.percent = cfg.Percent
	p.rollbackEnabled = cfg.Rollback.Enable
	// A new mapping configuration starts a fresh evaluation generation. Preserve lifetime totals.
	p.statsMu.Lock()
	p.epoch.Add(1)
	for _, stats := range p.stats {
		stats.buckets = [60]codexFastSessionBucket{}
		stats.decision = [60]codexFastSessionBucket{}
	}
	p.statsMu.Unlock()
	p.lastRollback = CodexFastSessionRollbackCheck{}
	if !slices.Equal(p.seed, cfg.Seed) {
		p.seed = slices.Clone(cfg.Seed)
		p.seedGen++
		p.members = slices.Clone(cfg.Seed)
		log.WithFields(log.Fields{"members": len(p.members)}).Info("codex fast session pool reloaded from seed")
	}
	p.adaptivePercent = p.percentCapLocked()
	p.lastAdjustAt = time.Time{}
}

// Replace sets the pool members.
func (p *CodexFastSessionPool) Replace(members []string) {
	if p == nil {
		return
	}
	members = config.NormalizeCodexFastSessionIDs(members)
	p.mu.Lock()
	p.members = members
	p.mu.Unlock()
}

// SeedGeneration increases each time the pool members are reset from a new seed.
func (p *CodexFastSessionPool) SeedGeneration() uint64 {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.seedGen
}

// Add inserts a member and reports whether the pool changed.
func (p *CodexFastSessionPool) Add(member string) bool {
	return p.addWithLimit(member, 0, "", 0)
}

// addWithLimit inserts a member when the pool has fewer than limit members (0 = unlimited)
// and records an admission event when detail is not empty. A non-zero gen must match the
// current seed generation, so results gathered for an older pool are not applied.
func (p *CodexFastSessionPool) addWithLimit(member string, limit int, detail string, gen uint64) bool {
	if p == nil {
		return false
	}
	normalized := config.NormalizeCodexFastSessionIDs([]string{member})
	if len(normalized) == 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if (gen != 0 && gen != p.seedGen) || slices.Contains(p.members, normalized[0]) || (limit > 0 && len(p.members) >= limit) {
		return false
	}
	p.members = append(slices.Clone(p.members), normalized[0])
	if detail != "" {
		p.addEventLocked(p.now(), "admit", normalized[0], detail)
	}
	return true
}

// Remove deletes a member and reports whether the pool changed.
func (p *CodexFastSessionPool) Remove(member string) bool {
	return p.removeWithDetail(member, "", 0)
}

// removeWithDetail deletes a member; a non-zero gen must match the current seed generation.
func (p *CodexFastSessionPool) removeWithDetail(member string, detail string, gen uint64) bool {
	if p == nil {
		return false
	}
	normalized := config.NormalizeCodexFastSessionIDs([]string{member})
	if len(normalized) == 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if gen != 0 && gen != p.seedGen {
		return false
	}
	idx := slices.Index(p.members, normalized[0])
	if idx < 0 {
		return false
	}
	p.members = slices.Delete(slices.Clone(p.members), idx, idx+1)
	if detail != "" {
		p.addEventLocked(p.now(), "remove", normalized[0], detail)
	}
	return true
}

func (p *CodexFastSessionPool) addEventLocked(now time.Time, action, value, detail string) {
	p.events = append(p.events, CodexFastSessionEvent{Time: now, Action: action, Value: value, Detail: detail})
	if len(p.events) > codexFastSessionMaxEvents {
		p.events = slices.Clone(p.events[len(p.events)-codexFastSessionMaxEvents:])
	}
}

// Members returns a copy of the current pool members.
func (p *CodexFastSessionPool) Members() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Clone(p.members)
}

// Select maps originalID onto a pool member. It returns the original value with group "off"
// when the feature is disabled or rolled back, the pool is empty, or originalID is empty; the
// original value with group "control" when originalID falls outside the canary share;
// otherwise the rendezvous-hashing winner with group "fast".
func (p *CodexFastSessionPool) Select(originalID string) (string, string) {
	value, group, _ := p.selectTracked(originalID)
	return value, group
}

// effectivePercentLocked reserves a stable control cohort while automatic rollback is enabled.
// percentCapLocked is the configured share, capped so that rollback keeps a control cohort.
func (p *CodexFastSessionPool) percentCapLocked() int {
	if p.rollbackEnabled {
		return min(p.percent, 99)
	}
	return p.percent
}

func (p *CodexFastSessionPool) effectivePercentLocked() int {
	if !p.rollbackEnabled {
		return p.percent
	}
	return min(p.adaptivePercent, p.percentCapLocked())
}

func (p *CodexFastSessionPool) selectTracked(originalID string) (string, string, uint64) {
	if p == nil {
		return originalID, CodexFastSessionGroupOff, 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	epoch := p.epoch.Load()
	if originalID == "" || !p.enabled || len(p.members) == 0 {
		return originalID, CodexFastSessionGroupOff, epoch
	}
	// Keep the salted hash: a share change only moves identities on its boundary, and the
	// buckets at or above the cap stay a fixed control cohort.
	bucket := codexFastSessionHash(codexFastSessionCanarySalt+originalID) % 100
	if bucket >= uint64(p.percentCapLocked()) {
		return originalID, CodexFastSessionGroupControl, epoch
	}
	if bucket >= uint64(p.effectivePercentLocked()) {
		return originalID, CodexFastSessionGroupOff, epoch
	}
	return codexFastSessionRendezvous(originalID, p.members), CodexFastSessionGroupFast, epoch
}

// Snapshot returns the pool configuration, members, per-group stats over the last
// windowMinutes minutes, rollback state, recent events, and the prober status.
func (p *CodexFastSessionPool) Snapshot(windowMinutes int) CodexFastSessionSnapshot {
	if p == nil {
		return CodexFastSessionSnapshot{}
	}
	p.mu.RLock()
	out := CodexFastSessionSnapshot{
		Enable:           p.enabled,
		Percent:          p.percent,
		EffectivePercent: p.effectivePercentLocked(),
		Seed:             slices.Clone(p.seed),
		Members:          slices.Clone(p.members),
		Groups:           make(map[string]CodexFastSessionGroupSnapshot, len(p.stats)),
		Rollback:         p.rollback,
		RollbackCheck:    p.lastRollback,
		Events:           slices.Clone(p.events),
	}
	p.mu.RUnlock()
	if out.Seed == nil {
		out.Seed = []string{}
	}
	if out.Members == nil {
		out.Members = []string{}
	}
	if out.Events == nil {
		out.Events = []CodexFastSessionEvent{}
	}
	for _, group := range []string{CodexFastSessionGroupFast, CodexFastSessionGroupControl} {
		out.Groups[group] = p.groupWindow(group, windowMinutes)
	}
	if prober := p.prober.Load(); prober != nil {
		status := prober.Status()
		out.Prober = &status
	}
	return out
}

// Observe records one finished upstream attempt for a group. TTFT is only sampled for
// successful small requests with a measured first token.
func (p *CodexFastSessionPool) Observe(group string, failed bool, ttft time.Duration, inputTokens int64) {
	if p == nil {
		return
	}
	p.observeGeneration(group, failed, ttft, inputTokens, p.epoch.Load())
}

func (p *CodexFastSessionPool) observeGeneration(group string, failed bool, ttft time.Duration, inputTokens int64, epoch uint64) {
	now := p.now()
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	stats, ok := p.stats[group]
	if !ok {
		return
	}
	stats.requests++
	if failed {
		stats.failures++
	}
	// Requests already in flight at a configuration change must not refill the new window.
	if epoch != p.epoch.Load() {
		return
	}
	minute := now.Truncate(time.Minute)
	idx := codexFastSessionBucketIndex(minute)
	d := &stats.decision[idx]
	if !d.minute.Equal(minute) {
		*d = codexFastSessionBucket{minute: minute}
	}
	d.requests++
	if failed {
		d.failures++
	}
	small := ttft > 0 && inputTokens < codexFastSessionSmallInputTokens
	p.observeHourLocked(now, group, failed, small, ttft, epoch)
	b := &stats.buckets[idx]
	if !b.minute.Equal(minute) {
		*b = codexFastSessionBucket{minute: minute}
	}
	b.requests++
	if failed {
		b.failures++
		return
	}
	if small && len(b.ttft) < codexFastSessionBucketSamples {
		b.ttft = append(b.ttft, ttft)
	}
}

// observeHourLocked adds one attempt to the hourly accumulator. statsMu must be held.
func (p *CodexFastSessionPool) observeHourLocked(now time.Time, group string, failed, small bool, ttft time.Duration, epoch uint64) {
	hour := now.Truncate(time.Hour)
	if p.hours == nil {
		p.hours = map[time.Time]*codexFastSessionHourAcc{}
	}
	acc := p.hours[hour]
	if acc == nil || acc.epoch != epoch {
		acc = &codexFastSessionHourAcc{epoch: epoch}
		p.hours[hour] = acc
	}
	g := acc.group(group)
	if g == nil {
		return
	}
	g.Requests++
	if failed {
		g.Failures++
		return
	}
	if small {
		g.Hist.Add(ttft)
	}
}

// RollHours returns the finished hours (before now's hour) and forgets them. Members, share and
// enable reflect the pool at roll time.
func (p *CodexFastSessionPool) RollHours(now time.Time) []codexspeed.FastSessionHour {
	if p == nil {
		return nil
	}
	current := now.Truncate(time.Hour)
	p.statsMu.Lock()
	var done []time.Time
	accs := map[time.Time]*codexFastSessionHourAcc{}
	for hour, acc := range p.hours {
		if hour.Before(current) {
			done = append(done, hour)
			accs[hour] = acc
			delete(p.hours, hour)
		}
	}
	p.statsMu.Unlock()
	slices.SortFunc(done, func(a, b time.Time) int { return a.Compare(b) })
	out := make([]codexspeed.FastSessionHour, 0, len(done))
	for _, hour := range done {
		out = append(out, p.hourRecord(hour, accs[hour]))
	}
	return out
}

// CurrentHour returns the unfinished hour so far.
func (p *CodexFastSessionPool) CurrentHour(now time.Time) codexspeed.FastSessionHour {
	if p == nil {
		return codexspeed.FastSessionHour{}
	}
	hour := now.Truncate(time.Hour)
	p.statsMu.Lock()
	var acc codexFastSessionHourAcc
	if src := p.hours[hour]; src != nil {
		acc.epoch = src.epoch
		acc.fast = cloneGroupHour(src.fast)
		acc.control = cloneGroupHour(src.control)
	}
	p.statsMu.Unlock()
	return p.hourRecord(hour, &acc)
}

func cloneGroupHour(g codexspeed.GroupHour) codexspeed.GroupHour {
	g.Hist = slices.Clone(g.Hist)
	return g
}

func (p *CodexFastSessionPool) hourRecord(hour time.Time, acc *codexFastSessionHourAcc) codexspeed.FastSessionHour {
	rec := codexspeed.FastSessionHour{Hour: hour}
	if acc != nil {
		rec.Epoch = acc.epoch
		rec.Fast = acc.fast
		rec.Control = acc.control
	}
	rec.Fast.Finalize()
	rec.Control.Finalize()
	p.mu.RLock()
	rec.Members = len(p.members)
	rec.EffectivePercent = p.effectivePercentLocked()
	rec.Enabled = p.enabled
	p.mu.RUnlock()
	rec.Label = codexspeed.FastSessionLabel(rec.Fast, rec.Control)
	return rec
}

func codexFastSessionBucketIndex(minute time.Time) int64 {
	idx := (minute.Unix() / 60) % 60
	if idx < 0 {
		idx += 60
	}
	return idx
}

func (p *CodexFastSessionPool) groupWindow(group string, windowMinutes int) CodexFastSessionGroupSnapshot {
	windowMinutes = min(max(windowMinutes, 1), 60)
	out := CodexFastSessionGroupSnapshot{WindowMinutes: windowMinutes}
	now := p.now().Truncate(time.Minute)
	var samples []time.Duration
	p.statsMu.Lock()
	stats, ok := p.stats[group]
	if ok {
		out.TotalRequests = stats.requests
		out.TotalFailures = stats.failures
		for i := 0; i < windowMinutes; i++ {
			minute := now.Add(-time.Duration(i) * time.Minute)
			b := &stats.buckets[codexFastSessionBucketIndex(minute)]
			if !b.minute.Equal(minute) {
				continue
			}
			out.WindowRequests += b.requests
			out.WindowFailures += b.failures
			samples = append(samples, b.ttft...)
		}
	}
	p.statsMu.Unlock()
	if out.WindowRequests > 0 {
		out.WindowFailureRate = float64(out.WindowFailures) / float64(out.WindowRequests)
	}
	out.SmallTTFTSamples = len(samples)
	out.SmallTTFTP50Ms = codexFastSessionPercentile(samples, 50).Milliseconds()
	out.SmallTTFTP90Ms = codexFastSessionPercentile(samples, 90).Milliseconds()
	return out
}

// EvaluateRollback adjusts the runtime fast share. The share halves, down to MinPercent, when
// the fast group's failure rate is clearly worse than the control group's, and otherwise
// climbs back to the configured cap by one step per RecoverIntervalSeconds. Recovery needs no
// proof that the fast group improved: a lowered share may leave too few samples to prove
// anything, so only failure evidence can hold the share down. TTFT is reported, not judged.
func (p *CodexFastSessionPool) EvaluateRollback(cfg config.CodexFastSessionRollbackConfig) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastRollback = CodexFastSessionRollbackCheck{Time: p.now(), Outcome: p.adjustShareLocked(cfg)}
}

// adjustShareLocked requires p.mu. It holds statsMu across reading and clearing the decision
// window, so a request finishing meanwhile lands wholly before or after the clear.
func (p *CodexFastSessionPool) adjustShareLocked(cfg config.CodexFastSessionRollbackConfig) string {
	if !cfg.Enable {
		return "rollback disabled"
	}
	limit := p.percentCapLocked()
	if !p.enabled || limit <= 0 || len(p.members) == 0 {
		return "mapping not active"
	}
	now := p.now()
	sinceAdjust := now.Sub(p.lastAdjustAt)
	if !p.lastAdjustAt.IsZero() && sinceAdjust < time.Duration(cfg.CooldownSeconds)*time.Second {
		return "cooling"
	}
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	fastRequests, fastFailures := p.decisionWindowLocked(CodexFastSessionGroupFast, cfg.WindowMinutes, now)
	controlRequests, controlFailures := p.decisionWindowLocked(CodexFastSessionGroupControl, cfg.WindowMinutes, now)
	minSamples := int64(max(cfg.MinSamples, 1))
	enough := fastRequests >= minSamples && controlRequests >= minSamples
	current := p.effectivePercentLocked()
	if enough {
		fastRate := float64(fastFailures) / float64(fastRequests)
		controlRate := float64(controlFailures) / float64(controlRequests)
		if (fastRate-controlRate)*100 > cfg.FailureWorsePoints {
			message := fmt.Sprintf("fast failure rate %.1f%% vs control %.1f%%", fastRate*100, controlRate*100)
			next := min(current, max(cfg.MinPercent, current/2))
			p.applyShareLocked(now, next, "decrease", message)
			return message
		}
	}
	if current < limit && (p.lastAdjustAt.IsZero() || sinceAdjust >= time.Duration(cfg.RecoverIntervalSeconds)*time.Second) {
		p.applyShareLocked(now, min(limit, current+cfg.IncreaseStepPercent), "increase", "no failure evidence since the last adjustment")
		return "recovering"
	}
	if !enough {
		return "insufficient samples"
	}
	return "ok"
}

// applyShareLocked requires p.mu and statsMu. The epoch stays put: in-flight requests remain
// evidence about the group they were sent in.
func (p *CodexFastSessionPool) applyShareLocked(now time.Time, next int, action, detail string) {
	previous := p.adaptivePercent
	p.adaptivePercent = next
	p.lastAdjustAt = now
	for _, stats := range p.stats {
		stats.decision = [60]codexFastSessionBucket{}
	}
	p.rollback.Reason = detail
	if next == previous {
		return
	}
	p.addEventLocked(now, action, "", fmt.Sprintf("%d%% -> %d%%: %s", previous, next, detail))
	log.WithFields(log.Fields{"from": previous, "to": next, "detail": detail}).Info("codex fast session share adjusted")
}

func (p *CodexFastSessionPool) decisionWindowLocked(group string, windowMinutes int, now time.Time) (requests, failures int64) {
	stats, ok := p.stats[group]
	if !ok {
		return 0, 0
	}
	windowMinutes = min(max(windowMinutes, 1), 60)
	current := now.Truncate(time.Minute)
	for i := 0; i < windowMinutes; i++ {
		minute := current.Add(-time.Duration(i) * time.Minute)
		b := &stats.decision[codexFastSessionBucketIndex(minute)]
		if b.minute.Equal(minute) {
			requests += b.requests
			failures += b.failures
		}
	}
	return requests, failures
}

// SelectCodexFastSession maps originalID with the process-wide pool. Once a prober is attached,
// it alone syncs the pool with the service's current config: executors may hold an older
// config during a background reload, and applying it per request would flip the seed back and
// forth, resetting members and rollback windows each time. Without a prober, cfg is applied.
func SelectCodexFastSession(cfg *config.Config, originalID string) (string, string, uint64) {
	if cfg == nil {
		return originalID, CodexFastSessionGroupOff, 0
	}
	pool := CodexFastSessions()
	pool.ApplyUnmanagedConfig(cfg.Codex.FastSessionIDs)
	return pool.selectTracked(originalID)
}

// ApplyUnmanagedConfig applies cfg only while no prober owns the config sync.
func (p *CodexFastSessionPool) ApplyUnmanagedConfig(cfg config.CodexFastSessionIDsConfig) {
	if p != nil && p.prober.Load() == nil {
		p.ApplyConfig(cfg)
	}
}

// observeCodexFastSessionRecord feeds a published usage record into the group stats, using the
// same filters as the codex pool dashboard.
func observeCodexFastSessionRecord(group string, record usage.Record, generations ...uint64) {
	if record.AdditionalModel || !usage.GenerateEnabled(record.Generate) {
		return
	}
	pool := CodexFastSessions()
	epoch := pool.epoch.Load()
	if len(generations) > 0 {
		epoch = generations[0]
	}
	pool.observeGeneration(group, record.Failed, record.TTFT, record.Detail.InputTokens, epoch)
}

func codexFastSessionRendezvous(originalID string, members []string) string {
	best := ""
	var bestScore uint64
	for _, member := range members {
		score := codexFastSessionHash(codexFastSessionHRWSalt + originalID + "\x00" + member)
		if best == "" || score > bestScore || (score == bestScore && member < best) {
			best = member
			bestScore = score
		}
	}
	return best
}

func codexFastSessionHash(value string) uint64 {
	sum := sha256.Sum256([]byte(value))
	return binary.BigEndian.Uint64(sum[:8])
}

// codexFastSessionPercentile returns the q-th percentile (nearest rank, lower) of samples.
func codexFastSessionPercentile(samples []time.Duration, q int) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	return sorted[(len(sorted)-1)*q/100]
}
