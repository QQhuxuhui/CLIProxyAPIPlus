package helps

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// Codex fast Session-Id groups.
const (
	// CodexFastSessionGroupOff means the request was not considered (feature disabled or
	// rolled back, empty pool, or no original identity) and is not counted in the group stats.
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

	mu           sync.RWMutex
	enabled      bool
	percent      int
	seed         []string
	members      []string
	disabled     bool
	rollback     CodexFastSessionRollbackStatus
	events       []CodexFastSessionEvent
	lastRollback CodexFastSessionRollbackCheck

	statsMu sync.Mutex
	stats   map[string]*codexFastSessionGroupStats

	prober atomic.Pointer[CodexFastSessionProber]
}

type codexFastSessionBucket struct {
	minute   time.Time
	requests int64
	failures int64
	ttft     []time.Duration
}

type codexFastSessionGroupStats struct {
	requests int64
	failures int64
	buckets  [60]codexFastSessionBucket
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
	Enable        bool                                     `json:"enable"`
	Percent       int                                      `json:"percent"`
	Seed          []string                                 `json:"seed"`
	Members       []string                                 `json:"members"`
	Groups        map[string]CodexFastSessionGroupSnapshot `json:"groups"`
	Rollback      CodexFastSessionRollbackStatus           `json:"rollback"`
	RollbackCheck CodexFastSessionRollbackCheck            `json:"rollback_check"`
	Events        []CodexFastSessionEvent                  `json:"events"`
	Prober        *CodexFastSessionProbeStatus             `json:"prober,omitempty"`
}

var defaultCodexFastSessionPool = NewCodexFastSessionPool()

// NewCodexFastSessionPool returns an empty, disabled pool.
func NewCodexFastSessionPool() *CodexFastSessionPool {
	return &CodexFastSessionPool{nowFunc: time.Now, stats: map[string]*codexFastSessionGroupStats{
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
// until the seed itself changes. Any change of enable/percent/seed also clears a runtime
// rollback.
func (p *CodexFastSessionPool) ApplyConfig(cfg config.CodexFastSessionIDsConfig) {
	if p == nil {
		return
	}
	cfg.Normalize()
	p.mu.RLock()
	unchanged := p.enabled == cfg.Enable && p.percent == cfg.Percent && slices.Equal(p.seed, cfg.Seed)
	p.mu.RUnlock()
	if unchanged {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.enabled = cfg.Enable
	p.percent = cfg.Percent
	if !slices.Equal(p.seed, cfg.Seed) {
		p.seed = slices.Clone(cfg.Seed)
		p.members = slices.Clone(cfg.Seed)
		log.WithFields(log.Fields{"members": len(p.members)}).Info("codex fast session pool reloaded from seed")
	}
	if p.disabled {
		now := p.now()
		p.disabled = false
		p.rollback.Disabled = false
		p.rollback.ReenabledAt = now
		p.addEventLocked(now, "reenable", "", "config changed")
		log.Info("codex fast session pool re-enabled after config change")
	}
}

// Disable turns the mapping off at runtime until enable/percent/seed change in the config.
func (p *CodexFastSessionPool) Disable(reason string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disabled {
		return
	}
	now := p.now()
	p.disabled = true
	p.rollback = CodexFastSessionRollbackStatus{Disabled: true, Reason: reason, DisabledAt: now, ReenabledAt: p.rollback.ReenabledAt}
	p.addEventLocked(now, "disable", "", reason)
	log.WithField("reason", reason).Warn("codex fast session pool disabled by rollback")
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

// Add inserts a member and reports whether the pool changed.
func (p *CodexFastSessionPool) Add(member string) bool {
	return p.addWithLimit(member, 0, "")
}

// addWithLimit inserts a member when the pool has fewer than limit members (0 = unlimited)
// and records an admission event when detail is not empty.
func (p *CodexFastSessionPool) addWithLimit(member string, limit int, detail string) bool {
	if p == nil {
		return false
	}
	normalized := config.NormalizeCodexFastSessionIDs([]string{member})
	if len(normalized) == 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if slices.Contains(p.members, normalized[0]) || (limit > 0 && len(p.members) >= limit) {
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
	return p.removeWithDetail(member, "")
}

func (p *CodexFastSessionPool) removeWithDetail(member string, detail string) bool {
	if p == nil {
		return false
	}
	normalized := config.NormalizeCodexFastSessionIDs([]string{member})
	if len(normalized) == 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
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
	if p == nil || originalID == "" {
		return originalID, CodexFastSessionGroupOff
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.enabled || p.disabled || len(p.members) == 0 {
		return originalID, CodexFastSessionGroupOff
	}
	if codexFastSessionHash(codexFastSessionCanarySalt+originalID)%100 >= uint64(p.percent) {
		return originalID, CodexFastSessionGroupControl
	}
	return codexFastSessionRendezvous(originalID, p.members), CodexFastSessionGroupFast
}

// Snapshot returns the pool configuration, members, per-group stats over the last
// windowMinutes minutes, rollback state, recent events, and the prober status.
func (p *CodexFastSessionPool) Snapshot(windowMinutes int) CodexFastSessionSnapshot {
	if p == nil {
		return CodexFastSessionSnapshot{}
	}
	p.mu.RLock()
	out := CodexFastSessionSnapshot{
		Enable:        p.enabled,
		Percent:       p.percent,
		Seed:          slices.Clone(p.seed),
		Members:       slices.Clone(p.members),
		Groups:        make(map[string]CodexFastSessionGroupSnapshot, len(p.stats)),
		Rollback:      p.rollback,
		RollbackCheck: p.lastRollback,
		Events:        slices.Clone(p.events),
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
	now := p.now()
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	stats, ok := p.stats[group]
	if !ok {
		return
	}
	minute := now.Truncate(time.Minute)
	idx := (minute.Unix() / 60) % 60
	if idx < 0 {
		idx += 60
	}
	b := &stats.buckets[idx]
	if !b.minute.Equal(minute) {
		*b = codexFastSessionBucket{minute: minute}
	}
	stats.requests++
	b.requests++
	if failed {
		stats.failures++
		b.failures++
		return
	}
	if ttft > 0 && inputTokens < codexFastSessionSmallInputTokens && len(b.ttft) < codexFastSessionBucketSamples {
		b.ttft = append(b.ttft, ttft)
	}
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
			idx := (minute.Unix() / 60) % 60
			if idx < 0 {
				idx += 60
			}
			b := &stats.buckets[idx]
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

// EvaluateRollback compares the fast and control groups over the rollback window and
// disables the pool when the fast group is worse by more than the configured margins. It only
// reads in-memory stats.
func (p *CodexFastSessionPool) EvaluateRollback(cfg config.CodexFastSessionRollbackConfig) {
	if p == nil {
		return
	}
	outcome := p.rollbackOutcome(cfg)
	p.mu.Lock()
	p.lastRollback = CodexFastSessionRollbackCheck{Time: p.now(), Outcome: outcome.message}
	p.mu.Unlock()
	if outcome.trigger {
		p.Disable(outcome.message)
	}
}

type codexFastSessionRollbackOutcome struct {
	trigger bool
	message string
}

func (p *CodexFastSessionPool) rollbackOutcome(cfg config.CodexFastSessionRollbackConfig) codexFastSessionRollbackOutcome {
	if !cfg.Enable {
		return codexFastSessionRollbackOutcome{message: "rollback disabled"}
	}
	p.mu.RLock()
	active := p.enabled && !p.disabled
	p.mu.RUnlock()
	if !active {
		return codexFastSessionRollbackOutcome{message: "mapping not active"}
	}
	fast := p.groupWindow(CodexFastSessionGroupFast, cfg.WindowMinutes)
	control := p.groupWindow(CodexFastSessionGroupControl, cfg.WindowMinutes)
	minSamples := int64(max(cfg.MinSamples, 1))
	if fast.WindowRequests < minSamples || control.WindowRequests < minSamples {
		return codexFastSessionRollbackOutcome{message: "insufficient samples"}
	}
	if diff := (fast.WindowFailureRate - control.WindowFailureRate) * 100; diff > cfg.FailureWorsePoints {
		return codexFastSessionRollbackOutcome{trigger: true, message: fmt.Sprintf("fast failure rate %.1f%% vs control %.1f%%", fast.WindowFailureRate*100, control.WindowFailureRate*100)}
	}
	if int64(fast.SmallTTFTSamples) < minSamples || int64(control.SmallTTFTSamples) < minSamples {
		return codexFastSessionRollbackOutcome{message: "failure rate ok; insufficient ttft samples"}
	}
	if float64(fast.SmallTTFTP50Ms) > float64(control.SmallTTFTP50Ms)*(1+cfg.TTFTWorsePercent/100) {
		return codexFastSessionRollbackOutcome{trigger: true, message: fmt.Sprintf("fast small-request ttft p50 %dms vs control %dms", fast.SmallTTFTP50Ms, control.SmallTTFTP50Ms)}
	}
	return codexFastSessionRollbackOutcome{message: "ok"}
}

// SelectCodexFastSession syncs the process-wide pool with cfg and maps originalID.
func SelectCodexFastSession(cfg *config.Config, originalID string) (string, string) {
	if cfg == nil {
		return originalID, CodexFastSessionGroupOff
	}
	pool := CodexFastSessions()
	pool.ApplyConfig(cfg.Codex.FastSessionIDs)
	return pool.Select(originalID)
}

// observeCodexFastSessionRecord feeds a published usage record into the group stats, using the
// same filters as the codex pool dashboard.
func observeCodexFastSessionRecord(group string, record usage.Record) {
	if record.AdditionalModel || !usage.GenerateEnabled(record.Generate) {
		return
	}
	CodexFastSessions().Observe(group, record.Failed, record.TTFT, record.Detail.InputTokens)
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
