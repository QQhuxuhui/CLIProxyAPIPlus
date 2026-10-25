package cliproxy

import (
	"context"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// startCodexFastSessionProber binds the Codex fast Session-Id prober and rollback checks to the
// service lifetime. The prober stays idle unless codex.fast-session-ids enables it.
func (s *Service) startCodexFastSessionProber(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	currentConfig := func() *internalconfig.Config {
		s.cfgMu.RLock()
		defer s.cfgMu.RUnlock()
		return s.cfg
	}
	helps.StartCodexFastSessionProber(ctx, helps.CodexFastSessionProbeDeps{
		Config: func() internalconfig.CodexFastSessionIDsConfig {
			if cfg := currentConfig(); cfg != nil {
				return cfg.Codex.FastSessionIDs
			}
			return internalconfig.CodexFastSessionIDsConfig{}
		},
		HomeMode:    s.coreManager.HomeEnabled,
		AcquireAuth: s.coreManager.AcquireCodexProbeAuth,
		Probe: func(ctx context.Context, auth *coreauth.Auth, model, sessionID string) (helps.CodexFastSessionProbeTiming, error) {
			timing, err := executor.NewCodexExecutor(currentConfig()).ProbeFastSession(ctx, auth, model, sessionID)
			s.coreManager.MarkCodexProbeResult(ctx, auth, model, err)
			return timing, err
		},
	})
}
