package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
)

// ListMasqueradeTraces returns a summary list of all stored trace records.
// GET /v0/management/masquerade-trace
func (h *Handler) ListMasqueradeTraces(c *gin.Context) {
	store := executor.GetGlobalTraceStore()
	summaries := store.List()
	c.JSON(http.StatusOK, gin.H{
		"traces":  summaries,
		"count":   len(summaries),
		"enabled": store.IsEnabled(),
	})
}

// GetMasqueradeTrace returns the full detail of a single trace record by ID.
// GET /v0/management/masquerade-trace/:id
func (h *Handler) GetMasqueradeTrace(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing trace id"})
		return
	}

	store := executor.GetGlobalTraceStore()
	record := store.Get(id)
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "trace record not found"})
		return
	}

	c.JSON(http.StatusOK, record)
}

// ClearMasqueradeTraces removes all stored trace records.
// DELETE /v0/management/masquerade-trace
func (h *Handler) ClearMasqueradeTraces(c *gin.Context) {
	store := executor.GetGlobalTraceStore()
	store.Clear()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "All masquerade trace records cleared",
	})
}
