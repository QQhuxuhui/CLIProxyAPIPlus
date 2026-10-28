package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type wireRetryExecutor struct {
	overloadStreamExecutor
	quota bool
	calls int
}

func (e *wireRetryExecutor) next() error {
	e.calls++
	if e.quota && e.calls == 1 {
		return &coreauth.Error{HTTPStatus: 429, Message: `{"error":{"type":"rate_limit_error","message":"quota"}}`}
	}
	return &coreauth.Error{HTTPStatus: 503, Message: `{"error":{"code":"server_is_overloaded"}}`}
}
func (e *wireRetryExecutor) Execute(context.Context, *coreauth.Auth, exec.Request, exec.Options) (exec.Response, error) {
	return exec.Response{}, e.next()
}
func (e *wireRetryExecutor) ExecuteStream(context.Context, *coreauth.Auth, exec.Request, exec.Options) (*exec.StreamResult, error) {
	ch := make(chan exec.StreamChunk, 1)
	ch <- exec.StreamChunk{Err: e.next()}
	close(ch)
	return &exec.StreamResult{Headers: http.Header{"Retry-After": {"999"}}, Chunks: ch}, nil
}
func TestCodexRetryWireHeaders(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, passthrough := range []bool{false, true} {
			for _, quota := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%v/pass=%v/quota=%v", stream, passthrough, quota), func(t *testing.T) {
					m := coreauth.NewManager(nil, nil, nil)
					cfg := &config.Config{}
					cfg.Codex.RetryBudget.Enable = true
					m.SetConfig(cfg)
					m.RegisterExecutor(&wireRetryExecutor{quota: quota})
					model := "wire-" + t.Name()
					for i := range 2 {
						id := fmt.Sprintf("%s-%d", model, i)
						_, err := m.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "codex"})
						if err != nil {
							t.Fatal(err)
						}
						registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					}
					h := NewBaseAPIHandlers(&config.SDKConfig{PassthroughHeaders: passthrough}, m)
					var msg *interfaces.ErrorMessage
					if stream {
						_, _, errors := h.ExecuteStreamWithAuthManager(context.Background(), "openai", model, []byte(`{}`), "")
						for item := range errors {
							msg = item
						}
					} else {
						_, _, msg = h.ExecuteWithAuthManager(context.Background(), "openai", model, []byte(`{}`), "")
					}
					if msg == nil {
						t.Fatal("missing final error")
					}
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
					h.WriteErrorResponse(c, msg)
					want := 529
					if quota {
						want = 429
					}
					seconds, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
					if recorder.Code != want || err != nil || seconds < 1 || seconds >= 999 {
						t.Fatalf("status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
					}
					if !quota && (seconds != 1 || !strings.Contains(recorder.Body.String(), "upstream_pool_busy")) {
						t.Fatalf("overload envelope=%s headers=%v", recorder.Body.String(), recorder.Header())
					}
				})
			}
		}
	}
}
