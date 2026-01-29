package management

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kiro"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
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

	if h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   "config not initialized",
		})
		return
	}

	kAuth := kiroauth.NewKiroAuth(h.cfg)
	tokenData := &kiroauth.KiroTokenData{
		AccessToken: strings.TrimSpace(accessToken),
		ProfileArn:  strings.TrimSpace(profileArn),
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	usageInfo, err := kAuth.GetUsageLimits(ctx, tokenData)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   "failed to get usage info: " + err.Error(),
		})
		return
	}

	var percentage float64
	if usageInfo.UsageLimit > 0 {
		percentage = (usageInfo.CurrentUsage / usageInfo.UsageLimit) * 100
	}

	daysUntilReset, nextResetDate := parseKiroResetTime(usageInfo.NextReset, time.Now())

	email := extractKiroEmail(auth)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"email":        email,
			"subscription": usageInfo.SubscriptionTitle,
			"usage": gin.H{
				"current":    usageInfo.CurrentUsage,
				"limit":      usageInfo.UsageLimit,
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
