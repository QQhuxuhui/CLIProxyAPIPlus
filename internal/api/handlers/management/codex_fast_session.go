package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

// GetCodexFastSession reports the Codex fast Session-Id pool configuration, members, per-group
// window stats, rollback state, recent pool events, and prober state. It is read-only apart
// from syncing the pool with the current config when no prober owns that sync.
func (h *Handler) GetCodexFastSession(c *gin.Context) {
	h.mu.Lock()
	cfg := h.cfg
	h.mu.Unlock()
	pool := helps.CodexFastSessions()
	window := 0
	if cfg != nil {
		fastCfg := cfg.Codex.FastSessionIDs
		fastCfg.Normalize()
		pool.ApplyUnmanagedConfig(fastCfg)
		window = fastCfg.Rollback.WindowMinutes
	}
	c.JSON(http.StatusOK, pool.Snapshot(window))
}
