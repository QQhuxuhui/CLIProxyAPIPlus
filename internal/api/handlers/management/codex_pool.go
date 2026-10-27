package management

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func (h *Handler) GetCodexPool(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager is not available"})
		return
	}
	c.JSON(http.StatusOK, h.authManager.CodexPoolSnapshot())
}

func (h *Handler) PutCodexPoolConfig(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration is not available"})
		return
	}
	next := h.cfg.CloneForRuntime()
	value := next.ProviderRequestRateCap["codex"].WithDefaults()
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	body := &value
	if err := decoder.Decode(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected a configuration object"})
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected one JSON configuration object"})
		return
	}
	if err := value.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if next.Home.Enabled && value.Enabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Home dispatch owns scheduling; configure rate admission on Home"})
		return
	}
	if next.ProviderRequestRateCap == nil {
		next.ProviderRequestRateCap = map[string]config.RequestRateCapConfig{}
	}
	next.ProviderRequestRateCap["codex"] = value.WithDefaults()
	h.persistConfigLocked(c, next)
}

// GetCodexPools exposes partition state without credential or token material.
func (h *Handler) GetCodexPools(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager is not available"})
		return
	}
	c.JSON(http.StatusOK, h.authManager.CodexPoolsSnapshot())
}
