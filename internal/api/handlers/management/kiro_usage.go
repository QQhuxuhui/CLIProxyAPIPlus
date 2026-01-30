package management

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kiro"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// GetKiroUsage handles GET /v0/management/kiro-usage
// Returns usage/quota information for a specific Kiro auth by auth_index
func (h *Handler) GetKiroUsage(c *gin.Context) {
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   "auth_index parameter is required",
		})
		return
	}

	if h.authManager == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   "auth manager not initialized",
		})
		return
	}

	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"error":   "auth not found",
		})
		return
	}

	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "kiro") {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   "not a kiro auth",
		})
		return
	}

	accessToken, profileArn := extractKiroCredentials(auth)
	if strings.TrimSpace(accessToken) == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   "kiro access token not found",
		})
		return
	}

	// Log diagnostic information
	log.Debugf("GetKiroUsage: auth_index=%s, profileArn='%s', accessToken length=%d", authIndex, profileArn, len(accessToken))

	if h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   "config not initialized",
		})
		return
	}

	// Use CodeWhispererClient which uses the REST API (GET with query params)
	// This works without profileArn, unlike the JSON-RPC API
	cwClient := kiro.NewCodeWhispererClient(h.cfg, "")

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	usageResp, err := cwClient.GetUsageLimits(ctx, strings.TrimSpace(accessToken))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   "failed to get usage info: " + err.Error(),
		})
		return
	}

	var currentUsage, usageLimit float64
	if len(usageResp.UsageBreakdownList) > 0 {
		breakdown := usageResp.UsageBreakdownList[0]
		if breakdown.CurrentUsageWithPrecision != nil {
			currentUsage = *breakdown.CurrentUsageWithPrecision
		}
		if breakdown.UsageLimitWithPrecision != nil {
			usageLimit = *breakdown.UsageLimitWithPrecision
		}
	}

	var percentage float64
	if usageLimit > 0 {
		percentage = (currentUsage / usageLimit) * 100
	}

	var nextResetRaw string
	if usageResp.NextDateReset != nil {
		nextResetRaw = fmt.Sprintf("%v", *usageResp.NextDateReset)
	}
	daysUntilReset, nextResetDate := parseKiroResetTime(nextResetRaw, time.Now())

	var subscriptionTitle string
	if usageResp.SubscriptionInfo != nil {
		subscriptionTitle = usageResp.SubscriptionInfo.SubscriptionTitle
	}

	email := extractKiroEmail(auth)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"email":        email,
			"subscription": subscriptionTitle,
			"usage": gin.H{
				"current":    currentUsage,
				"limit":      usageLimit,
				"percentage": percentage,
			},
			"reset": gin.H{
				"days_until": daysUntilReset,
				"next_date":  nextResetDate,
			},
		},
	})
}

// extractKiroCredentials extracts access token and profile ARN from auth object
func extractKiroCredentials(auth *coreauth.Auth) (accessToken, profileArn string) {
	if auth == nil {
		return "", ""
	}

	if auth.Metadata != nil {
		if token, ok := auth.Metadata["access_token"].(string); ok && strings.TrimSpace(token) != "" {
			accessToken = strings.TrimSpace(token)
		}
		if arn, ok := auth.Metadata["profile_arn"].(string); ok && strings.TrimSpace(arn) != "" {
			profileArn = strings.TrimSpace(arn)
		}
	}

	if accessToken == "" && auth.Attributes != nil {
		if token := strings.TrimSpace(auth.Attributes["access_token"]); token != "" {
			accessToken = token
		}
		if arn := strings.TrimSpace(auth.Attributes["profile_arn"]); arn != "" {
			profileArn = arn
		}
	}

	if accessToken == "" && auth.Metadata != nil {
		if token, ok := auth.Metadata["accessToken"].(string); ok && strings.TrimSpace(token) != "" {
			accessToken = strings.TrimSpace(token)
		}
		if arn, ok := auth.Metadata["profileArn"].(string); ok && strings.TrimSpace(arn) != "" {
			profileArn = strings.TrimSpace(arn)
		}
	}

	return accessToken, profileArn
}

// extractKiroEmail extracts email from auth object
func extractKiroEmail(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}

	if auth.Metadata != nil {
		if email, ok := auth.Metadata["email"].(string); ok && strings.TrimSpace(email) != "" {
			return strings.TrimSpace(email)
		}
	}

	if auth.Attributes != nil {
		if email := strings.TrimSpace(auth.Attributes["email"]); email != "" {
			return email
		}
	}

	return ""
}

func parseKiroResetTime(raw string, now time.Time) (daysUntilReset int, nextResetDate string) {
	if strings.TrimSpace(raw) == "" {
		return 0, ""
	}
	ts, err := strconv.ParseFloat(raw, 64)
	if err != nil || ts <= 0 {
		return 0, ""
	}
	if ts > 1e12 {
		ts = ts / 1000
	}
	if now.IsZero() {
		now = time.Now()
	}
	resetTime := time.Unix(int64(ts), 0).UTC()
	daysUntilReset = int(resetTime.Sub(now).Hours() / 24)
	nextResetDate = resetTime.Format(time.RFC3339)
	return daysUntilReset, nextResetDate
}
