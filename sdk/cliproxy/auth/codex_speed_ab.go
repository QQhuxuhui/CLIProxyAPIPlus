package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexspeed"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// Codex speed-priority request groups.
const (
	CodexSpeedGroupTreatment = "treatment"
	CodexSpeedGroupControl   = "control"
	// CodexSpeedGroupOff covers sessions between the effective share and the cap: they use the
	// normal selector and are not counted, so halving never moves treated sessions into control.
	CodexSpeedGroupOff = "off"
	// CodexSpeedGroupExcluded marks a control request that reused a binding chosen by treatment.
	CodexSpeedGroupExcluded = "excluded"
)

const (
	codexSpeedBucketSalt = "cli-proxy-api:codex:speed:bucket:"
	codexSpeedWRRIdle    = 5 * time.Minute
	// codexSpeedSessionTTL is the session lifetime when no affinity TTL is configured.
	codexSpeedSessionTTL  = time.Hour
	codexSpeedWindowSlots = 60
)

type codexSpeedSession struct {
	bucket  int
	rebound bool
	expires time.Time
}

type codexSpeedWRR struct {
	mu       sync.Mutex
	current  map[string]int64
	lastPick map[string]time.Time
}

type codexSpeedWindowSlot struct {
	minute                      time.Time
	treatReq, treatFail         int64
	controlReq, controlFail     int64
	treatAttempt, ctrlAttempt   int64
	treatOverload, ctrlOverload int64
}

// codexSpeedAB holds the experiment generation, share control, sessions and WRR state.
type codexSpeedAB struct {
	mu         sync.Mutex
	genID      uint64
	genHash    string
	genStart   time.Time
	cap        int
	effective  int
	rollback   bool
	lastAdjust time.Time
	window     [codexSpeedWindowSlots]codexSpeedWindowSlot

	sessionsMu sync.Mutex
	sessions   map[string]*codexSpeedSession

	wrrMu sync.Mutex
	wrr   map[uint64]*codexSpeedWRR

	probes atomic.Int64

	// hour accumulators are guarded by mu.
	hourStart time.Time
	treat     codexspeed.SpeedGroupHour
	control   codexspeed.SpeedGroupHour
}

func (ab *codexSpeedAB) init(now time.Time) {
	ab.sessions = map[string]*codexSpeedSession{}
	ab.wrr = map[uint64]*codexSpeedWRR{}
	ab.genStart = now
	ab.hourStart = now.Truncate(time.Hour)
}

// codexSpeedGenerationHash covers every field whose change starts a new experiment generation.
func codexSpeedGenerationHash(cfg internalconfig.CodexSpeedPriorityConfig) string {
	key := fmt.Sprintf("%t|%d|%d/%d/%d|%d|%d|%d|%d|%d|%d|%d|%d|%t",
		cfg.Enable, cfg.Percent, cfg.Weights.Fast, cfg.Weights.Unknown, cfg.Weights.Slow,
		cfg.FastGapMs, cfg.FastMaxOverloadPercent, cfg.SlowOverloadPercent, cfg.MinSamples,
		cfg.SampleWindowMinutes, cfg.HalfLifeMinutes, cfg.BaselineMinMs, cfg.MaxReasoningTokens, cfg.RollbackEnabled())
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}

func codexSpeedCap(cfg internalconfig.CodexSpeedPriorityConfig) int {
	if cfg.RollbackEnabled() {
		return min(cfg.Percent, 99)
	}
	return cfg.Percent
}

// applyConfig starts a new generation when a generation field changed.
func (ab *codexSpeedAB) applyConfig(cfg internalconfig.CodexSpeedPriorityConfig, now time.Time) {
	hash := codexSpeedGenerationHash(cfg)
	ab.mu.Lock()
	defer ab.mu.Unlock()
	ab.rollback = cfg.RollbackEnabled()
	ab.cap = codexSpeedCap(cfg)
	if hash == ab.genHash {
		ab.effective = min(ab.effective, ab.cap)
		return
	}
	ab.startGenerationLocked(hash, now, now)
}

func (ab *codexSpeedAB) startGenerationLocked(hash string, start, now time.Time) {
	ab.genID++
	ab.genHash = hash
	ab.genStart = start
	ab.effective = ab.cap
	ab.lastAdjust = now
	ab.window = [codexSpeedWindowSlots]codexSpeedWindowSlot{}
	ab.hourStart = now.Truncate(time.Hour)
	ab.treat = codexspeed.SpeedGroupHour{}
	ab.control = codexspeed.SpeedGroupHour{}
}

// restoreGeneration keeps a persisted generation start when the config hash is unchanged.
func (ab *codexSpeedAB) restoreGeneration(hash string, start time.Time) {
	ab.mu.Lock()
	defer ab.mu.Unlock()
	if hash != "" && hash == ab.genHash && !start.IsZero() {
		ab.genStart = start
	}
}

func (ab *codexSpeedAB) generation() (uint64, string, time.Time) {
	ab.mu.Lock()
	defer ab.mu.Unlock()
	return ab.genID, ab.genHash, ab.genStart
}

func (ab *codexSpeedAB) shares() (int, int) {
	ab.mu.Lock()
	defer ab.mu.Unlock()
	return ab.cap, ab.effective
}

func codexSpeedBucket(key string) int {
	return int(codexFastSessionStyleHash(codexSpeedBucketSalt+key) % 100)
}

func codexFastSessionStyleHash(value string) uint64 {
	sum := sha256.Sum256([]byte(value))
	var out uint64
	for _, b := range sum[:8] {
		out = out<<8 | uint64(b)
	}
	return out
}

// groupFor maps a stable bucket to the current group and generation.
func (ab *codexSpeedAB) groupFor(bucket int) (string, uint64) {
	ab.mu.Lock()
	defer ab.mu.Unlock()
	switch {
	case bucket >= ab.cap:
		return CodexSpeedGroupControl, ab.genID
	case bucket < ab.effective:
		return CodexSpeedGroupTreatment, ab.genID
	default:
		return CodexSpeedGroupOff, ab.genID
	}
}

// sessionBucket returns the session's stable bucket, creating it on first use, and extends the
// session's sliding expiry.
func (ab *codexSpeedAB) sessionBucket(key string, ttl time.Duration, now time.Time) int {
	if key == "" {
		return rand.IntN(100)
	}
	ab.sessionsMu.Lock()
	defer ab.sessionsMu.Unlock()
	s := ab.sessions[key]
	if s == nil || !now.Before(s.expires) {
		s = &codexSpeedSession{bucket: codexSpeedBucket(key)}
		ab.sessions[key] = s
	}
	s.expires = now.Add(ttl)
	return s.bucket
}

// adoptBucket gives a new binding identity the bucket of the request that created it, unless
// the identity already has one (concurrent first bindings keep the first writer).
func (ab *codexSpeedAB) adoptBucket(key string, bucket int, ttl time.Duration, now time.Time) {
	if key == "" {
		return
	}
	ab.sessionsMu.Lock()
	defer ab.sessionsMu.Unlock()
	if s := ab.sessions[key]; s != nil && now.Before(s.expires) {
		s.expires = now.Add(ttl)
		return
	}
	ab.sessions[key] = &codexSpeedSession{bucket: bucket, expires: now.Add(ttl)}
}

// reboundOnce reports whether the session already used its slow-binding replacement.
func (ab *codexSpeedAB) rebound(key string, now time.Time) bool {
	ab.sessionsMu.Lock()
	defer ab.sessionsMu.Unlock()
	s := ab.sessions[key]
	return s != nil && now.Before(s.expires) && s.rebound
}

func (ab *codexSpeedAB) markRebound(key string, ttl time.Duration, now time.Time) {
	ab.sessionsMu.Lock()
	defer ab.sessionsMu.Unlock()
	s := ab.sessions[key]
	if s == nil || !now.Before(s.expires) {
		s = &codexSpeedSession{bucket: codexSpeedBucket(key)}
		ab.sessions[key] = s
	}
	s.rebound = true
	s.expires = now.Add(ttl)
}

// pruneSessions drops expired sessions and idle WRR entries.
func (ab *codexSpeedAB) prune(now time.Time) {
	ab.sessionsMu.Lock()
	for key, s := range ab.sessions {
		if !now.Before(s.expires) {
			delete(ab.sessions, key)
		}
	}
	ab.sessionsMu.Unlock()
	ab.wrrMu.Lock()
	for _, w := range ab.wrr {
		w.mu.Lock()
		for id, at := range w.lastPick {
			if now.Sub(at) > codexSpeedWRRIdle {
				delete(w.lastPick, id)
				delete(w.current, id)
			}
		}
		w.mu.Unlock()
	}
	ab.wrrMu.Unlock()
}

func (ab *codexSpeedAB) wrrFor(poolID uint64) *codexSpeedWRR {
	ab.wrrMu.Lock()
	defer ab.wrrMu.Unlock()
	w := ab.wrr[poolID]
	if w == nil {
		w = &codexSpeedWRR{current: map[string]int64{}, lastPick: map[string]time.Time{}}
		ab.wrr[poolID] = w
	}
	return w
}

// pickSmoothWeightedBy runs smooth weighted round robin with weights from weightFn. Entries
// for absent candidates keep their value until pruned.
func pickSmoothWeightedBy(auths []*Auth, current map[string]int64, weightFn func(*Auth) int64) *Auth {
	var picked *Auth
	var pickedCurrent, total int64
	for _, a := range auths {
		if a == nil {
			continue
		}
		w := weightFn(a)
		if w <= 0 {
			continue
		}
		current[a.ID] = saturatingAddInt64(current[a.ID], w)
		total = saturatingAddInt64(total, w)
		if picked == nil || current[a.ID] > pickedCurrent {
			picked, pickedCurrent = a, current[a.ID]
		}
	}
	if picked != nil {
		current[picked.ID] = saturatingAddInt64(current[picked.ID], -total)
	}
	return picked
}

// PickWeighted selects among auths of one pool by tier-weighted smooth round robin.
func (t *CodexSpeedTracker) PickWeighted(poolID uint64, auths []*Auth) *Auth {
	if t == nil || len(auths) == 0 {
		return nil
	}
	cfg := t.config()
	w := t.ab.wrrFor(poolID)
	w.mu.Lock()
	defer w.mu.Unlock()
	picked := pickSmoothWeightedBy(auths, w.current, func(a *Auth) int64 { return t.weightFor(a, cfg) })
	if picked != nil {
		w.lastPick[picked.ID] = t.now()
	}
	return picked
}

func (ab *codexSpeedAB) slotLocked(now time.Time) *codexSpeedWindowSlot {
	minute := now.Truncate(time.Minute)
	slot := &ab.window[(minute.Unix()/60)%codexSpeedWindowSlots]
	if !slot.minute.Equal(minute) {
		*slot = codexSpeedWindowSlot{minute: minute}
	}
	return slot
}

func (ab *codexSpeedAB) groupHourLocked(group string, now time.Time) *codexspeed.SpeedGroupHour {
	if hour := now.Truncate(time.Hour); hour.After(ab.hourStart) {
		// The hour rolled without a RollHour call; drop it rather than mixing hours.
		ab.hourStart = hour
		ab.treat = codexspeed.SpeedGroupHour{}
		ab.control = codexspeed.SpeedGroupHour{}
	}
	switch group {
	case CodexSpeedGroupTreatment:
		return &ab.treat
	case CodexSpeedGroupControl:
		return &ab.control
	}
	return nil
}

func (ab *codexSpeedAB) observeAttempt(at CodexSpeedAttempt, now time.Time) {
	if at.Group != CodexSpeedGroupTreatment && at.Group != CodexSpeedGroupControl {
		return
	}
	ab.mu.Lock()
	defer ab.mu.Unlock()
	if at.Generation != ab.genID {
		return
	}
	g := ab.groupHourLocked(at.Group, now)
	if g == nil {
		return
	}
	g.Attempts++
	slot := ab.slotLocked(now)
	if at.Group == CodexSpeedGroupTreatment {
		slot.treatAttempt++
	} else {
		slot.ctrlAttempt++
	}
	switch {
	case at.Overload:
		g.Overloads++
		g.AttemptFailures++
		if at.Group == CodexSpeedGroupTreatment {
			slot.treatOverload++
		} else {
			slot.ctrlOverload++
		}
	case !at.Success:
		g.AttemptFailures++
	case at.TTFT > 0 && at.InputTokens < codexSpeedSmallInputTokens:
		g.Hist.Add(at.TTFT)
	}
}

// observeRequest records one finished client request that executed a Codex attempt.
func (ab *codexSpeedAB) observeRequest(group string, gen uint64, failed bool, status int, now time.Time) {
	if group != CodexSpeedGroupTreatment && group != CodexSpeedGroupControl {
		return
	}
	ab.mu.Lock()
	defer ab.mu.Unlock()
	if gen != ab.genID {
		return
	}
	g := ab.groupHourLocked(group, now)
	g.Requests++
	slot := ab.slotLocked(now)
	if group == CodexSpeedGroupTreatment {
		slot.treatReq++
	} else {
		slot.controlReq++
	}
	if failed {
		g.RequestFailures++
		if group == CodexSpeedGroupTreatment {
			slot.treatFail++
		} else {
			slot.controlFail++
		}
	}
	if status == 529 {
		g.Final529++
	}
}

func (ab *codexSpeedAB) countProbe() { ab.probes.Add(1) }

// evaluateRollback halves the effective share when treatment requests fail clearly more than
// control within the window, and otherwise climbs back one step per recover interval.
func (ab *codexSpeedAB) evaluateRollback(cfg internalconfig.CodexSpeedPriorityConfig, now time.Time) string {
	ab.mu.Lock()
	defer ab.mu.Unlock()
	if !ab.rollback {
		ab.effective = ab.cap
		return ""
	}
	var tr, tf, cr, cf int64
	window := time.Duration(cfg.Rollback.WindowMinutes) * time.Minute
	for i := range ab.window {
		s := &ab.window[i]
		if s.minute.IsZero() || now.Sub(s.minute) >= window {
			continue
		}
		tr, tf, cr, cf = tr+s.treatReq, tf+s.treatFail, cr+s.controlReq, cf+s.controlFail
	}
	if tr >= int64(cfg.Rollback.MinSamples) && cr >= int64(cfg.Rollback.MinSamples) {
		diff := 100*float64(tf)/float64(tr) - 100*float64(cf)/float64(cr)
		if diff >= float64(cfg.Rollback.FailureWorsePoints) && ab.effective > cfg.Rollback.MinPercent {
			ab.effective = max(cfg.Rollback.MinPercent, ab.effective/2)
			ab.lastAdjust = now
			// Start a new decision window: the evidence behind this halving is spent.
			ab.window = [codexSpeedWindowSlots]codexSpeedWindowSlot{}
			return fmt.Sprintf("halved to %d%%: treatment fails %.1f points more than control", ab.effective, diff)
		}
	}
	if ab.effective < ab.cap && now.Sub(ab.lastAdjust) >= time.Duration(cfg.Rollback.RecoverIntervalSeconds)*time.Second {
		ab.effective = min(ab.cap, ab.effective+cfg.Rollback.RecoverStepPercent)
		ab.lastAdjust = now
		return fmt.Sprintf("recovered to %d%%", ab.effective)
	}
	return ""
}

// rollHour returns the finished hour's group stats and resets them.
func (ab *codexSpeedAB) rollHour(now time.Time) (time.Time, codexspeed.SpeedGroupHour, codexspeed.SpeedGroupHour, int64, bool) {
	ab.mu.Lock()
	defer ab.mu.Unlock()
	current := now.Truncate(time.Hour)
	if !ab.hourStart.Before(current) {
		return time.Time{}, codexspeed.SpeedGroupHour{}, codexspeed.SpeedGroupHour{}, 0, false
	}
	hour, treat, control := ab.hourStart, ab.treat, ab.control
	ab.hourStart = current
	ab.treat = codexspeed.SpeedGroupHour{}
	ab.control = codexspeed.SpeedGroupHour{}
	return hour, treat, control, ab.probes.Swap(0), true
}

func (ab *codexSpeedAB) currentHour() (codexspeed.SpeedGroupHour, codexspeed.SpeedGroupHour, int64) {
	ab.mu.Lock()
	defer ab.mu.Unlock()
	treat, control := ab.treat, ab.control
	treat.Hist = append(codexspeed.Histogram(nil), treat.Hist...)
	control.Hist = append(codexspeed.Histogram(nil), control.Hist...)
	return treat, control, ab.probes.Load()
}

// codexSpeedRequestState is shared by every attempt of one client request through the context;
// metadata maps are cloned along the execution chain, the context is not.
type codexSpeedRequestState struct {
	once     sync.Once
	mu       sync.Mutex
	inited   bool
	group    string
	gen      uint64
	bucket   int
	executed atomic.Bool
}

type codexSpeedStateKey struct{}

func (s *codexSpeedRequestState) get() (string, uint64, int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.group, s.gen, s.bucket, s.inited
}

func (s *codexSpeedRequestState) setGroup(group string) {
	s.mu.Lock()
	s.group = group
	s.mu.Unlock()
}

func codexSpeedStateFrom(ctx context.Context) *codexSpeedRequestState {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(codexSpeedStateKey{}).(*codexSpeedRequestState)
	return s
}

// withCodexSpeedState attaches a request state when speed priority is active and the request
// may reach Codex.
func (t *CodexSpeedTracker) withCodexSpeedState(ctx context.Context, providers []string) (context.Context, *codexSpeedRequestState) {
	if !t.Active() || !hasCodexProvider(providers) || codexSpeedStateFrom(ctx) != nil {
		return ctx, nil
	}
	state := &codexSpeedRequestState{}
	return context.WithValue(ctx, codexSpeedStateKey{}, state), state
}

// CodexSpeedGroupFromContext returns the request's speed group and generation for usage tagging.
func CodexSpeedGroupFromContext(ctx context.Context) (string, uint64) {
	s := codexSpeedStateFrom(ctx)
	if s == nil {
		return "", 0
	}
	group, gen, _, inited := s.get()
	if !inited {
		return "", 0
	}
	return group, gen
}

// MarkCodexSpeedExecuted records that the request started at least one Codex attempt.
func MarkCodexSpeedExecuted(ctx context.Context) {
	if s := codexSpeedStateFrom(ctx); s != nil {
		s.executed.Store(true)
	}
}

// initRequest fixes the request's bucket and group on its first real (non-preview) pick.
func (t *CodexSpeedTracker) initRequest(state *codexSpeedRequestState, key string, ttl time.Duration) {
	if state == nil {
		return
	}
	state.once.Do(func() {
		bucket := t.ab.sessionBucket(key, ttl, t.now())
		group, gen := t.ab.groupFor(bucket)
		state.mu.Lock()
		state.bucket, state.group, state.gen, state.inited = bucket, group, gen, true
		state.mu.Unlock()
	})
}

// finishRequest records the request outcome once it is final.
func (t *CodexSpeedTracker) finishRequest(state *codexSpeedRequestState, failed bool, status int) {
	if state == nil || !state.executed.Load() {
		return
	}
	group, gen, _, inited := state.get()
	if !inited {
		return
	}
	t.ab.observeRequest(group, gen, failed, status, t.now())
}

// finishStreamRequest records a streaming request; a bootstrap failure returned as an error
// result with a nil error counts as failed.
func (t *CodexSpeedTracker) finishStreamRequest(state *codexSpeedRequestState, result *cliproxyexecutor.StreamResult, err error) {
	if result != nil {
		if v, ok := streamErrorResults.LoadAndDelete(result); ok && err == nil {
			err, _ = v.(error)
		}
	}
	t.finishRequest(state, err != nil, statusCodeFromError(err))
}

func speedStateGet(state *codexSpeedRequestState) (string, uint64, int, bool) {
	if state == nil {
		return "", 0, 0, false
	}
	return state.get()
}

// noteBindingUse excludes a control request from the A/B when it reuses a treatment binding.
func (t *CodexSpeedTracker) noteBindingUse(state *codexSpeedRequestState, binding codexPoolBinding) {
	if state == nil || binding.origin != CodexSpeedGroupTreatment {
		return
	}
	if group, _, _, inited := state.get(); inited && group == CodexSpeedGroupControl {
		state.setGroup(CodexSpeedGroupExcluded)
	}
}

// treatmentWeighted reports whether this pick should use tier weights.
func (t *CodexSpeedTracker) treatmentWeighted(ctx context.Context) bool {
	if !t.Active() || !t.Classifying() {
		return false
	}
	if preview, _ := ctx.Value(poolPreviewKey{}).(bool); preview {
		return false
	}
	group, _, _, inited := speedStateGet(codexSpeedStateFrom(ctx))
	return inited && group == CodexSpeedGroupTreatment
}
