package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexquota"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	codexUsageURL       = "https://chatgpt.com/backend-api/wham/usage"
	codexUsageUserAgent = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"
	codexUsageBodyLimit = 1 << 20
)

// StartCodexQuota launches quota aggregation for the lifetime of ctx.
func (h *Handler) StartCodexQuota(ctx context.Context) {
	if h == nil || h.cfg == nil {
		return
	}
	authDir, errResolve := util.ResolveAuthDir(h.cfg.AuthDir)
	if errResolve != nil || authDir == "" {
		log.WithError(errResolve).Warn("codex quota: auth directory unavailable, reporting disabled")
		return
	}
	service := codexquota.New(filepath.Join(authDir, codexquota.DirName), h.codexQuotaSource, h.probeCodexUsage)
	h.mu.Lock()
	h.codexQuota = service
	h.mu.Unlock()
	service.Start(ctx)
}

func (h *Handler) codexQuotaSource() codexquota.Source {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.authManager == nil {
		return nil
	}
	return h.authManager
}

func (h *Handler) codexQuotaService() *codexquota.Service {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.codexQuota
}

// probeCodexUsage issues the same read-only request as the management panel.
// It shares the management APICall timeout: a probe is bounded management work.
func (h *Handler) probeCodexUsage(ctx context.Context, authID string) (int, []byte, error) {
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return 0, nil, errors.New("auth manager is not available")
	}
	auth, ok := manager.GetByID(authID)
	if !ok {
		return 0, nil, errors.New("credential not found")
	}
	token := tokenValueForAuth(auth)
	if token == "" {
		return 0, nil, errors.New("credential has no token")
	}
	// An expired access token is awaiting refresh; its 401 says nothing about the account.
	if expiry, ok := auth.AccessTokenExpirationTime(); ok && !expiry.After(time.Now()) {
		return 0, nil, errors.New("access token expired, waiting for refresh")
	}
	ctx, cancel := context.WithTimeout(ctx, defaultAPICallTimeout)
	defer cancel()
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if errRequest != nil {
		return 0, nil, fmt.Errorf("build usage request: %w", errRequest)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", codexUsageUserAgent)
	req.Header.Set("Content-Type", "application/json")
	if accountID, _ := auth.Metadata["account_id"].(string); accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	transport := h.apiCallTransport(auth, "")
	if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
		defer closer.CloseIdleConnections()
	}
	resp, errDo := (&http.Client{Transport: transport}).Do(req)
	if errDo != nil {
		return 0, nil, fmt.Errorf("usage request failed: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("codex quota: close usage response failed")
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, codexUsageBodyLimit))
	if errRead != nil {
		return resp.StatusCode, nil, fmt.Errorf("read usage response: %w", errRead)
	}
	return resp.StatusCode, body, nil
}

// GetCodexQuota returns the pool quota report and add-accounts advice.
func (h *Handler) GetCodexQuota(c *gin.Context) {
	service := h.codexQuotaService()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "codex quota reporting is not available"})
		return
	}
	summary := service.Summary()
	if summary == nil {
		c.JSON(http.StatusOK, gin.H{"available": false, "settings": service.Settings()})
		return
	}
	c.JSON(http.StatusOK, summary)
}

// GetCodexQuotaAccounts returns per-credential quota keyed by account name.
func (h *Handler) GetCodexQuotaAccounts(c *gin.Context) {
	service := h.codexQuotaService()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "codex quota reporting is not available"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"accounts": service.Rows()})
}

// PutCodexQuotaSettings merges the supplied options and applies them immediately.
func (h *Handler) PutCodexQuotaSettings(c *gin.Context) {
	service := h.codexQuotaService()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "codex quota reporting is not available"})
		return
	}
	next := service.Settings()
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(&next); errDecode != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errDecode.Error()})
		return
	}
	if errUpdate := service.UpdateSettings(next); errUpdate != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errUpdate.Error()})
		return
	}
	c.JSON(http.StatusOK, next)
}
