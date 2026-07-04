package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

func TestAuthenticateManagementKey_LocalhostIPBan_BlocksCorrectKeyDuringBan(t *testing.T) {
	h := &Handler{
		cfg:            &config.Config{},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
	}

	for i := 0; i < 5; i++ {
		allowed, statusCode, errMsg := h.AuthenticateManagementKey("127.0.0.1", true, "wrong-secret")
		if allowed {
			t.Fatalf("expected auth to be denied at attempt %d", i+1)
		}
		if statusCode != http.StatusUnauthorized || errMsg != "invalid management key" {
			t.Fatalf("unexpected auth failure at attempt %d: status=%d msg=%q", i+1, statusCode, errMsg)
		}
	}

	allowed, statusCode, errMsg := h.AuthenticateManagementKey("127.0.0.1", true, "test-secret")
	if allowed {
		t.Fatalf("expected correct key to be denied while banned")
	}
	if statusCode != http.StatusForbidden {
		t.Fatalf("expected forbidden status while banned, got %d", statusCode)
	}
	if !strings.HasPrefix(errMsg, "IP banned due to too many failed attempts. Try again in") {
		t.Fatalf("unexpected banned message: %q", errMsg)
	}
}

func TestMiddlewareSetsSupportPluginHeader(t *testing.T) {

	h := &Handler{
		cfg:            &config.Config{},
		failedAttempts: make(map[string]*attemptInfo),
		envSecret:      "test-secret",
	}
	middleware := h.Middleware()

	t.Run("invalid key", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		c.Request.RemoteAddr = "127.0.0.1:12345"
		c.Request.Header.Set("X-Management-Key", "wrong-secret")

		middleware(c)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})

	t.Run("valid key", func(t *testing.T) {
		engine := gin.New()
		engine.GET("/v0/management/config", middleware, func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Management-Key", "test-secret")
		engine.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != pluginhost.SupportPluginHeaderValue() {
			t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, pluginhost.SupportPluginHeaderValue())
		}
	})
}

// TestNewHandlerWarnsWhenEnvPasswordOverridesExplicitAllowRemoteFalse verifies that
// setting MANAGEMENT_PASSWORD while remote-management.allow-remote is false (the
// documented safe default) emits a prominent startup warning, since the env var
// silently forces remote management access to be allowed regardless of that
// setting (behavior unchanged; this only adds visibility for operators).
func TestNewHandlerWarnsWhenEnvPasswordOverridesExplicitAllowRemoteFalse(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-env-secret")

	hook := test.NewLocal(log.StandardLogger())
	t.Cleanup(hook.Reset)

	cfg := &config.Config{RemoteManagement: config.RemoteManagement{AllowRemote: false}}
	h := NewHandler(cfg, "", nil)

	if !h.allowRemoteOverride {
		t.Fatal("expected allowRemoteOverride to be true when MANAGEMENT_PASSWORD is set")
	}

	var found bool
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel && strings.Contains(entry.Message, "MANAGEMENT_PASSWORD") && strings.Contains(entry.Message, "allow-remote") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a warning log about MANAGEMENT_PASSWORD overriding allow-remote=false, got entries: %+v", hook.AllEntries())
	}
}

// TestNewHandlerNoWarnWhenAllowRemoteAlreadyTrue verifies the warning only fires
// when the env var actually changes effective behavior versus the configured value.
func TestNewHandlerNoWarnWhenAllowRemoteAlreadyTrue(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-env-secret")

	hook := test.NewLocal(log.StandardLogger())
	t.Cleanup(hook.Reset)

	cfg := &config.Config{RemoteManagement: config.RemoteManagement{AllowRemote: true}}
	_ = NewHandler(cfg, "", nil)

	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel && strings.Contains(entry.Message, "MANAGEMENT_PASSWORD") {
			t.Fatalf("did not expect an override warning when allow-remote is already true, got: %q", entry.Message)
		}
	}
}

// TestNewHandlerNoWarnWithoutEnvPassword verifies no override warning fires when
// MANAGEMENT_PASSWORD is not set at all.
func TestNewHandlerNoWarnWithoutEnvPassword(t *testing.T) {
	hook := test.NewLocal(log.StandardLogger())
	t.Cleanup(hook.Reset)

	cfg := &config.Config{RemoteManagement: config.RemoteManagement{AllowRemote: false}}
	h := NewHandler(cfg, "", nil)

	if h.allowRemoteOverride {
		t.Fatal("expected allowRemoteOverride to be false without MANAGEMENT_PASSWORD")
	}
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel && strings.Contains(entry.Message, "MANAGEMENT_PASSWORD") {
			t.Fatalf("did not expect an override warning without MANAGEMENT_PASSWORD, got: %q", entry.Message)
		}
	}
}
