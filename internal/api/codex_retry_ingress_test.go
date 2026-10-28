package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexRetryIngressUsesWebsocketTokenSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, connection, upgrade string
		wantBudget                bool
	}{
		{"ordinary", "", "", true},
		{"header without connection", "", "websocket", true},
		{"websocket", "Upgrade", "websocket", false},
		{"token list", "keep-alive, Upgrade", "websocket, other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.Use(codexRetryIngressMiddleware())
			router.GET("/request", func(c *gin.Context) {
				ctx := c.Request.Context()
				hasBudget := auth.WithCodexRetryBudget(ctx) == ctx
				if hasBudget != tc.wantBudget {
					t.Errorf("budget=%v want %v", hasBudget, tc.wantBudget)
				}
				c.Status(http.StatusOK)
			})
			req := httptest.NewRequest(http.MethodGet, "/request", nil)
			req.Header.Set("Connection", tc.connection)
			req.Header.Set("Upgrade", tc.upgrade)
			router.ServeHTTP(httptest.NewRecorder(), req)
		})
	}
}
