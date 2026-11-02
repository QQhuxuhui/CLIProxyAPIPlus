package cliproxy

import (
	"context"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexspeed"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
)

// codexSpeedTickInterval drives hourly rollover and the minute summary.
const codexSpeedTickInterval = time.Minute

// startCodexSpeedRuntime loads the speed-strategy history under the auth dir and, once a
// minute, persists every finished hour. A missing auth dir disables persistence only.
func (s *Service) startCodexSpeedRuntime(ctx context.Context) {
	if s == nil {
		return
	}
	s.cfgMu.RLock()
	authDirRaw := ""
	if s.cfg != nil {
		authDirRaw = s.cfg.AuthDir
	}
	s.cfgMu.RUnlock()
	authDir, errResolve := util.ResolveAuthDir(authDirRaw)
	if errResolve != nil || authDir == "" {
		log.WithError(errResolve).Warn("codex speed: auth directory unavailable, history disabled")
		return
	}
	rt := codexspeed.NewRuntime(codexspeed.NewStore(filepath.Join(authDir, codexspeed.DirName)), time.Now)
	codexspeed.SetDefault(rt)
	go runCodexSpeedTicker(ctx, rt, time.Now)
}

func runCodexSpeedTicker(ctx context.Context, rt *codexspeed.Runtime, now func() time.Time) {
	ticker := time.NewTicker(codexSpeedTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			codexSpeedTick(rt, now())
		}
	}
}

// codexSpeedTick persists finished hours.
func codexSpeedTick(rt *codexspeed.Runtime, now time.Time) {
	for _, hour := range helps.CodexFastSessions().RollHours(now) {
		rec := hour
		rt.Record(codexspeed.HourRecord{Hour: rec.Hour, FastSession: &rec})
	}
}
