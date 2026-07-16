package management

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// antigravityLoadCodeAssistDefaultBaseURL mirrors the executor's production base
// URL used for loadCodeAssist (the plan/套餐 + credits source).
const antigravityLoadCodeAssistDefaultBaseURL = "https://cloudcode-pa.googleapis.com"

// RefreshAntigravityCredits force-refreshes the antigravity plan (套餐) and
// credits balance for a single account by calling loadCodeAssist, then updates
// the cached credits hint so the account page can render the plan immediately.
//
// This exists because the executor only refreshes the credits hint lazily (and
// throttled), so a freshly imported account shows an empty 套餐 until it has
// served a request. Body: {"auth_index": "..."}.
func (h *Handler) RefreshAntigravityCredits(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		AuthIndex string `json:"auth_index"`
	}
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	authIndex := strings.TrimSpace(req.AuthIndex)
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}

	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth is not an antigravity account"})
		return
	}

	ctx := c.Request.Context()
	token, errToken := h.refreshAntigravityOAuthAccessToken(ctx, auth)
	if errToken != nil || strings.TrimSpace(token) == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to acquire access token: %v", errToken)})
		return
	}

	hint, cacheable, errRefresh := h.fetchAntigravityCredits(ctx, auth, token)
	if errRefresh != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to refresh credits: %v", errRefresh)})
		return
	}
	if cacheable {
		coreauth.SetAntigravityCreditsHint(strings.TrimSpace(auth.ID), hint)
	}

	auth.EnsureIndex()
	c.JSON(http.StatusOK, gin.H{
		"status":            "ok",
		"auth_index":        auth.Index,
		"paid_tier":         hint.PaidTierID,
		"available":         hint.Available,
		"credit_amount":     hint.CreditAmount,
		"min_credit_amount": hint.MinCreditAmount,
	})
}

// fetchAntigravityCredits replicates the executor's loadCodeAssist call so the
// management API can force-refresh a single account's plan/credits on demand.
func (h *Handler) fetchAntigravityCredits(ctx context.Context, auth *coreauth.Auth, token string) (coreauth.AntigravityCreditsHint, bool, error) {
	baseURL := antigravityLoadCodeAssistBaseURLForAuth(auth)
	endpointURL := strings.TrimSuffix(baseURL, "/") + "/v1internal:loadCodeAssist"
	reqBody := []byte(`{"metadata":{"ideType":"ANTIGRAVITY"}}`)

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(reqBody))
	if errReq != nil {
		return coreauth.AntigravityCreditsHint{}, false, errReq
	}
	httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	httpReq.Header.Set("Accept", "*/*")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", misc.AntigravityLoadCodeAssistUserAgent(antigravityConfiguredUserAgent(auth)))

	httpClient := h.antigravityCreditsHTTPClient(auth)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		return coreauth.AntigravityCreditsHint{}, false, errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("management: close loadCodeAssist response body error: %v", errClose)
		}
	}()

	bodyBytes, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		return coreauth.AntigravityCreditsHint{}, false, errRead
	}
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		return coreauth.AntigravityCreditsHint{}, false, fmt.Errorf("loadCodeAssist returned status %d: %s", httpResp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}
	hint, cacheable := parseAntigravityCredits(bodyBytes)
	return hint, cacheable, nil
}

func parseAntigravityCredits(bodyBytes []byte) (coreauth.AntigravityCreditsHint, bool) {
	paidTierID := strings.TrimSpace(gjson.GetBytes(bodyBytes, "paidTier.id").String())
	hint := coreauth.AntigravityCreditsHint{
		PaidTierID: paidTierID,
		UpdatedAt:  time.Now(),
	}

	credits := gjson.GetBytes(bodyBytes, "paidTier.availableCredits")
	if !credits.IsArray() {
		hint.Known = true
		return hint, true
	}
	for _, credit := range credits.Array() {
		if !strings.EqualFold(credit.Get("creditType").String(), "GOOGLE_ONE_AI") {
			continue
		}
		creditAmount, errCA := strconv.ParseFloat(strings.TrimSpace(credit.Get("creditAmount").String()), 64)
		if errCA != nil {
			continue
		}
		minAmount, errMA := strconv.ParseFloat(strings.TrimSpace(credit.Get("minimumCreditAmountForUsage").String()), 64)
		if errMA != nil {
			continue
		}
		hint.Known = true
		hint.Available = creditAmount >= minAmount
		hint.CreditAmount = creditAmount
		hint.MinCreditAmount = minAmount
		return hint, true
	}
	return hint, false
}

func (h *Handler) antigravityCreditsHTTPClient(auth *coreauth.Auth) *http.Client {
	return &http.Client{Transport: h.apiCallTransport(auth)}
}

// antigravityConfiguredUserAgent extracts a caller-configured UA from the auth,
// mirroring the executor helper of the same purpose.
func antigravityConfiguredUserAgent(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if ua := strings.TrimSpace(auth.Attributes["user_agent"]); ua != "" {
			return ua
		}
	}
	if auth.Metadata != nil {
		if ua, ok := auth.Metadata["user_agent"].(string); ok {
			if ua = strings.TrimSpace(ua); ua != "" {
				return ua
			}
		}
	}
	return ""
}

// antigravityLoadCodeAssistBaseURLForAuth resolves a per-auth custom base URL,
// falling back to the production loadCodeAssist host.
func antigravityLoadCodeAssistBaseURLForAuth(auth *coreauth.Auth) string {
	if auth != nil {
		if auth.Attributes != nil {
			if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
				return strings.TrimSuffix(v, "/")
			}
		}
		if auth.Metadata != nil {
			if v, ok := auth.Metadata["base_url"].(string); ok {
				if v = strings.TrimSpace(v); v != "" {
					return strings.TrimSuffix(v, "/")
				}
			}
		}
	}
	return antigravityLoadCodeAssistDefaultBaseURL
}
