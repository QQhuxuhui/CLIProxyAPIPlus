package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexPoolConfigSaveAndValidation(t *testing.T) {
	h := &Handler{cfg: &config.Config{}, configFilePath: writeTestConfigFile(t), authManager: coreauth.NewManager(nil, nil, nil)}
	call := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/codex-pool/config", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PutCodexPoolConfig(ctx)
		return rec
	}
	if rec := call(`{"enabled":true,"headroom":0.75}`); rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	got := h.cfg.ProviderRequestRateCap["codex"]
	if !got.Enabled || got.Headroom != .75 || got.InitialPerMinute != 20 {
		t.Fatalf("saved: %+v", got)
	}
	persisted, err := config.LoadConfig(h.configFilePath)
	if err != nil || persisted.ProviderRequestRateCap["codex"] != got {
		t.Fatalf("persisted: %v %v", persisted, err)
	}
	for _, body := range []string{`{"headroom":2}`, `{"min-per-minute":70}`, `{"probe-interval":"oops"}`, `{"enabled":false,"fail-fast":true}`, `{"unknown":true}`} {
		if rec := call(body); rec.Code != 400 {
			t.Fatalf("invalid %s: %d", body, rec.Code)
		}
		if h.cfg.ProviderRequestRateCap["codex"] != got {
			t.Fatal("invalid update mutated live config")
		}
	}
	h.configFilePath = t.TempDir() + "/missing/config.yaml"
	if rec := call(`{"headroom":0.5}`); rec.Code != 500 {
		t.Fatalf("save failure: %d", rec.Code)
	}
	if h.cfg.ProviderRequestRateCap["codex"] != got {
		t.Fatal("failed persistence mutated live config")
	}
}

func TestCodexPoolSnapshotEndpoint(t *testing.T) {
	h := &Handler{authManager: coreauth.NewManager(nil, nil, nil)}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	h.GetCodexPool(ctx)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"points"`) || strings.Contains(rec.Body.String(), "api_key") {
		t.Fatalf("snapshot: %d %s", rec.Code, rec.Body.String())
	}
}
