package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexquota"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexspeed"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
)

// Upstream addresses are variables so tests can point them at a local server.
var (
	codexUsageURL        = "https://chatgpt.com/backend-api/wham/usage"
	codexResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	codexResetConsumeURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
)

const (
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
	service.SetResetHooks(codexquota.ResetHooks{Credits: h.probeCodexResetCredits, Consume: h.consumeCodexResetCredit, ClearCooldown: h.clearCodexCooldown})
	h.mu.Lock()
	h.codexQuota = service
	h.mu.Unlock()
	codexspeed.SetQuotaProvider(func() map[string]codexspeed.QuotaReading { return codexSpeedQuotaReadings(service.Rows()) })
	service.Start(ctx)
}

// codexSpeedQuotaReadings maps quota rows to the longest window's used percent per auth file,
// skipping pending and invalid rows. Duplicate rows (several files of one upstream account)
// are kept; consumers deduplicate by AccountKey.
func codexSpeedQuotaReadings(rows map[string]codexquota.AccountRow) map[string]codexspeed.QuotaReading {
	out := make(map[string]codexspeed.QuotaReading, len(rows))
	for name, row := range rows {
		if row.Invalid || len(row.Windows) == 0 {
			continue
		}
		longest := row.Windows[0]
		for _, w := range row.Windows[1:] {
			if w.Minutes > longest.Minutes {
				longest = w
			}
		}
		if longest.Pending {
			continue
		}
		key := row.AccountKey
		if key == "" {
			key = name
		}
		out[name] = codexspeed.QuotaReading{AccountKey: key, UsedPercent: longest.UsedPercent}
	}
	return out
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
	return h.codexRequest(ctx, authID, http.MethodGet, codexUsageURL, nil, nil)
}

// probeCodexResetCredits lists reset credits with the panel's desktop headers.
func (h *Handler) probeCodexResetCredits(ctx context.Context, authID string) (int, []byte, error) {
	return h.codexRequest(ctx, authID, http.MethodGet, codexResetCreditsURL, nil, map[string]string{
		"Accept":      "application/json",
		"OpenAI-Beta": "codex-1",
		"Originator":  "Codex Desktop",
	})
}

// consumeCodexResetCredit redeems one credit. A fresh request id per attempt
// matches the management panel.
func (h *Handler) consumeCodexResetCredit(ctx context.Context, authID string) error {
	payload, errMarshal := json.Marshal(map[string]string{"redeem_request_id": uuid.NewString()})
	if errMarshal != nil {
		return fmt.Errorf("encode reset request: %w", errMarshal)
	}
	status, _, errRequest := h.codexRequest(ctx, authID, http.MethodPost, codexResetConsumeURL, payload, nil)
	if errRequest != nil {
		return errRequest
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return &codexquota.UpstreamError{Status: status}
	}
	return nil
}

func (h *Handler) clearCodexCooldown(ctx context.Context, authID string) error {
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return errors.New("auth manager is not available")
	}
	_, _, errReset := manager.ResetQuota(ctx, authID)
	return errReset
}

func (h *Handler) codexRequest(ctx context.Context, authID, method, url string, payload []byte, headers map[string]string) (int, []byte, error) {
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
	req, errRequest := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
	if errRequest != nil {
		return 0, nil, fmt.Errorf("build codex request: %w", errRequest)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", codexUsageUserAgent)
	req.Header.Set("Content-Type", "application/json")
	if accountID, _ := auth.Metadata["account_id"].(string); accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	transport := h.apiCallTransport(auth, "")
	if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
		defer closer.CloseIdleConnections()
	}
	resp, errDo := (&http.Client{Transport: transport}).Do(req)
	if errDo != nil {
		return 0, nil, fmt.Errorf("codex request failed: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("codex quota: close response failed")
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, codexUsageBodyLimit))
	if errRead != nil {
		return resp.StatusCode, nil, fmt.Errorf("read codex response: %w", errRead)
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

// GetCodexQuotaResetCheck reads one account's live reset state for the confirm dialog.
func (h *Handler) GetCodexQuotaResetCheck(c *gin.Context) {
	service := h.codexQuotaService()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "codex quota reporting is not available"})
		return
	}
	status, errCheck := service.CheckReset(c.Request.Context(), strings.TrimSpace(c.Query("name")))
	if errCheck != nil {
		writeCodexResetError(c, errCheck)
		return
	}
	c.JSON(http.StatusOK, status)
}

// PostCodexQuotaReset spends one reset credit of the named account.
func (h *Handler) PostCodexQuotaReset(c *gin.Context) {
	service := h.codexQuotaService()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "codex quota reporting is not available"})
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if errBind := c.ShouldBindJSON(&body); errBind != nil || strings.TrimSpace(body.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	status, errReset := service.Reset(c.Request.Context(), strings.TrimSpace(body.Name))
	if errReset != nil {
		writeCodexResetError(c, errReset)
		return
	}
	c.JSON(http.StatusOK, status)
}

func writeCodexResetError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, codexquota.ErrAccountNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, codexquota.ErrNoResetCredit), errors.Is(err, codexquota.ErrResetBusy):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
	}
}
