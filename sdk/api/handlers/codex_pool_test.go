package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestCodexPoolBusyHTTPEnvelope(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{ProviderRequestRateCap: map[string]internalconfig.RequestRateCapConfig{"codex": {Enabled: true, FailFast: true}}})
	manager.RegisterExecutor(&modelExecutionCaptureExecutor{provider: "codex"})
	id := "pool-envelope"
	registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "pool-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	_, err := manager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive, Unavailable: true, NextRetryAfter: time.Now().Add(7 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Execute(context.Background(), []string{"codex"}, coreexecutor.Request{Model: "pool-model"}, coreexecutor.Options{})
	if err == nil {
		t.Fatal("expected busy pool")
	}
	err = enrichAuthSelectionError(err, []string{"codex"}, "pool-model")
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	NewBaseAPIHandlers(nil, manager).WriteErrorResponse(ctx, executionErrorMessage(err))
	if rec.Code != 529 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("response: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	var body struct {
		Error struct{ Type, Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "upstream_pool_busy" || body.Error.Code != "pool_at_capacity" || body.Error.Message != "codex pool at capacity, retry later" {
		t.Fatalf("body: %s", rec.Body.String())
	}
	if manager.CodexPoolSnapshot().FastFailures != 1 {
		t.Fatal("fast failure must be counted once")
	}
}
