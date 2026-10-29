package helps

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"crypto/sha256"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// Codex egress groups tag each routed attempt for the rollback comparison.
const (
	// CodexEgressGroupFirstDirect is a first attempt on the direct route.
	CodexEgressGroupFirstDirect = "first_direct"
	// CodexEgressGroupShiftPool is a first attempt shifted to the pool during a direct overload.
	CodexEgressGroupShiftPool = "shift_pool"
	// CodexEgressGroupRetryPool is a retry routed through the pool.
	CodexEgressGroupRetryPool = "retry_pool"
	// CodexEgressGroupRetryControl is a retry kept direct as the control group.
	CodexEgressGroupRetryControl = "retry_control"
	// CodexEgressGroupRetryFallback is a retry that wanted the pool but found no usable proxy,
	// or ran while the pool was rolled back.
	CodexEgressGroupRetryFallback = "retry_fallback"

	// CodexEgressRouteDirect is the route key of the direct route.
	CodexEgressRouteDirect = "direct"

	codexEgressHistoryMinutes = 60
	codexEgressBucketSamples  = 512
	codexEgressMaxEvents      = 50
	// codexEgressSmallInputTokens bounds the "small request" TTFT samples used for rollback.
	codexEgressSmallInputTokens = 20000
)

// CodexEgressPool routes Codex attempts across outbound proxies and keeps the per-route stats.
type CodexEgressPool struct {
	// managed is set once a service-level sync owns applying the config; executors may hold an
	// older config during a background reload and must not flip the pool back.
	managed atomic.Bool

	mu     sync.Mutex
	now    func() time.Time
	random func() float64

	cfg     config.CodexEgressPoolConfig
	cfgKey  string
	proxies []*codexEgressProxy

	routes map[string]*codexEgressSeries
	groups map[string]*codexEgressSeries

	shiftPercent   int
	shiftCheckedAt time.Time
	shiftMessage   string

	// compareSince starts the rollback comparison window; it moves on (re)enable so samples
	// from a rolled-back period never decide the next one.
	compareSince      time.Time
	disabledUntil     time.Time
	trial             bool
	rollbackCheckedAt time.Time
	rollbackMessage   string

	events []CodexEgressEvent
}

type codexEgressProxy struct {
	name     string
	rawURL   string
	masked   string
	hostPort string
	maxConc  int
	disabled bool

	inflight      int
	cooldownUntil time.Time
	failStreak    int
	lastFailure   string
	lastFailureAt time.Time

	transport        http.RoundTripper
	transportTimeout time.Duration
}

type codexEgressSeries struct {
	buckets [codexEgressHistoryMinutes]codexEgressBucket
}

type codexEgressBucket struct {
	minute    int64
	attempts  int
	success   int
	overload  int
	other     int
	connFail  int
	conn429   int
	ttft      []time.Duration
	ttftSmall []time.Duration
}

// CodexEgressEvent is one state change shown on the management page.
type CodexEgressEvent struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}

var (
	codexEgressPoolOnce sync.Once
	codexEgressPool     *CodexEgressPool
)

// CodexEgressPools returns the process-wide egress pool.
func CodexEgressPools() *CodexEgressPool {
	codexEgressPoolOnce.Do(func() {
		codexEgressPool = newCodexEgressPool(time.Now, rand.Float64)
	})
	return codexEgressPool
}

func newCodexEgressPool(now func() time.Time, random func() float64) *CodexEgressPool {
	return &CodexEgressPool{
		now:    now,
		random: random,
		routes: make(map[string]*codexEgressSeries),
		groups: make(map[string]*codexEgressSeries),
	}
}

// ApplyConfig syncs the pool with cfg. Proxy state (in-flight, cooldown) survives reloads for
// URLs that remain configured.
func (p *CodexEgressPool) ApplyConfig(cfg config.CodexEgressPoolConfig) {
	if p == nil {
		return
	}
	cfg.Normalize()
	raw, _ := json.Marshal(cfg)
	key := string(raw)
	p.mu.Lock()
	defer p.mu.Unlock()
	if key == p.cfgKey {
		return
	}
	wasEnabled := p.cfg.Enable
	p.cfg = cfg
	p.cfgKey = key
	old := make(map[string]*codexEgressProxy, len(p.proxies))
	for _, px := range p.proxies {
		old[px.rawURL] = px
	}
	timeout := time.Duration(cfg.ConnectTimeoutSeconds) * time.Second
	proxies := make([]*codexEgressProxy, 0, len(cfg.Proxies))
	for _, entry := range cfg.Proxies {
		px := old[entry.URL]
		if px == nil {
			px = &codexEgressProxy{rawURL: entry.URL, masked: MaskCodexEgressProxyURL(entry.URL), hostPort: codexEgressHostPort(entry.URL)}
		}
		if px.transport == nil || px.transportTimeout != timeout {
			rt := newUtlsRoundTripper(entry.URL)
			rt.connectTimeout = timeout
			px.transport, px.transportTimeout = rt, timeout
		}
		px.name = entry.Name
		if px.name == "" {
			px.name = px.hostPort
		}
		px.maxConc = entry.MaxConcurrency
		px.disabled = entry.Disabled
		proxies = append(proxies, px)
	}
	p.proxies = proxies
	now := p.now()
	if !cfg.Enable {
		p.shiftPercent = 0
		p.disabledUntil = time.Time{}
		p.trial = false
	}
	if cfg.Enable && !wasEnabled {
		p.compareSince = now
	}
	if !cfg.Shift.Enable {
		p.shiftPercent = 0
	}
	p.addEventLocked(now, "config", fmt.Sprintf("config applied: enable=%v proxies=%d", cfg.Enable, len(proxies)))
}

// ApplyUnmanagedConfig applies cfg only while no service-level sync owns the config.
func (p *CodexEgressPool) ApplyUnmanagedConfig(cfg config.CodexEgressPoolConfig) {
	if p != nil && !p.managed.Load() {
		p.ApplyConfig(cfg)
	}
}

// Active reports whether the pool is enabled with at least one usable proxy entry.
func (p *CodexEgressPool) Active() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.activeLocked()
}

// StartCodexEgressPoolSync makes current() the only config source of the process-wide pool and
// re-applies it every 10 seconds until ctx is done.
func StartCodexEgressPoolSync(ctx context.Context, current func() config.CodexEgressPoolConfig) {
	pool := CodexEgressPools()
	if current == nil {
		return
	}
	pool.ApplyConfig(current())
	pool.managed.Store(true)
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pool.ApplyConfig(current())
			}
		}
	}()
}

// codexEgressDecision is the route chosen for one attempt.
type codexEgressDecision struct {
	group string
	proxy *codexEgressProxy
	tried map[*codexEgressProxy]struct{}
}

// selectRoute picks the route of one attempt. authKey keeps a credential on the same proxy.
func (p *CodexEgressPool) selectRoute(authKey string, retry bool) codexEgressDecision {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.activeLocked() {
		return codexEgressDecision{}
	}
	now := p.now()
	p.tickLocked(now)
	poolAllowed := !now.Before(p.disabledUntil)
	d := codexEgressDecision{tried: make(map[*codexEgressProxy]struct{})}
	if retry {
		switch {
		case !poolAllowed:
			d.group = CodexEgressGroupRetryFallback
		case p.random()*100 < float64(p.cfg.ControlPercent):
			d.group = CodexEgressGroupRetryControl
		default:
			d.proxy = p.pickLocked(authKey, d.tried, now)
			d.group = CodexEgressGroupRetryPool
			if d.proxy == nil {
				d.group = CodexEgressGroupRetryFallback
			}
		}
		return d
	}
	d.group = CodexEgressGroupFirstDirect
	if poolAllowed && !p.trial && p.shiftPercent > 0 && p.random()*100 < float64(p.shiftPercent) {
		if px := p.pickLocked(authKey, d.tried, now); px != nil {
			d.proxy = px
			d.group = CodexEgressGroupShiftPool
		}
	}
	return d
}

// reselect releases the failed proxy and picks another one, or the direct route.
func (p *CodexEgressPool) reselect(d codexEgressDecision, authKey string, connectErr error) codexEgressDecision {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if d.proxy != nil {
		d.tried[d.proxy] = struct{}{}
		p.releaseLocked(d.proxy)
		p.markConnectFailureLocked(d.proxy, connectErr, now)
	}
	d.proxy = p.pickLocked(authKey, d.tried, now)
	return d
}

func (p *CodexEgressPool) activeLocked() bool {
	if !p.cfg.Enable {
		return false
	}
	for _, px := range p.proxies {
		if !px.disabled {
			return true
		}
	}
	return false
}

// pickLocked returns the credential's preferred usable proxy (rendezvous order) and counts it
// in flight, or nil when none is usable.
func (p *CodexEgressPool) pickLocked(authKey string, tried map[*codexEgressProxy]struct{}, now time.Time) *codexEgressProxy {
	var best *codexEgressProxy
	var bestScore uint64
	for _, px := range p.proxies {
		if px.disabled || px.inflight >= px.maxConc || now.Before(px.cooldownUntil) {
			continue
		}
		if _, skip := tried[px]; skip {
			continue
		}
		score := codexEgressHash(authKey + "\x00" + px.rawURL)
		if best == nil || score > bestScore {
			best, bestScore = px, score
		}
	}
	if best != nil {
		best.inflight++
	}
	return best
}

func (p *CodexEgressPool) release(px *codexEgressProxy) {
	if px == nil {
		return
	}
	p.mu.Lock()
	p.releaseLocked(px)
	p.mu.Unlock()
}

func (p *CodexEgressPool) releaseLocked(px *codexEgressProxy) {
	if px.inflight > 0 {
		px.inflight--
	}
}

func (p *CodexEgressPool) markConnectSuccess(px *codexEgressProxy) {
	if px == nil {
		return
	}
	p.mu.Lock()
	px.failStreak = 0
	p.mu.Unlock()
}

func (p *CodexEgressPool) markConnectFailureLocked(px *codexEgressProxy, err error, now time.Time) {
	px.failStreak++
	cooldown := time.Duration(p.cfg.CooldownBaseSeconds) * time.Second
	for i := 1; i < px.failStreak && cooldown < time.Duration(p.cfg.CooldownMaxSeconds)*time.Second; i++ {
		cooldown *= 2
	}
	cooldown = min(cooldown, time.Duration(p.cfg.CooldownMaxSeconds)*time.Second)
	px.cooldownUntil = now.Add(cooldown)
	reason := codexEgressSafeError(err, px)
	px.lastFailure = reason
	px.lastFailureAt = now
	b := p.bucketLocked(p.routes, px.rawURL, now)
	b.connFail++
	if strings.Contains(reason, "429") {
		b.conn429++
	}
	p.addEventLocked(now, "proxy_cooldown", fmt.Sprintf("%s cooling %s after: %s", px.name, cooldown, reason))
}

// observe records the outcome of one routed attempt.
func (p *CodexEgressPool) observe(group, route string, record usage.Record) {
	if p == nil || group == "" || record.AdditionalModel || !usage.GenerateEnabled(record.Generate) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, b := range []*codexEgressBucket{p.bucketLocked(p.routes, route, now), p.bucketLocked(p.groups, group, now)} {
		b.attempts++
		switch {
		case !record.Failed:
			b.success++
			if record.TTFT > 0 && len(b.ttft) < codexEgressBucketSamples {
				b.ttft = append(b.ttft, record.TTFT)
				if record.Detail.InputTokens < codexEgressSmallInputTokens {
					b.ttftSmall = append(b.ttftSmall, record.TTFT)
				}
			}
		case codexEgressIsOverload(record.Fail):
			b.overload++
		default:
			b.other++
		}
	}
}

func codexEgressIsOverload(fail usage.Failure) bool {
	if fail.StatusCode == 502 || fail.StatusCode == 503 {
		return true
	}
	return strings.Contains(strings.ToLower(fail.Body), "server_is_overloaded")
}

func (p *CodexEgressPool) bucketLocked(series map[string]*codexEgressSeries, key string, now time.Time) *codexEgressBucket {
	s := series[key]
	if s == nil {
		s = &codexEgressSeries{}
		series[key] = s
	}
	minute := now.Unix() / 60
	b := &s.buckets[minute%codexEgressHistoryMinutes]
	if b.minute != minute {
		*b = codexEgressBucket{minute: minute}
	}
	return b
}

// codexEgressWindow aggregates buckets of one series between since and now.
type codexEgressWindow struct {
	Attempts  int `json:"attempts"`
	Success   int `json:"success"`
	Overload  int `json:"overload"`
	Other     int `json:"other"`
	ConnFail  int `json:"connect_failures"`
	Conn429   int `json:"connect_429"`
	ttft      []time.Duration
	ttftSmall []time.Duration
}

func (w codexEgressWindow) failureRate() float64 {
	if w.Attempts == 0 {
		return 0
	}
	return float64(w.Overload+w.Other) / float64(w.Attempts)
}

func (w codexEgressWindow) overloadRate() float64 {
	if w.Attempts == 0 {
		return 0
	}
	return float64(w.Overload) / float64(w.Attempts)
}

func (p *CodexEgressPool) windowLocked(series map[string]*codexEgressSeries, key string, since, now time.Time) codexEgressWindow {
	var w codexEgressWindow
	s := series[key]
	if s == nil {
		return w
	}
	from, to := since.Unix()/60, now.Unix()/60
	for i := range s.buckets {
		b := &s.buckets[i]
		if b.minute < from || b.minute > to || to-b.minute >= codexEgressHistoryMinutes {
			continue
		}
		w.Attempts += b.attempts
		w.Success += b.success
		w.Overload += b.overload
		w.Other += b.other
		w.ConnFail += b.connFail
		w.Conn429 += b.conn429
		w.ttft = append(w.ttft, b.ttft...)
		w.ttftSmall = append(w.ttftSmall, b.ttftSmall...)
	}
	return w
}

// tickLocked runs the once-a-minute rollback check and shift controller.
func (p *CodexEgressPool) tickLocked(now time.Time) {
	// A finished rollback period turns into a retry-only trial.
	if !p.disabledUntil.IsZero() && !now.Before(p.disabledUntil) && !p.trial {
		p.trial = true
		p.compareSince = now
		p.disabledUntil = time.Time{}
		p.addEventLocked(now, "trial", "rollback period over; retry-only trial started")
	}
	if p.cfg.Rollback.Enable && now.Sub(p.rollbackCheckedAt) >= time.Minute && !now.Before(p.disabledUntil) {
		p.rollbackCheckedAt = now
		p.checkRollbackLocked(now)
	}
	if now.Sub(p.shiftCheckedAt) >= time.Minute {
		p.shiftCheckedAt = now
		p.adjustShiftLocked(now)
	}
}

func (p *CodexEgressPool) rollbackSinceLocked(now time.Time) time.Time {
	since := now.Add(-time.Duration(p.cfg.Rollback.WindowMinutes) * time.Minute)
	if p.compareSince.After(since) {
		since = p.compareSince
	}
	return since
}

// compareLocked compares a pool group against its direct group. It returns a non-empty reason
// when the pool should roll back, and whether there were enough samples to judge.
func (p *CodexEgressPool) compareLocked(poolGroup, directGroup string, since, now time.Time) (string, bool) {
	rb := p.cfg.Rollback
	poolW := p.windowLocked(p.groups, poolGroup, since, now)
	directW := p.windowLocked(p.groups, directGroup, since, now)
	if poolW.Attempts < rb.MinSamples || directW.Attempts < rb.MinSamples {
		return "", false
	}
	if (directW.failureRate()-poolW.failureRate())*100 < rb.FailureBetterPoints {
		return fmt.Sprintf("%s failure %.1f%% vs %s %.1f%% (needs %.0f points better)", poolGroup, poolW.failureRate()*100, directGroup, directW.failureRate()*100, rb.FailureBetterPoints), true
	}
	if len(poolW.ttftSmall) >= rb.MinSamples && len(directW.ttftSmall) >= rb.MinSamples {
		pp, dp := codexEgressPercentile(poolW.ttftSmall, 50), codexEgressPercentile(directW.ttftSmall, 50)
		if float64(pp) > float64(dp)*(1+rb.TTFTWorsePercent/100) {
			return fmt.Sprintf("%s small-request ttft p50 %dms vs %s %dms", poolGroup, pp.Milliseconds(), directGroup, dp.Milliseconds()), true
		}
	}
	return "", true
}

func (p *CodexEgressPool) checkRollbackLocked(now time.Time) {
	since := p.rollbackSinceLocked(now)
	judged := false
	for _, pair := range [][2]string{{CodexEgressGroupRetryPool, CodexEgressGroupRetryControl}, {CodexEgressGroupShiftPool, CodexEgressGroupFirstDirect}} {
		reason, ok := p.compareLocked(pair[0], pair[1], since, now)
		judged = judged || ok
		if reason != "" {
			p.disabledUntil = now.Add(time.Duration(p.cfg.Rollback.RetryMinutes) * time.Minute)
			p.trial = false
			p.shiftPercent = 0
			p.rollbackMessage = reason
			p.addEventLocked(now, "rollback", "pool disabled: "+reason)
			return
		}
	}
	if !judged {
		p.rollbackMessage = "insufficient samples"
		return
	}
	p.rollbackMessage = "ok"
	if p.trial {
		p.trial = false
		p.addEventLocked(now, "trial", "trial passed; pool fully enabled")
	}
}

func (p *CodexEgressPool) adjustShiftLocked(now time.Time) {
	sh := p.cfg.Shift
	if !sh.Enable || p.trial || now.Before(p.disabledUntil) {
		p.shiftPercent = 0
		return
	}
	w := p.windowLocked(p.routes, CodexEgressRouteDirect, now.Add(-time.Duration(sh.WindowMinutes)*time.Minute), now)
	if w.Attempts < sh.MinSamples {
		p.shiftMessage = fmt.Sprintf("direct samples %d < %d", w.Attempts, sh.MinSamples)
		return
	}
	rate := w.overloadRate() * 100
	prev := p.shiftPercent
	switch {
	case rate > sh.EnterOverloadPercent:
		if p.shiftPercent == 0 {
			p.shiftPercent = sh.StartPercent
		} else {
			p.shiftPercent = min(p.shiftPercent+sh.StepPercent, sh.MaxPercent)
		}
	case rate < sh.ExitOverloadPercent:
		p.shiftPercent = max(p.shiftPercent-sh.StepPercent, 0)
	}
	p.shiftMessage = fmt.Sprintf("direct overload %.1f%% over %d attempts", rate, w.Attempts)
	if p.shiftPercent != prev {
		p.addEventLocked(now, "shift", fmt.Sprintf("shift %d%% -> %d%% (%s)", prev, p.shiftPercent, p.shiftMessage))
	}
}

func (p *CodexEgressPool) addEventLocked(now time.Time, kind, message string) {
	p.events = append(p.events, CodexEgressEvent{Time: now, Kind: kind, Message: message})
	if len(p.events) > codexEgressMaxEvents {
		p.events = slices.Clone(p.events[len(p.events)-codexEgressMaxEvents:])
	}
}

// CodexEgressSnapshot is the management view of the pool.
type CodexEgressSnapshot struct {
	Enable          bool                         `json:"enable"`
	Config          config.CodexEgressPoolConfig `json:"config"`
	ShiftPercent    int                          `json:"shift_percent"`
	ShiftMessage    string                       `json:"shift_message"`
	DisabledUntil   *time.Time                   `json:"disabled_until,omitempty"`
	Trial           bool                         `json:"trial"`
	RollbackMessage string                       `json:"rollback_message"`
	CompareSince    time.Time                    `json:"compare_since"`
	Proxies         []CodexEgressProxyStatus     `json:"proxies"`
	Routes          []CodexEgressRouteSeries     `json:"routes"`
	Groups          map[string]CodexEgressStats  `json:"groups"`
	Events          []CodexEgressEvent           `json:"events"`
}

// CodexEgressProxyStatus is the live state of one proxy.
type CodexEgressProxyStatus struct {
	Index           int        `json:"index"`
	Name            string     `json:"name"`
	URL             string     `json:"url"`
	HostPort        string     `json:"host_port"`
	MaxConcurrency  int        `json:"max_concurrency"`
	Disabled        bool       `json:"disabled"`
	InFlight        int        `json:"in_flight"`
	Status          string     `json:"status"`
	CooldownUntil   *time.Time `json:"cooldown_until,omitempty"`
	FailStreak      int        `json:"fail_streak"`
	LastFailure     string     `json:"last_failure,omitempty"`
	LastFailureTime *time.Time `json:"last_failure_time,omitempty"`
}

// CodexEgressRouteSeries is the per-minute history of one route.
type CodexEgressRouteSeries struct {
	Key    string             `json:"key"`
	Name   string             `json:"name"`
	Points []CodexEgressPoint `json:"points"`
}

// CodexEgressPoint is one minute of one route.
type CodexEgressPoint struct {
	Time      time.Time `json:"time"`
	Attempts  int       `json:"attempts"`
	Success   int       `json:"success"`
	Overload  int       `json:"overload"`
	Other     int       `json:"other"`
	ConnFail  int       `json:"connect_failures"`
	Conn429   int       `json:"connect_429"`
	TTFTP50Ms int64     `json:"ttft_p50_ms"`
	TTFTP90Ms int64     `json:"ttft_p90_ms"`
}

// CodexEgressStats summarizes one group over the rollback window.
type CodexEgressStats struct {
	Attempts          int     `json:"attempts"`
	FailureRate       float64 `json:"failure_rate"`
	OverloadRate      float64 `json:"overload_rate"`
	SmallTTFTSamples  int     `json:"small_ttft_samples"`
	SmallTTFTP50Ms    int64   `json:"small_ttft_p50_ms"`
	SmallTTFTP90Ms    int64   `json:"small_ttft_p90_ms"`
	TTFTP50Ms         int64   `json:"ttft_p50_ms"`
	TTFTP90Ms         int64   `json:"ttft_p90_ms"`
	WindowStartedTime string  `json:"window_started"`
}

// Snapshot returns the management view with proxy credentials masked.
func (p *CodexEgressPool) Snapshot() CodexEgressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if p.activeLocked() {
		p.tickLocked(now)
	}
	out := CodexEgressSnapshot{
		Enable:          p.cfg.Enable,
		Config:          MaskCodexEgressConfig(p.cfg),
		ShiftPercent:    p.shiftPercent,
		ShiftMessage:    p.shiftMessage,
		Trial:           p.trial,
		RollbackMessage: p.rollbackMessage,
		CompareSince:    p.compareSince,
		Groups:          make(map[string]CodexEgressStats),
		Events:          slices.Clone(p.events),
	}
	if now.Before(p.disabledUntil) {
		t := p.disabledUntil
		out.DisabledUntil = &t
	}
	for i, px := range p.proxies {
		st := CodexEgressProxyStatus{Index: i, Name: px.name, URL: px.masked, HostPort: px.hostPort, MaxConcurrency: px.maxConc, Disabled: px.disabled, InFlight: px.inflight, FailStreak: px.failStreak, LastFailure: px.lastFailure}
		switch {
		case px.disabled:
			st.Status = "disabled"
		case now.Before(px.cooldownUntil):
			st.Status = "cooling"
			t := px.cooldownUntil
			st.CooldownUntil = &t
		case px.inflight >= px.maxConc:
			st.Status = "full"
		default:
			st.Status = "ok"
		}
		if !px.lastFailureAt.IsZero() {
			t := px.lastFailureAt
			st.LastFailureTime = &t
		}
		out.Proxies = append(out.Proxies, st)
	}
	out.Routes = append(out.Routes, p.routeSeriesLocked(CodexEgressRouteDirect, "直连", now))
	for _, px := range p.proxies {
		out.Routes = append(out.Routes, p.routeSeriesLocked(px.rawURL, px.name, now))
	}
	since := p.rollbackSinceLocked(now)
	for _, g := range []string{CodexEgressGroupFirstDirect, CodexEgressGroupShiftPool, CodexEgressGroupRetryPool, CodexEgressGroupRetryControl, CodexEgressGroupRetryFallback} {
		w := p.windowLocked(p.groups, g, since, now)
		out.Groups[g] = CodexEgressStats{
			Attempts:          w.Attempts,
			FailureRate:       w.failureRate(),
			OverloadRate:      w.overloadRate(),
			SmallTTFTSamples:  len(w.ttftSmall),
			SmallTTFTP50Ms:    codexEgressPercentile(w.ttftSmall, 50).Milliseconds(),
			SmallTTFTP90Ms:    codexEgressPercentile(w.ttftSmall, 90).Milliseconds(),
			TTFTP50Ms:         codexEgressPercentile(w.ttft, 50).Milliseconds(),
			TTFTP90Ms:         codexEgressPercentile(w.ttft, 90).Milliseconds(),
			WindowStartedTime: since.Format(time.RFC3339),
		}
	}
	return out
}

func (p *CodexEgressPool) routeSeriesLocked(key, name string, now time.Time) CodexEgressRouteSeries {
	out := CodexEgressRouteSeries{Key: name, Name: name}
	if key == CodexEgressRouteDirect {
		out.Key = CodexEgressRouteDirect
	} else {
		out.Key = codexEgressHostPort(key)
	}
	s := p.routes[key]
	current := now.Unix() / 60
	for m := current - codexEgressHistoryMinutes + 1; m <= current; m++ {
		pt := CodexEgressPoint{Time: time.Unix(m*60, 0)}
		if s != nil {
			if b := &s.buckets[m%codexEgressHistoryMinutes]; b.minute == m {
				pt.Attempts, pt.Success, pt.Overload, pt.Other, pt.ConnFail, pt.Conn429 = b.attempts, b.success, b.overload, b.other, b.connFail, b.conn429
				pt.TTFTP50Ms = codexEgressPercentile(b.ttft, 50).Milliseconds()
				pt.TTFTP90Ms = codexEgressPercentile(b.ttft, 90).Milliseconds()
			}
		}
		out.Points = append(out.Points, pt)
	}
	return out
}

// RawProxyURL returns the unmasked URL of the configured proxy at index.
func (p *CodexEgressPool) RawProxyURL(index int) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < 0 || index >= len(p.proxies) {
		return "", false
	}
	return p.proxies[index].rawURL, true
}

// MaskCodexEgressProxyURL hides the proxy password, keeping the user name.
func MaskCodexEgressProxyURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "<invalid proxy URL>"
	}
	if u.User == nil {
		return u.String()
	}
	user := u.User.Username()
	u.User = nil
	rest := u.String()
	if i := strings.Index(rest, "://"); i >= 0 {
		return rest[:i+3] + user + ":***@" + rest[i+3:]
	}
	return rest
}

// MaskCodexEgressConfig returns cfg with every proxy password masked.
func MaskCodexEgressConfig(cfg config.CodexEgressPoolConfig) config.CodexEgressPoolConfig {
	out := cfg
	out.Proxies = slices.Clone(cfg.Proxies)
	for i := range out.Proxies {
		out.Proxies[i].URL = MaskCodexEgressProxyURL(out.Proxies[i].URL)
	}
	return out
}

// UnmaskCodexEgressConfig restores masked proxy passwords in next from current, matching on the
// masked URL. Entries whose masked URL matches nothing keep their value as given.
func UnmaskCodexEgressConfig(next, current config.CodexEgressPoolConfig) config.CodexEgressPoolConfig {
	byMasked := make(map[string]string, len(current.Proxies))
	for _, p := range current.Proxies {
		byMasked[MaskCodexEgressProxyURL(p.URL)] = p.URL
	}
	next.Proxies = slices.Clone(next.Proxies)
	for i := range next.Proxies {
		if raw, ok := byMasked[strings.TrimSpace(next.Proxies[i].URL)]; ok && strings.Contains(next.Proxies[i].URL, ":***@") {
			next.Proxies[i].URL = raw
		}
	}
	return next
}

func codexEgressHostPort(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "<invalid proxy URL>"
	}
	return u.Host
}

// codexEgressSafeError renders a proxy failure without the proxy credentials.
func codexEgressSafeError(err error, px *codexEgressProxy) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if px != nil {
		msg = strings.ReplaceAll(msg, px.rawURL, px.masked)
		if u, errParse := url.Parse(px.rawURL); errParse == nil && u.User != nil {
			if pw, ok := u.User.Password(); ok && pw != "" {
				msg = strings.ReplaceAll(msg, pw, "***")
			}
		}
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}

func codexEgressHash(value string) uint64 {
	sum := sha256.Sum256([]byte(value))
	return binary.BigEndian.Uint64(sum[:8])
}

// codexEgressPercentile returns the q-th percentile (nearest rank, lower) of samples.
func codexEgressPercentile(samples []time.Duration, q int) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	return sorted[(len(sorted)-1)*q/100]
}

// errCodexEgressConnect marks a failure before the request reached the upstream through a proxy.
type codexEgressConnectError struct{ err error }

func (e *codexEgressConnectError) Error() string { return e.err.Error() }
func (e *codexEgressConnectError) Unwrap() error { return e.err }

func isCodexEgressConnectError(err error) bool {
	var target *codexEgressConnectError
	return errors.As(err, &target)
}
