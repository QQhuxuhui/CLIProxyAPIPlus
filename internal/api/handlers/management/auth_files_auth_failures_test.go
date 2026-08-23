package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestListAuthFiles_IncludesAuthFailuresAndLastError(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:           "runtime-only-auth-2",
		Provider:     "codex",
		AuthFailures: 3,
		LastError:    &coreauth.Error{Code: "unauthorized", Message: "web image credential was rejected (token_invalidated)", HTTPStatus: 401},
		Attributes:   map[string]string{"runtime_only": "true"},
		Metadata:     map[string]any{"type": "codex"},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.tokenStore = &memoryAuthStore{}

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)

	h.ListAuthFiles(ginCtx)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode list payload: %v", errUnmarshal)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("expected 1 auth entry, got %d", len(payload.Files))
	}
	entry := payload.Files[0]
	if got, _ := entry["auth_failures"].(float64); got != 3 {
		t.Fatalf("auth_failures = %#v, want 3", entry["auth_failures"])
	}
	lastError, ok := entry["last_error"].(map[string]any)
	if !ok {
		t.Fatalf("expected last_error object, got %#v", entry["last_error"])
	}
	if lastError["code"] != "unauthorized" || lastError["http_status"] != float64(401) {
		t.Fatalf("unexpected last_error: %#v", lastError)
	}
}
