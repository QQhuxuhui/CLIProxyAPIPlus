package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/latencytier"
)

// GetLatencyTier returns the latency tiering configuration and the latest snapshot.
func (h *Handler) GetLatencyTier(c *gin.Context) {
	tracker := latencytier.Default()
	if tracker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "latency tiering is not installed"})
		return
	}
	c.JSON(http.StatusOK, tracker.Snapshot())
}

// PutLatencyTierConfig replaces the runtime configuration. Body is a full Config
// object; omitted numeric fields fall back to the current values.
func (h *Handler) PutLatencyTierConfig(c *gin.Context) {
	tracker := latencytier.Default()
	if tracker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "latency tiering is not installed"})
		return
	}
	cfg := tracker.Config()
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	if err := tracker.SetConfig(cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "config": tracker.Config()})
}

// PostLatencyTierRecompute forces an immediate re-ranking.
func (h *Handler) PostLatencyTierRecompute(c *gin.Context) {
	tracker := latencytier.Default()
	if tracker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "latency tiering is not installed"})
		return
	}
	tracker.Recompute()
	c.JSON(http.StatusOK, tracker.Snapshot())
}
