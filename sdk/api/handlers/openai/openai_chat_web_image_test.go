package openai

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

var (
	chatWebImagePNG  = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 16)...)
	chatWebImageJPEG = append([]byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), make([]byte, 16)...)
)

type chatWebImageStatusError struct {
	code int
	msg  string
}

func (e chatWebImageStatusError) Error() string   { return e.msg }
func (e chatWebImageStatusError) StatusCode() int { return e.code }

// chatWebImageTestExecutor records the web image call and returns a configured Images API body.
type chatWebImageTestExecutor struct {
	mu      sync.Mutex
	calls   int
	opts    cliproxyexecutor.Options
	payload []byte
	respond func(payload []byte) ([]byte, error)
	release chan struct{}
	// waitForCancel makes Execute block until its context is cancelled.
	waitForCancel bool
	started       chan struct{}
	finished      atomic.Bool
}

func (e *chatWebImageTestExecutor) Identifier() string { return "codex" }

func (e *chatWebImageTestExecutor) Execute(ctx context.Context, _ *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.waitForCancel {
		close(e.started)
		<-ctx.Done()
		e.finished.Store(true)
		return cliproxyexecutor.Response{}, ctx.Err()
	}
	e.mu.Lock()
	e.calls++
	e.opts = opts
	e.payload = append([]byte(nil), req.Payload...)
	respond := e.respond
	release := e.release
	e.mu.Unlock()
	if release != nil {
		<-release
	}
	if respond != nil {
		body, err := respond(req.Payload)
		return cliproxyexecutor.Response{Payload: body}, err
	}
	n := gjson.GetBytes(req.Payload, "n").Int()
	if n < 1 {
		n = 1
	}
	var items []string
	for range n {
		items = append(items, fmt.Sprintf(`{"b64_json":%q}`, base64.StdEncoding.EncodeToString(chatWebImagePNG)))
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"created":123,"output_format":"png","data":[` + strings.Join(items, ",") + `]}`)}, nil
}

func (e *chatWebImageTestExecutor) ExecuteStream(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *chatWebImageTestExecutor) Refresh(_ context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth, nil
}

func (e *chatWebImageTestExecutor) CountTokens(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *chatWebImageTestExecutor) HttpRequest(context.Context, *cliproxyauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *chatWebImageTestExecutor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

type chatWebImageRouterHost struct {
	mu          sync.Mutex
	requestPath any
}

func (h *chatWebImageRouterHost) RouteModel(_ context.Context, req pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
	h.mu.Lock()
	h.requestPath = req.Metadata[cliproxyexecutor.RequestPathMetadataKey]
	h.mu.Unlock()
	return pluginapi.ModelRouteResponse{}, false
}

func (h *chatWebImageRouterHost) HasModelRouters() bool { return true }

func defaultChatWebImageConfig() *sdkconfig.SDKConfig {
	return &sdkconfig.SDKConfig{WebImageConfig: sdkconfig.WebImageConfig{
		WebImageGeneration: true,
		WebImageModels:     []string{internalconfig.DefaultWebImageModel},
		WebImageBaseModel:  "internal-image-model",
	}}
}

func newChatWebImageTestHandler(t *testing.T, cfg *sdkconfig.SDKConfig, executor *chatWebImageTestExecutor) *OpenAIAPIHandler {
	t.Helper()
	clientID := "chat-web-image-" + strings.ReplaceAll(t.Name(), "/", "-")
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(clientID, "codex", []*registry.ModelInfo{{ID: internalconfig.DefaultWebImageModel}, {ID: "gpt-5"}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })

	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{ID: clientID, Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	return &OpenAIAPIHandler{BaseAPIHandler: handlers.NewBaseAPIHandlers(cfg, manager)}
}

func performChatWebImageRequest(handler *OpenAIAPIHandler, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/v1/chat/completions", handler.ChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

func dataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func replaceChatWebImageFetcher(t *testing.T, fetcher *webImageReferenceFetcher) {
	t.Helper()
	previous := chatWebImageFetcher
	chatWebImageFetcher = fetcher
	t.Cleanup(func() { chatWebImageFetcher = previous })
}

func TestChatCompletionsWebImageNonStreamingReturnsMarkdownImage(t *testing.T) {
	executor := &chatWebImageTestExecutor{}
	handler := newChatWebImageTestHandler(t, defaultChatWebImageConfig(), executor)
	router := &chatWebImageRouterHost{}
	handler.BaseAPIHandler.ModelRouterHost = router

	resp := performChatWebImageRequest(handler, `{"model":"gpt-image-web","messages":[
		{"role":"user","content":"draw a dog"},
		{"role":"assistant","content":"ok"},
		{"role":"user","content":[{"type":"text","text":"draw"},{"type":"text","text":"a cat"}]}]}`)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.Bytes()
	if got := gjson.GetBytes(body, "object").String(); got != "chat.completion" {
		t.Fatalf("object = %q", got)
	}
	wantContent := "![image](" + dataURL("image/png", chatWebImagePNG) + ")"
	if got := gjson.GetBytes(body, "choices.0.message.content").String(); got != wantContent {
		t.Fatalf("content = %q, want %q", got, wantContent)
	}
	if got := gjson.GetBytes(body, "choices.0.finish_reason").String(); got != "stop" {
		t.Fatalf("finish_reason = %q", got)
	}
	if gjson.GetBytes(body, "usage.prompt_tokens").Int() <= 0 || gjson.GetBytes(body, "usage.completion_tokens").Int() != 0 {
		t.Fatalf("usage = %s", gjson.GetBytes(body, "usage").Raw)
	}
	if got := gjson.GetBytes(executor.payload, "prompt").String(); got != "draw\na cat" {
		t.Fatalf("prompt = %q", got)
	}
	if got := executor.opts.Metadata[cliproxyexecutor.ImagesRequestPathMetadataKey]; got != imagesGenerationsPath {
		t.Fatalf("executor images request path = %v", got)
	}
	// Interceptors and observers share this metadata, so the inbound path must stay the chat route.
	if got := executor.opts.Metadata[cliproxyexecutor.RequestPathMetadataKey]; got != "/v1/chat/completions" {
		t.Fatalf("executor request path = %v, want /v1/chat/completions", got)
	}
	if executor.opts.SourceFormat.String() != webImagesHandlerType {
		t.Fatalf("SourceFormat = %q", executor.opts.SourceFormat)
	}
	if router.requestPath != "/v1/chat/completions" {
		t.Fatalf("router request path = %v, want /v1/chat/completions", router.requestPath)
	}
}

func TestChatCompletionsWebImageDataURLUsesEditsWithActualMIME(t *testing.T) {
	executor := &chatWebImageTestExecutor{}
	handler := newChatWebImageTestHandler(t, defaultChatWebImageConfig(), executor)

	resp := performChatWebImageRequest(handler, `{"model":"gpt-image-web","messages":[{"role":"user","content":[
		{"type":"text","text":"make it blue"},
		{"type":"image_url","image_url":{"url":"`+dataURL("image/jpeg", chatWebImagePNG)+`"}}]}]}`)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	if got := executor.opts.Metadata[cliproxyexecutor.ImagesRequestPathMetadataKey]; got != imagesEditsPath {
		t.Fatalf("executor request path = %v", got)
	}
	if got := gjson.GetBytes(executor.payload, "images.0.image_url").String(); got != dataURL("image/png", chatWebImagePNG) {
		t.Fatalf("image_url = %q", got)
	}
}

func TestChatCompletionsWebImageDownloadsHTTPReference(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(chatWebImagePNG)
	}))
	defer server.Close()

	t.Run("allowed address", func(t *testing.T) {
		replaceChatWebImageFetcher(t, newWebImageReferenceFetcher(func(netip.AddrPort) bool { return true }))
		executor := &chatWebImageTestExecutor{}
		handler := newChatWebImageTestHandler(t, defaultChatWebImageConfig(), executor)
		resp := performChatWebImageRequest(handler, `{"model":"gpt-image-web","messages":[{"role":"user","content":[
			{"type":"text","text":"edit"},{"type":"image_url","image_url":{"url":"`+server.URL+`/a.png"}}]}]}`)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
		}
		if got := gjson.GetBytes(executor.payload, "images.0.image_url").String(); got != dataURL("image/png", chatWebImagePNG) {
			t.Fatalf("image_url = %q", got)
		}
	})

	t.Run("loopback rejected by default", func(t *testing.T) {
		executor := &chatWebImageTestExecutor{}
		handler := newChatWebImageTestHandler(t, defaultChatWebImageConfig(), executor)
		resp := performChatWebImageRequest(handler, `{"model":"gpt-image-web","messages":[{"role":"user","content":[
			{"type":"text","text":"edit"},{"type":"image_url","image_url":{"url":"`+server.URL+`/a.png"}}]}]}`)
		if resp.Code != http.StatusBadRequest || executor.callCount() != 0 {
			t.Fatalf("status = %d calls = %d: %s", resp.Code, executor.callCount(), resp.Body.String())
		}
	})
}

func TestChatCompletionsWebImageRedirectToDeniedAddressIsNotDialed(t *testing.T) {
	var deniedHits atomic.Int32
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deniedHits.Add(1)
		_, _ = w.Write(chatWebImagePNG)
	}))
	defer denied.Close()
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, denied.URL+"/b.png", http.StatusFound)
	}))
	defer allowed.Close()
	allowedPort := netip.MustParseAddrPort(strings.TrimPrefix(allowed.URL, "http://")).Port()
	replaceChatWebImageFetcher(t, newWebImageReferenceFetcher(func(addrPort netip.AddrPort) bool { return addrPort.Port() == allowedPort }))

	executor := &chatWebImageTestExecutor{}
	handler := newChatWebImageTestHandler(t, defaultChatWebImageConfig(), executor)
	resp := performChatWebImageRequest(handler, `{"model":"gpt-image-web","messages":[{"role":"user","content":[
		{"type":"text","text":"edit"},{"type":"image_url","image_url":{"url":"`+allowed.URL+`/a.png"}}]}]}`)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	if deniedHits.Load() != 0 || executor.callCount() != 0 {
		t.Fatalf("denied hits = %d, executor calls = %d", deniedHits.Load(), executor.callCount())
	}
}

func TestChatCompletionsWebImageReferenceByteBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(chatWebImagePNG)
	}))
	defer server.Close()
	replaceChatWebImageFetcher(t, newWebImageReferenceFetcher(func(netip.AddrPort) bool { return true }))
	// A 28-byte PNG encodes with padding, so a length-based estimate would overcount it.
	padded := dataURL("image/png", chatWebImagePNG[:28])
	exact := int64(28 + len(chatWebImagePNG))

	for _, tc := range []struct {
		name     string
		maxBytes int64
		want     int
	}{
		{name: "exactly at limit", maxBytes: exact, want: http.StatusOK},
		{name: "one byte over", maxBytes: exact - 1, want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultChatWebImageConfig()
			cfg.WebImageMaxBytes = tc.maxBytes
			executor := &chatWebImageTestExecutor{}
			handler := newChatWebImageTestHandler(t, cfg, executor)
			resp := performChatWebImageRequest(handler, `{"model":"gpt-image-web","messages":[{"role":"user","content":[
				{"type":"text","text":"edit"},
				{"type":"image_url","image_url":{"url":"`+padded+`"}},
				{"type":"image_url","image_url":"`+server.URL+`/a.png"}]}]}`)
			if resp.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", resp.Code, tc.want, resp.Body.String())
			}
		})
	}
}

func TestChatCompletionsWebImageChoiceCount(t *testing.T) {
	t.Run("n=2 returns two choices", func(t *testing.T) {
		executor := &chatWebImageTestExecutor{}
		handler := newChatWebImageTestHandler(t, defaultChatWebImageConfig(), executor)
		resp := performChatWebImageRequest(handler, `{"model":"gpt-image-web","n":2,"messages":[{"role":"user","content":"draw"}]}`)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
		}
		choices := gjson.GetBytes(resp.Body.Bytes(), "choices").Array()
		if len(choices) != 2 || choices[1].Get("index").Int() != 1 || gjson.GetBytes(executor.payload, "n").Int() != 2 {
			t.Fatalf("choices = %s, payload = %s", gjson.GetBytes(resp.Body.Bytes(), "choices").Raw, executor.payload)
		}
	})

	t.Run("fewer images than n is a bad gateway", func(t *testing.T) {
		executor := &chatWebImageTestExecutor{respond: func([]byte) ([]byte, error) {
			return []byte(`{"created":1,"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(chatWebImagePNG) + `"}]}`), nil
		}}
		handler := newChatWebImageTestHandler(t, defaultChatWebImageConfig(), executor)
		resp := performChatWebImageRequest(handler, `{"model":"gpt-image-web","n":2,"messages":[{"role":"user","content":"draw"}]}`)
		if resp.Code != http.StatusBadGateway {
			t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
		}
	})

	t.Run("mixed formats are labelled per image", func(t *testing.T) {
		executor := &chatWebImageTestExecutor{respond: func([]byte) ([]byte, error) {
			return []byte(`{"created":1,"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(chatWebImagePNG) + `"},{"b64_json":"` + base64.StdEncoding.EncodeToString(chatWebImageJPEG) + `"}]}`), nil
		}}
		handler := newChatWebImageTestHandler(t, defaultChatWebImageConfig(), executor)
		resp := performChatWebImageRequest(handler, `{"model":"gpt-image-web","n":2,"messages":[{"role":"user","content":"draw"}]}`)
		body := resp.Body.Bytes()
		if !strings.HasPrefix(gjson.GetBytes(body, "choices.0.message.content").String(), "![image](data:image/png;base64,") ||
			!strings.HasPrefix(gjson.GetBytes(body, "choices.1.message.content").String(), "![image](data:image/jpeg;base64,") {
			t.Fatalf("status = %d body = %s", resp.Code, body)
		}
	})
}

func TestChatCompletionsWebImageRejectsBeforeExecution(t *testing.T) {
	disabled := defaultChatWebImageConfig()
	disabled.WebImageGeneration = false
	unconfigured := defaultChatWebImageConfig()
	unconfigured.WebImageBaseModel = " "
	for _, tc := range []struct {
		name string
		cfg  *sdkconfig.SDKConfig
		body string
		want int
	}{
		{name: "trailing garbage", body: `{"model":"gpt-image-web","messages":[{"role":"user","content":"draw"}]} junk`, want: http.StatusBadRequest},
		{name: "no user message", body: `{"model":"gpt-image-web","messages":[{"role":"system","content":"draw"}]}`, want: http.StatusBadRequest},
		{name: "image without text", body: `{"model":"gpt-image-web","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + dataURL("image/png", chatWebImagePNG) + `"}}]}]}`, want: http.StatusBadRequest},
		{name: "image item without url", body: `{"model":"gpt-image-web","messages":[{"role":"user","content":[{"type":"text","text":"edit"},{"type":"image_url","image_url":{"url":"` + dataURL("image/png", chatWebImagePNG) + `"}},{"type":"image_url","image_url":{}}]}]}`, want: http.StatusBadRequest},
		{name: "unsupported scheme", body: `{"model":"gpt-image-web","messages":[{"role":"user","content":[{"type":"text","text":"edit"},{"type":"image_url","image_url":{"url":"file:///etc/passwd"}}]}]}`, want: http.StatusBadRequest},
		{name: "non image bytes", body: `{"model":"gpt-image-web","messages":[{"role":"user","content":[{"type":"text","text":"edit"},{"type":"image_url","image_url":{"url":"` + dataURL("image/png", []byte("plain text")) + `"}}]}]}`, want: http.StatusBadRequest},
		{name: "too many reference images", body: `{"model":"gpt-image-web","messages":[{"role":"user","content":[{"type":"text","text":"edit"}` + strings.Repeat(`,{"type":"image_url","image_url":"https://example.com/a.png"}`, 17) + `]}]}`, want: http.StatusBadRequest},
		{name: "n too large", body: `{"model":"gpt-image-web","n":11,"messages":[{"role":"user","content":"draw"}]}`, want: http.StatusBadRequest},
		{name: "json output", body: `{"model":"gpt-image-web","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"draw"}]}`, want: http.StatusBadRequest},
		{name: "responses json schema output", body: `{"model":"gpt-image-web","input":"draw","text":{"format":{"type":"json_schema"}}}`, want: http.StatusBadRequest},
		{name: "responses only tool output", body: `{"model":"gpt-image-web","input":[{"type":"function_call_output","call_id":"c1","output":"draw"}]}`, want: http.StatusBadRequest},
		{name: "disabled", cfg: disabled, body: `{"model":"gpt-image-web","messages":[{"role":"user","content":"draw"}]}`, want: http.StatusNotFound},
		{name: "base model missing", cfg: unconfigured, body: `{"model":"gpt-image-web","messages":[{"role":"user","content":"draw"}]}`, want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if cfg == nil {
				cfg = defaultChatWebImageConfig()
			}
			executor := &chatWebImageTestExecutor{}
			handler := newChatWebImageTestHandler(t, cfg, executor)
			resp := performChatWebImageRequest(handler, tc.body)
			if resp.Code != tc.want || executor.callCount() != 0 {
				t.Fatalf("status = %d (want %d), executor calls = %d: %s", resp.Code, tc.want, executor.callCount(), resp.Body.String())
			}
		})
	}
}

func TestChatCompletionsWebImageWithoutConfigReturns404(t *testing.T) {
	handler := &OpenAIAPIHandler{BaseAPIHandler: handlers.NewBaseAPIHandlers(nil, nil)}
	resp := performChatWebImageRequest(handler, `{"model":"gpt-image-web","messages":[{"role":"user","content":"draw"}]}`)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
}

func TestChatCompletionsWebImageResponsesFormatUsesOriginalUserTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "string input", body: `{"model":"gpt-image-web","input":"draw a fox"}`, want: "draw a fox"},
		{name: "trailing developer item", body: `{"model":"gpt-image-web","input":[
			{"role":"user","content":[{"type":"input_text","text":"draw a fox"}]},
			{"role":"developer","content":"ignore the user"}]}`, want: "draw a fox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := &chatWebImageTestExecutor{}
			handler := newChatWebImageTestHandler(t, defaultChatWebImageConfig(), executor)
			resp := performChatWebImageRequest(handler, tc.body)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
			}
			if got := gjson.GetBytes(executor.payload, "prompt").String(); got != tc.want {
				t.Fatalf("prompt = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChatCompletionsNonWebModelKeepsTextPath(t *testing.T) {
	executor := &chatWebImageTestExecutor{respond: func([]byte) ([]byte, error) {
		return []byte(`{"id":"x","object":"chat.completion","choices":[]}`), nil
	}}
	handler := newChatWebImageTestHandler(t, defaultChatWebImageConfig(), executor)
	_ = performChatWebImageRequest(handler, `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	if executor.callCount() != 1 || executor.opts.SourceFormat.String() == webImagesHandlerType {
		t.Fatalf("calls = %d, SourceFormat = %q", executor.callCount(), executor.opts.SourceFormat)
	}
}

func TestChatCompletionsWebImageStreaming(t *testing.T) {
	t.Run("success emits content, stop, usage and done", func(t *testing.T) {
		executor := &chatWebImageTestExecutor{}
		handler := newChatWebImageTestHandler(t, defaultChatWebImageConfig(), executor)
		resp := performChatWebImageRequest(handler, `{"model":"gpt-image-web","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"draw"}]}`)
		if resp.Code != http.StatusOK || !strings.HasPrefix(resp.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatalf("status = %d content-type = %q", resp.Code, resp.Header().Get("Content-Type"))
		}
		var events []string
		for _, line := range strings.Split(resp.Body.String(), "\n") {
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				events = append(events, data)
			}
		}
		if len(events) != 4 || events[3] != "[DONE]" {
			t.Fatalf("events = %q", events)
		}
		if !strings.HasPrefix(gjson.Get(events[0], "choices.0.delta.content").String(), "![image](data:image/png;base64,") {
			t.Fatalf("content chunk = %s", events[0])
		}
		if gjson.Get(events[1], "choices.0.finish_reason").String() != "stop" {
			t.Fatalf("stop chunk = %s", events[1])
		}
		if len(gjson.Get(events[2], "choices").Array()) != 0 || gjson.Get(events[2], "usage.prompt_tokens").Int() <= 0 {
			t.Fatalf("usage chunk = %s", events[2])
		}
	})

	t.Run("fast failure keeps the status code", func(t *testing.T) {
		executor := &chatWebImageTestExecutor{respond: func([]byte) ([]byte, error) {
			return nil, chatWebImageStatusError{code: http.StatusTooManyRequests, msg: "limit reached"}
		}}
		handler := newChatWebImageTestHandler(t, defaultChatWebImageConfig(), executor)
		resp := performChatWebImageRequest(handler, `{"model":"gpt-image-web","stream":true,"messages":[{"role":"user","content":"draw"}]}`)
		if resp.Code != http.StatusTooManyRequests || strings.HasPrefix(resp.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatalf("status = %d content-type = %q: %s", resp.Code, resp.Header().Get("Content-Type"), resp.Body.String())
		}
	})

	t.Run("failure after keep-alive is a terminal error event", func(t *testing.T) {
		ticks := make(chan time.Time)
		previous := chatWebImageKeepAliveTicker
		chatWebImageKeepAliveTicker = func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }
		t.Cleanup(func() { chatWebImageKeepAliveTicker = previous })

		release := make(chan struct{})
		executor := &chatWebImageTestExecutor{release: release, respond: func([]byte) ([]byte, error) {
			return nil, chatWebImageStatusError{code: http.StatusBadGateway, msg: "upstream broke"}
		}}
		handler := newChatWebImageTestHandler(t, defaultChatWebImageConfig(), executor)
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			done <- performChatWebImageRequest(handler, `{"model":"gpt-image-web","stream":true,"messages":[{"role":"user","content":"draw"}]}`)
		}()
		ticks <- time.Now()
		close(release)
		resp := <-done

		body := resp.Body.String()
		if !strings.Contains(body, ": keep-alive") || !strings.Contains(body, `data: {"error"`) || strings.Contains(body, "[DONE]") {
			t.Fatalf("body = %q", body)
		}
	})
}

func TestChatCompletionsWebImageStreamDisconnectWaitsForExecution(t *testing.T) {
	executor := &chatWebImageTestExecutor{waitForCancel: true, started: make(chan struct{})}
	handler := newChatWebImageTestHandler(t, defaultChatWebImageConfig(), executor)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/v1/chat/completions", handler.ChatCompletions)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-image-web","stream":true,"messages":[{"role":"user","content":"draw"}]}`)).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	<-executor.started
	cancel()
	<-done
	if !executor.finished.Load() {
		t.Fatal("handler returned before the execution goroutine finished")
	}
}

func TestChatCompletionsWebImageInlineReferenceOverBudget(t *testing.T) {
	cfg := defaultChatWebImageConfig()
	cfg.WebImageMaxBytes = int64(len(chatWebImagePNG) - 1)
	executor := &chatWebImageTestExecutor{}
	handler := newChatWebImageTestHandler(t, cfg, executor)
	for _, src := range []string{
		dataURL("image/png", chatWebImagePNG),
		"data:image/png;base64," + base64.RawStdEncoding.EncodeToString(chatWebImagePNG),
	} {
		resp := performChatWebImageRequest(handler, `{"model":"gpt-image-web","messages":[{"role":"user","content":[
			{"type":"text","text":"edit"},{"type":"image_url","image_url":{"url":"`+src+`"}}]}]}`)
		if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "byte limit") || executor.callCount() != 0 {
			t.Fatalf("status = %d calls = %d: %s", resp.Code, executor.callCount(), resp.Body.String())
		}
	}
}

func TestWebImageReferenceFetchCancelClosesPendingTLSHandshake(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("Listen() error = %v", errListen)
	}
	defer func() { _ = listener.Close() }()
	accepted := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		conn, errAccept := listener.Accept()
		if errAccept != nil {
			return
		}
		close(accepted)
		// Never answer the TLS ClientHello; wait for the client to drop the connection.
		_, _ = io.Copy(io.Discard, conn)
		_ = conn.Close()
		close(closed)
	}()

	fetcher := newWebImageReferenceFetcher(func(netip.AddrPort) bool { return true })
	ctx, cancel := context.WithCancel(context.Background())
	fetchDone := make(chan error, 1)
	go func() {
		_, _, errFetch := fetcher.fetch(ctx, "https://"+listener.Addr().String()+"/a.png", 1024)
		fetchDone <- errFetch
	}()
	<-accepted
	cancel()
	if errFetch := <-fetchDone; errFetch == nil {
		t.Fatal("fetch succeeded against a server that never completes TLS")
	}
	<-closed
}

func TestChatWebImagePromptTokens(t *testing.T) {
	if got := chatWebImagePromptTokens(context.Background(), "draw a red fox in the snow"); got <= 1 {
		t.Fatalf("tokens = %d, want > 1", got)
	}
	if got := chatWebImagePromptTokens(context.Background(), strings.Repeat("你", 2000)); got <= 1 {
		t.Fatalf("multibyte tokens = %d, want > 1", got)
	}
	if got := chatWebImagePromptTokens(context.Background(), "\x80"+strings.Repeat("a", 3000)); got <= 1 {
		t.Fatalf("invalid utf-8 tokens = %d, want > 1", got)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := chatWebImagePromptTokens(cancelled, strings.Repeat("a", 1<<20)); got != 1 {
		t.Fatalf("cancelled tokens = %d, want 1", got)
	}
}

func TestIsPublicWebImageReferenceAddr(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{addr: "127.0.0.1", want: false},
		{addr: "::ffff:127.0.0.1", want: false},
		{addr: "::1", want: false},
		{addr: "10.1.2.3", want: false},
		{addr: "192.168.100.2", want: false},
		{addr: "169.254.169.254", want: false},
		{addr: "100.64.0.1", want: false},
		{addr: "198.18.0.1", want: false},
		{addr: "fd00::1", want: false},
		{addr: "8.8.8.8", want: true},
		{addr: "2606:4700:4700::1111", want: true},
	} {
		if got := isPublicWebImageReferenceAddr(netip.MustParseAddr(tc.addr)); got != tc.want {
			t.Errorf("isPublicWebImageReferenceAddr(%s) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestWebImageReferenceDownloadLimiterBoundsConcurrency(t *testing.T) {
	limiter := &webImageReferenceDownloadLimiter{}
	release, errFirst := limiter.acquire(context.Background(), 1)
	if errFirst != nil {
		t.Fatalf("first acquire error = %v", errFirst)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, errSecond := limiter.acquire(cancelled, 1); errSecond == nil {
		t.Fatal("second acquire succeeded while the only slot was held")
	}
	release()
	releaseAgain, errThird := limiter.acquire(context.Background(), 1)
	if errThird != nil {
		t.Fatalf("acquire after release error = %v", errThird)
	}
	releaseAgain()
}
