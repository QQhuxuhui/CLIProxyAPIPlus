package cliproxy

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexspeed"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// codexSpeedTickInterval drives the recompute, hourly rollover and the minute summary.
	codexSpeedTickInterval = time.Minute
	// codexSpeedStateEvery is how often accounts.dat is rewritten, in ticks.
	codexSpeedStateEvery = 5
	// codexSpeedMaxProbesInFlight bounds concurrent speed probes.
	codexSpeedMaxProbesInFlight = 8
)

type codexSpeedService struct {
	s       *Service
	rt      *codexspeed.Runtime
	tracker *coreauth.CodexSpeedTracker
	ticks   int

	infosMu sync.Mutex
	infos   []coreauth.CodexSpeedAuthInfo

	inflight atomic.Int64
	// acquire and launch are the probe admission and start hooks; tests replace them.
	acquire func(id, model string) *coreauth.Auth
	launch  func(ctx context.Context, auth *coreauth.Auth, model string)
}

// startCodexSpeedRuntime loads the speed-strategy history and state under the auth dir, then
// runs the minute maintenance and the probe loop for the service lifetime. Without an auth dir
// the tracker still runs; only persistence is disabled.
func (s *Service) startCodexSpeedRuntime(ctx context.Context) {
	if s == nil {
		return
	}
	authDir, errResolve := util.ResolveAuthDir(s.currentConfig().AuthDir)
	var store *codexspeed.Store
	if errResolve != nil || authDir == "" {
		log.WithError(errResolve).Warn("codex speed: auth directory unavailable, history disabled")
	} else {
		store = codexspeed.NewStore(filepath.Join(authDir, codexspeed.DirName))
	}
	rt := codexspeed.NewRuntime(store, time.Now)
	codexspeed.SetDefault(rt)
	svc := &codexSpeedService{s: s, rt: rt, tracker: coreauth.CodexSpeed()}
	svc.acquire = func(id, model string) *coreauth.Auth {
		auth, _ := s.coreManager.AcquireCodexProbeAuthByID(id, model)
		return auth
	}
	svc.launch = func(ctx context.Context, auth *coreauth.Auth, model string) {
		go svc.probe(ctx, auth, model)
	}
	svc.applyConfig()
	if errRestore := svc.tracker.RestoreState(store.LoadAccounts()); errRestore != nil {
		log.WithError(errRestore).Warn("codex speed: ignoring unreadable accounts state")
	}
	if errPrune := store.PruneTransitions(time.Now()); errPrune != nil {
		log.WithError(errPrune).Warn("codex speed: prune transitions failed")
	}
	go svc.runTicker(ctx)
	go svc.runProber(ctx)
}

func (s *Service) currentConfig() *internalconfig.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if s.cfg == nil {
		return &internalconfig.Config{}
	}
	return s.cfg
}

func (c *codexSpeedService) applyConfig() internalconfig.CodexSpeedPriorityConfig {
	cfg := c.s.currentConfig()
	speedCfg := cfg.Codex.SpeedPriority
	speedCfg.Normalize()
	poolsConfigured := cfg.Codex.Pools.Enable && !cfg.Home.Enabled
	running := c.s.coreManager != nil && c.s.coreManager.CodexPoolsRunning()
	c.tracker.SetConfig(speedCfg, poolsConfigured, running)
	return speedCfg
}

func (c *codexSpeedService) runTicker(ctx context.Context) {
	ticker := time.NewTicker(codexSpeedTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.saveState()
			return
		case <-ticker.C:
			c.tick(time.Now())
		}
	}
}

// tick runs one minute of maintenance.
func (c *codexSpeedService) tick(now time.Time) {
	speedCfg := c.applyConfig()
	var infos []coreauth.CodexSpeedAuthInfo
	if c.s.coreManager != nil {
		infos = c.s.coreManager.CodexSpeedAuthInfos(speedCfg.Probe.Model)
	}
	readings := codexspeed.QuotaReadings()
	usedByKey := make(map[string]float64, len(readings))
	for _, r := range readings {
		usedByKey[r.AccountKey] = r.UsedPercent
	}
	for i := range infos {
		if r, ok := readings[infos[i].ID]; ok {
			infos[i].QuotaUsed = r.UsedPercent
		}
	}
	c.infosMu.Lock()
	c.infos = infos
	c.infosMu.Unlock()

	if transitions := c.tracker.Recompute(infos); len(transitions) > 0 {
		if err := c.rt.Store().AppendTransitions(transitions); err != nil {
			log.WithError(err).Error("codex speed: persist transitions failed")
		}
	}
	if event := c.tracker.Tick(); event != "" {
		log.Infof("codex speed share %s", event)
	}
	c.rollHours(now, usedByKey)
	c.ticks++
	if c.ticks%codexSpeedStateEvery == 0 {
		c.saveState()
	}
	log.Info(c.tracker.SummaryLine())
}

// rollHours persists every finished hour, merging fast session and speed records by hour.
func (c *codexSpeedService) rollHours(now time.Time, usedByKey map[string]float64) {
	records := map[time.Time]*codexspeed.HourRecord{}
	get := func(hour time.Time) *codexspeed.HourRecord {
		if rec := records[hour]; rec != nil {
			return rec
		}
		rec := &codexspeed.HourRecord{Hour: hour}
		records[hour] = rec
		return rec
	}
	for _, hour := range helps.CodexFastSessions().RollHours(now) {
		fs := hour
		get(fs.Hour).FastSession = &fs
	}
	if speedHours := c.tracker.RollHours(usedByKey); len(speedHours) > 0 {
		for _, hour := range speedHours {
			sh := hour
			rec := get(sh.Hour)
			if rec.Speed == nil {
				rec.Speed = &sh
			} else {
				rec.SpeedParts = append(rec.SpeedParts, sh)
			}
		}
		if err := c.rt.Store().PruneTransitions(now); err != nil {
			log.WithError(err).Warn("codex speed: prune transitions failed")
		}
	}
	hours := make([]time.Time, 0, len(records))
	for hour := range records {
		hours = append(hours, hour)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i].Before(hours[j]) })
	for _, hour := range hours {
		c.rt.Record(*records[hour])
	}
}

func (c *codexSpeedService) saveState() {
	data, err := c.tracker.MarshalState()
	if err != nil {
		log.WithError(err).Error("codex speed: encode state failed")
		return
	}
	if errSave := c.rt.Store().SaveAccounts(data); errSave != nil {
		log.WithError(errSave).Error("codex speed: persist state failed")
	}
}

// runProber sends up to probe.max-per-second probes each second while speed priority is active.
func (c *codexSpeedService) runProber(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !c.tracker.Active() || c.s.coreManager == nil {
			continue
		}
		speedCfg := c.s.currentConfig().Codex.SpeedPriority
		speedCfg.Normalize()
		c.infosMu.Lock()
		infos := c.infos
		c.infosMu.Unlock()
		c.probeRound(ctx, infos, speedCfg.Probe.MaxPerSecond, speedCfg.Probe.Model)
	}
}

// probeRound starts up to maxPerSecond probes, walking the whole candidate order until the
// budget, the in-flight cap or the candidates run out. A failed admission restores the
// reservation and spends no budget. It returns the number of probes started.
func (c *codexSpeedService) probeRound(ctx context.Context, infos []coreauth.CodexSpeedAuthInfo, maxPerSecond int, model string) int {
	budget := min(maxPerSecond, codexSpeedMaxProbesInFlight-int(c.inflight.Load()))
	started := 0
	if budget <= 0 {
		return 0
	}
	for _, id := range c.tracker.ProbeCandidates(infos, len(infos)) {
		if started >= budget {
			break
		}
		previous := c.tracker.LastProbe(id)
		if !c.tracker.ReserveProbe(id) {
			continue
		}
		auth := c.acquire(id, model)
		if auth == nil {
			c.tracker.CancelProbe(id, previous)
			continue
		}
		started++
		c.inflight.Add(1)
		c.launch(ctx, auth, model)
	}
	return started
}

func (c *codexSpeedService) probe(ctx context.Context, auth *coreauth.Auth, model string) {
	defer c.inflight.Add(-1)
	defer c.s.coreManager.ReleaseCodexProbeAuth(auth)
	timing, err := executor.NewCodexExecutor(c.s.currentConfig()).ProbeFastSession(ctx, auth, model, uuid.NewString())
	res := coreauth.CodexSpeedProbeResult{AuthID: auth.ID}
	switch {
	case err == nil:
		res.Success = true
		if timing.Created > 0 && timing.First > timing.Created {
			res.Gap = timing.First - timing.Created
		}
	case errors.Is(err, context.Canceled):
		return
	default:
		res.Overload = codexSpeedProbeOverload(err)
	}
	c.tracker.ObserveProbe(res)
}

func codexSpeedProbeOverload(err error) bool {
	type statusCoder interface{ StatusCode() int }
	var sc statusCoder
	if errors.As(err, &sc) {
		switch sc.StatusCode() {
		case 502, 503, 529:
			return true
		}
	}
	return strings.Contains(err.Error(), "server_is_overloaded")
}
