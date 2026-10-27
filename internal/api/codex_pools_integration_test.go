package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexPoolsHTTPStreamingWithOAuthFileStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upstreamCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"server_is_overloaded","message":"fixture overload"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"resp_fixture","model":"pool-http-model"}}`,
			`{"type":"response.output_item.added","item":{"id":"msg_fixture","type":"message","role":"assistant","content":[]},"output_index":0}`,
			`{"type":"response.output_text.delta","item_id":"msg_fixture","output_index":0,"content_index":0,"delta":"hello"}`,
			`{"type":"response.completed","response":{"id":"resp_fixture","status":"completed","output":[{"id":"msg_fixture","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"fixture-key"}}, AuthDir: dir, Codex: config.CodexConfig{Pools: config.CodexPoolsConfig{Enable: true}, StreamBootstrapBuffering: true}}
	manager := coreauth.NewManager(store, coreauth.NewSessionAffinitySelector(&coreauth.RoundRobinSelector{}), nil)
	manager.SetConfig(cfg)
	manager.SetRetryConfig(1, 6, 0)
	manager.SetAsyncSuccessResults(true)
	defer func() {
		_ = manager.StopSuccessResults(context.Background())
		_ = manager.FlushCodexPools(context.Background())
		manager.StopAutoRefresh()
	}()
	manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(cfg))
	for i := range 8 {
		id := fmt.Sprintf("pool-http-auth-%d", i)
		_, err := manager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "codex", FileName: id + ".json", Attributes: map[string]string{"base_url": upstream.URL, coreauth.AttributeAuthKind: coreauth.AuthKindOAuth, coreauth.AttributeSourceBackend: coreauth.AuthSourceFile, coreauth.AttributePath: filepath.Join(dir, id+".json")}, Metadata: map[string]any{"type": "codex", "access_token": "fixture-token", "refresh_token": "fixture-refresh"}})
		if err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "pool-http-model"}})
		defer registry.GetGlobalRegistry().UnregisterClient(id)
		manager.RefreshSchedulerEntry(id)
	}
	server := NewServer(cfg, manager, sdkaccess.NewManager(), filepath.Join(dir, "config.yaml"))
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"pool-http-model","input":"hello","stream":true}`))
			request.Header.Set("Authorization", "Bearer fixture-key")
			request.Header.Set("Session-Id", fmt.Sprintf("fixture-session-%d", i))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.engine.ServeHTTP(response, request)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "response.completed") {
				t.Errorf("response=%d %s", response.Code, response.Body.String())
			}
		}(i)
	}
	wg.Wait()
	if err := manager.StopSuccessResults(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state := manager.CodexPoolsSnapshot(); !state.Enabled || len(state.Pools) < 2 {
		t.Fatalf("pool state=%+v", state)
	}
	var successes int64
	for _, a := range manager.List() {
		successes += a.Success
	}
	if successes != 32 {
		t.Fatalf("successful results=%d want32", successes)
	}
	if files, err := os.ReadDir(dir); err != nil || len(files) < 8 {
		t.Fatalf("OAuth files missing: %v %d", err, len(files))
	}
}
