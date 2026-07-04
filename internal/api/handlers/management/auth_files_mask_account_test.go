package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestListAuthFiles_MasksAPIKeyAccountKeepsOAuthEmail asserts that the auth-files
// listing masks the raw upstream API key for api_key auths (so the plaintext key
// never reaches the management HTTP response) while leaving the OAuth account
// email unchanged.
func TestListAuthFiles_MasksAPIKeyAccountKeepsOAuthEmail(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	const rawKey = "sk-secret-abcdef-1234567890-XYZ"
	const email = "alice@example.com"

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "apikey-auth",
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key":      rawKey,
			"runtime_only": "true",
		},
	}); err != nil {
		t.Fatalf("register api_key auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:         "oauth-auth",
		Provider:   "gemini",
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"email": email},
	}); err != nil {
		t.Fatalf("register oauth auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	h.ListAuthFiles(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), rawKey) {
		t.Fatalf("raw api key leaked in auth-files listing body: %s", rec.Body.String())
	}

	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	var apiKeyEntry, oauthEntry map[string]any
	for _, f := range payload.Files {
		switch f["id"] {
		case "apikey-auth":
			apiKeyEntry = f
		case "oauth-auth":
			oauthEntry = f
		}
	}
	if apiKeyEntry == nil || oauthEntry == nil {
		t.Fatalf("expected both auth entries, got %#v", payload.Files)
	}

	if got := apiKeyEntry["account_type"]; got != "api_key" {
		t.Fatalf("api_key account_type = %v, want api_key", got)
	}
	if got, want := apiKeyEntry["account"], util.HideAPIKey(rawKey); got != want {
		t.Fatalf("api_key account = %v, want masked %q", got, want)
	}

	if got := oauthEntry["account_type"]; got != "oauth" {
		t.Fatalf("oauth account_type = %v, want oauth", got)
	}
	if got := oauthEntry["account"]; got != email {
		t.Fatalf("oauth account = %v, want unchanged email %q", got, email)
	}
}
