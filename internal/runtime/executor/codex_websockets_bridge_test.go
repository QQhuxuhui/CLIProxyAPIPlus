package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func bridgeTestConfig(percent int) *config.Config {
	return &config.Config{Codex: config.CodexConfig{HTTPWebsocketBridge: config.CodexHTTPWebsocketBridgeConfig{Enable: true, Percent: percent}}}
}

func bridgeTestOAuth(baseURL string) *cliproxyauth.Auth {
	attrs := map[string]string{}
	if baseURL != "" {
		attrs["base_url"] = baseURL
	}
	return &cliproxyauth.Auth{
		ID:         "auth-bridge-1",
		Provider:   "codex",
		Attributes: attrs,
		Metadata:   map[string]any{"access_token": "oauth-token", "account_id": "acct-1"},
	}
}

func bridgeTestOpts() cliproxyexecutor.Options {
	return cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true}
}

func TestShouldBridgeCodexStreamToWebsocket(t *testing.T) {
	ctx := logging.WithRequestID(context.Background(), "req-1")
	imageOpts := bridgeTestOpts()
	imageOpts.Metadata = map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/images/generations"}
	compactOpts := bridgeTestOpts()
	compactOpts.Alt = "responses/compact"
	chatOpts := bridgeTestOpts()
	chatOpts.SourceFormat = sdktranslator.FormatOpenAI
	crossOpts := bridgeTestOpts()
	crossOpts.ResponseFormat = sdktranslator.FormatClaude
	apiKeyAuth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "sk-1"}}
	kindAPIKeyAuth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"auth_kind": "apikey"}, Metadata: map[string]any{"access_token": "tok"}}
	grokOpts := bridgeTestOpts()
	grokOpts.Headers = http.Header{"User-Agent": []string{"grok-shell/1.0"}}
	proxyAuth := bridgeTestOAuth("")
	proxyAuth.ProxyURL = "https://proxy.example.com:8443"
	proxyCfg := bridgeTestConfig(100)
	proxyCfg.ProxyURL = "socks5://127.0.0.1:1080"
	noTokenAuth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{}}

	tests := []struct {
		name string
		ctx  context.Context
		cfg  *config.Config
		auth *cliproxyauth.Auth
		opts cliproxyexecutor.Options
		want bool
	}{
		{name: "bridged", ctx: ctx, cfg: bridgeTestConfig(100), auth: bridgeTestOAuth(""), opts: bridgeTestOpts(), want: true},
		{name: "nil config", ctx: ctx, auth: bridgeTestOAuth(""), opts: bridgeTestOpts()},
		{name: "disabled", ctx: ctx, cfg: &config.Config{Codex: config.CodexConfig{HTTPWebsocketBridge: config.CodexHTTPWebsocketBridgeConfig{Percent: 100}}}, auth: bridgeTestOAuth(""), opts: bridgeTestOpts()},
		{name: "zero percent", ctx: ctx, cfg: bridgeTestConfig(0), auth: bridgeTestOAuth(""), opts: bridgeTestOpts()},
		{name: "downstream websocket", ctx: cliproxyexecutor.WithDownstreamWebsocket(ctx), cfg: bridgeTestConfig(100), auth: bridgeTestOAuth(""), opts: bridgeTestOpts()},
		{name: "chat completions", ctx: ctx, cfg: bridgeTestConfig(100), auth: bridgeTestOAuth(""), opts: chatOpts},
		{name: "responses input other output", ctx: ctx, cfg: bridgeTestConfig(100), auth: bridgeTestOAuth(""), opts: crossOpts},
		{name: "compact", ctx: ctx, cfg: bridgeTestConfig(100), auth: bridgeTestOAuth(""), opts: compactOpts},
		{name: "images endpoint", ctx: ctx, cfg: bridgeTestConfig(100), auth: bridgeTestOAuth(""), opts: imageOpts},
		{name: "api key", ctx: ctx, cfg: bridgeTestConfig(100), auth: apiKeyAuth, opts: bridgeTestOpts()},
		{name: "explicit api key kind", ctx: ctx, cfg: bridgeTestConfig(100), auth: kindAPIKeyAuth, opts: bridgeTestOpts()},
		{name: "no token", ctx: ctx, cfg: bridgeTestConfig(100), auth: noTokenAuth, opts: bridgeTestOpts()},
		{name: "nil auth", ctx: ctx, cfg: bridgeTestConfig(100), opts: bridgeTestOpts()},
		{name: "no request id below 100", ctx: context.Background(), cfg: bridgeTestConfig(99), auth: bridgeTestOAuth(""), opts: bridgeTestOpts()},
		{name: "no request id at 100", ctx: context.Background(), cfg: bridgeTestConfig(100), auth: bridgeTestOAuth(""), opts: bridgeTestOpts(), want: true},
		{name: "auth proxy", ctx: ctx, cfg: bridgeTestConfig(100), auth: proxyAuth, opts: bridgeTestOpts()},
		{name: "global proxy", ctx: ctx, cfg: proxyCfg, auth: bridgeTestOAuth(""), opts: bridgeTestOpts()},
		{name: "injected round tripper", ctx: context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(http.DefaultTransport)), cfg: bridgeTestConfig(100), auth: bridgeTestOAuth(""), opts: bridgeTestOpts()},
		{name: "grok client", ctx: ctx, cfg: bridgeTestConfig(100), auth: bridgeTestOAuth(""), opts: grokOpts},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldBridgeCodexStreamToWebsocket(tt.ctx, tt.cfg, tt.auth, tt.opts); got != tt.want {
				t.Fatalf("shouldBridgeCodexStreamToWebsocket() = %v, want %v", got, tt.want)
			}
		})
	}
}

type bridgeUpstream struct {
	server    *httptest.Server
	requests  chan []byte
	upgrades  atomic.Int32
	sseBodies atomic.Int32
}

// newBridgeUpstream serves the websocket frames on an upgrade request and the same frames as an
// HTTP SSE stream otherwise, so the HTTP and bridged paths can be compared on one input.
func newBridgeUpstream(t *testing.T, frames func(request []byte) []string) *bridgeUpstream {
	t.Helper()
	up := &bridgeUpstream{requests: make(chan []byte, 8)}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			up.sseBodies.Add(1)
			body := new(bytes.Buffer)
			_, _ = body.ReadFrom(r.Body)
			up.requests <- body.Bytes()
			w.Header().Set("Content-Type", "text/event-stream")
			for _, frame := range frames(body.Bytes()) {
				eventType := gjson.Get(frame, "type").String()
				if strings.HasPrefix(eventType, "codex.") || strings.HasPrefix(eventType, "responsesapi.") {
					continue
				}
				_, _ = w.Write([]byte("event: " + eventType + "\ndata: " + frame + "\n\n"))
			}
			return
		}
		up.upgrades.Add(1)
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade websocket: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			up.requests <- bytes.Clone(payload)
			for _, frame := range frames(payload) {
				if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(frame)); errWrite != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(up.server.Close)
	return up
}

const (
	bridgeRateLimitsFrame = `{"type":"codex.rate_limits","plan_type":"free","rate_limits":{"primary":{"used_percent":12,"window_minutes":300,"reset_at":1764000000}}}`
	bridgeMetadataFrame   = `{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"secret-turn-state","x-models-etag":"etag-1"}}`
	bridgeTimingFrame     = `{"type":"responsesapi.websocket_timing","connect_ms":12}`
	bridgeCreatedFrame    = `{"type":"response.created","response":{"id":"resp-1","model":"gpt-6-luna","status":"in_progress"}}`
	bridgeRespMetaFrame   = `{"type":"response.metadata","metadata":{"moderation":"ok"}}`
	bridgeDeltaFrame      = `{"type":"response.output_text.delta","item_id":"msg-1","output_index":0,"content_index":0,"delta":"hello"}`
	bridgeCompletedFrame  = `{"type":"response.completed","response":{"id":"resp-1","model":"gpt-6-luna","status":"completed","output":[],"usage":{"input_tokens":11,"input_tokens_details":{"cached_tokens":5},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":18},"tool_usage":{"image_gen":{"input_tokens":2,"output_tokens":40,"total_tokens":42}}}}`
)

func bridgeFullFrames([]byte) []string {
	return []string{bridgeRateLimitsFrame, bridgeMetadataFrame, bridgeTimingFrame, bridgeCreatedFrame, bridgeRespMetaFrame, bridgeDeltaFrame, bridgeCompletedFrame}
}

func collectBridgeChunks(t *testing.T, result *cliproxyexecutor.StreamResult) ([][]byte, error) {
	t.Helper()
	var chunks [][]byte
	timeout := time.After(5 * time.Second)
	for {
		select {
		case chunk, ok := <-result.Chunks:
			if !ok {
				return chunks, nil
			}
			if chunk.Err != nil {
				return chunks, chunk.Err
			}
			chunks = append(chunks, chunk.Payload)
		case <-timeout:
			t.Fatal("timed out waiting for stream")
			return nil, nil
		}
	}
}

func TestCodexWebsocketBridgeStreamFiltersAndFramesEvents(t *testing.T) {
	for _, buffering := range []bool{false, true} {
		t.Run("buffering="+map[bool]string{false: "off", true: "on"}[buffering], func(t *testing.T) {
			up := newBridgeUpstream(t, bridgeFullFrames)
			cfg := bridgeTestConfig(100)
			cfg.Codex.StreamBootstrapBuffering = buffering
			exec := NewCodexAutoExecutor(cfg)
			auth := bridgeTestOAuth(up.server.URL)
			auth.Attributes["service_tier"] = "priority"
			req := cliproxyexecutor.Request{
				Model:   "gpt-6-luna",
				Payload: []byte(`{"model":"gpt-6-luna","stream":true,"input":"hi","previous_response_id":"resp-0","generate":false,"prompt_cache_retention":"24h","safety_identifier":"sid","stream_options":{"include_obfuscation":false,"reasoning_summary_delivery":"inline"}}`),
			}
			result, errExec := exec.ExecuteStream(logging.WithRequestID(context.Background(), "req-1"), auth, req, bridgeTestOpts())
			if errExec != nil {
				t.Fatalf("ExecuteStream() error = %v", errExec)
			}
			if result.Headers != nil {
				t.Fatalf("bridged stream returned handshake headers: %v", result.Headers)
			}
			chunks, errStream := collectBridgeChunks(t, result)
			if errStream != nil {
				t.Fatalf("stream error = %v", errStream)
			}
			if up.upgrades.Load() != 1 || up.sseBodies.Load() != 0 {
				t.Fatalf("upgrades=%d sse=%d, want one websocket request", up.upgrades.Load(), up.sseBodies.Load())
			}

			var types []string
			for _, chunk := range chunks {
				text := string(chunk)
				if strings.Contains(text, "codex.") || strings.Contains(text, "responsesapi.") || strings.Contains(text, "secret-turn-state") {
					t.Fatalf("websocket-only event leaked downstream: %s", text)
				}
				lines := strings.SplitN(text, "\n", 2)
				if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
					t.Fatalf("chunk is not an event/data frame: %q", text)
				}
				eventType := strings.TrimPrefix(lines[0], "event: ")
				if got := gjson.Get(strings.TrimPrefix(lines[1], "data: "), "type").String(); got != eventType {
					t.Fatalf("event line %q does not match data type %q", eventType, got)
				}
				types = append(types, eventType)
			}
			want := []string{"response.created", "response.metadata", "response.output_text.delta", "response.completed"}
			if strings.Join(types, ",") != strings.Join(want, ",") {
				t.Fatalf("event types = %v, want %v", types, want)
			}

			upstreamReq := <-up.requests
			for _, field := range []string{"previous_response_id", "generate", "prompt_cache_retention", "safety_identifier", "stream_options.include_obfuscation"} {
				if gjson.GetBytes(upstreamReq, field).Exists() {
					t.Fatalf("upstream request still carries %s: %s", field, upstreamReq)
				}
			}
			if got := gjson.GetBytes(upstreamReq, "stream_options.reasoning_summary_delivery").String(); got != "inline" {
				t.Fatalf("reasoning_summary_delivery = %q, want inline", got)
			}
			if got := gjson.GetBytes(upstreamReq, "service_tier").String(); got != "priority" {
				t.Fatalf("service_tier = %q, want priority", got)
			}
		})
	}
}

func TestCodexWebsocketBridgeTimingFrameKeepsBootstrapFailover(t *testing.T) {
	up := newBridgeUpstream(t, func([]byte) []string {
		return []string{bridgeCreatedFrame, bridgeTimingFrame, codexOverloadEvent}
	})
	cfg := bridgeTestConfig(100)
	cfg.Codex.StreamBootstrapBuffering = true
	exec := NewCodexAutoExecutor(cfg)
	req := cliproxyexecutor.Request{Model: "gpt-6-luna", Payload: []byte(`{"model":"gpt-6-luna","stream":true,"input":"hi"}`)}
	result, errExec := exec.ExecuteStream(context.Background(), bridgeTestOAuth(up.server.URL), req, bridgeTestOpts())
	if errExec == nil {
		chunks, _ := collectBridgeChunks(t, result)
		t.Fatalf("ExecuteStream() committed the stream before the overload: %q", chunks)
	}
	var se statusErr
	if !errors.As(errExec, &se) || (se.StatusCode() != http.StatusServiceUnavailable && se.StatusCode() != http.StatusTooManyRequests) {
		t.Fatalf("ExecuteStream() error = %T %v, want bootstrap overload", errExec, errExec)
	}
}

func TestParseCodexWebsocketStreamErrorOnBridge(t *testing.T) {
	type headerCarrier interface{ Headers() http.Header }

	t.Run("rate limit uses http cooldown and drops headers", func(t *testing.T) {
		payload := []byte(`{"type":"error","status":429,"error":{"type":"rate_limit_error","message":"slow down"},"headers":{"retry-after":"30","x-secret":"v"}}`)
		before := time.Now()
		err, ok := parseCodexWebsocketStreamError(true, payload, false)
		if !ok {
			t.Fatal("error frame not parsed")
		}
		if _, carries := err.(headerCarrier); carries {
			t.Fatalf("bridged error %T exposes headers", err)
		}
		se, isStatus := err.(statusErr)
		if !isStatus {
			t.Fatalf("error type = %T, want statusErr", err)
		}
		quota, hasQuota := se.QuotaDetail()
		if !hasQuota || !se.IsCredentialScoped() {
			t.Fatalf("quota=%v scoped=%v, want account-wide cooldown", hasQuota, se.IsCredentialScoped())
		}
		if quota.ResetAt.Before(before.Add(29*time.Second)) || quota.ResetAt.After(time.Now().Add(31*time.Second)) {
			t.Fatalf("ResetAt = %v, want about now+30s", quota.ResetAt)
		}
		nativeErr, _ := parseCodexWebsocketStreamError(false, payload, false)
		if _, carries := nativeErr.(headerCarrier); !carries {
			t.Fatalf("downstream websocket error %T lost its headers", nativeErr)
		}
	})

	t.Run("connection limit keeps websocket classification", func(t *testing.T) {
		for _, payload := range []string{
			`{"type":"error","status":429,"error":{"code":"websocket_connection_limit_reached"}}`,
			`{"type":"error","status":429,"error":{"code":"websocket_connection_limit_reached"},"headers":{"retry-after":"30"}}`,
		} {
			err, ok := parseCodexWebsocketStreamError(true, []byte(payload), false)
			if !ok {
				t.Fatal("error frame not parsed")
			}
			if _, carries := err.(headerCarrier); carries {
				t.Fatalf("bridged error %T exposes headers", err)
			}
			se, isStatus := err.(statusErr)
			if !isStatus {
				t.Fatalf("error type = %T, want statusErr", err)
			}
			if _, hasQuota := se.QuotaDetail(); hasQuota || se.IsCredentialScoped() {
				t.Fatalf("connection limit became an account-wide cooldown: %+v", se)
			}
			if retry := se.RetryAfter(); retry == nil || *retry != 0 {
				t.Fatalf("RetryAfter = %v, want 0", retry)
			}
		}
	})

	t.Run("status and body follow the http contract", func(t *testing.T) {
		for _, tc := range []struct {
			payload string
			status  int
		}{
			{payload: `{"type":"error","status":503,"error":{"message":"Selected model is at capacity. Please try a different model."}}`, status: 503},
			{payload: `{"type":"error","status":401,"error":{"type":"invalid_request_error","message":"token expired"}}`, status: 401},
		} {
			err, ok := parseCodexWebsocketStreamError(true, []byte(tc.payload), false)
			if !ok {
				t.Fatal("error frame not parsed")
			}
			want := newCodexStatusErrWithHeaders(tc.status, buildCodexWebsocketErrorPayload([]byte(tc.payload), tc.status), nil, false)
			got, isStatus := err.(statusErr)
			if !isStatus || got.StatusCode() != want.StatusCode() || got.Error() != want.Error() {
				t.Fatalf("bridged error = %T %v, want %d %v", err, err, want.StatusCode(), want.Error())
			}
		}
	})
}

func TestCodexWebsocketBridgeUsesEphemeralSession(t *testing.T) {
	up := newBridgeUpstream(t, func([]byte) []string { return []string{bridgeCompletedFrame} })
	exec := NewCodexAutoExecutor(bridgeTestConfig(100))
	opts := bridgeTestOpts()
	opts.Metadata = map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "session-bridge-1"}
	for i := 0; i < 2; i++ {
		req := cliproxyexecutor.Request{Model: "gpt-6-luna", Payload: []byte(`{"model":"gpt-6-luna","stream":true,"input":"hi"}`)}
		result, errExec := exec.ExecuteStream(context.Background(), bridgeTestOAuth(up.server.URL), req, opts)
		if errExec != nil {
			t.Fatalf("ExecuteStream() error = %v", errExec)
		}
		if _, errStream := collectBridgeChunks(t, result); errStream != nil {
			t.Fatalf("stream error = %v", errStream)
		}
	}
	if got := up.upgrades.Load(); got != 2 {
		t.Fatalf("websocket upgrades = %d, want 2 (no session reuse)", got)
	}
}

type recordingLifecycle struct {
	mu     sync.Mutex
	binds  int
	ends   int
	retain int
}

func (l *recordingLifecycle) Bind(func() error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.binds++
	return nil
}

func (l *recordingLifecycle) End(string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ends++
}

func (l *recordingLifecycle) Retain() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.retain++
}

func TestCodexWebsocketBridgeLeavesLifecycleToConductor(t *testing.T) {
	up := newBridgeUpstream(t, func([]byte) []string { return []string{bridgeCompletedFrame} })
	exec := NewCodexAutoExecutor(bridgeTestConfig(100))
	lifecycle := &recordingLifecycle{}
	opts := bridgeTestOpts()
	opts.ExecutionLifecycle = lifecycle
	req := cliproxyexecutor.Request{Model: "gpt-6-luna", Payload: []byte(`{"model":"gpt-6-luna","stream":true,"input":"hi"}`)}
	result, errExec := exec.ExecuteStream(context.Background(), bridgeTestOAuth(up.server.URL), req, opts)
	if errExec != nil {
		t.Fatalf("ExecuteStream() error = %v", errExec)
	}
	chunks, errStream := collectBridgeChunks(t, result)
	if errStream != nil {
		t.Fatalf("stream error = %v", errStream)
	}
	if len(chunks) == 0 || !strings.Contains(string(chunks[len(chunks)-1]), "response.completed") {
		t.Fatalf("terminal frame not delivered: %q", chunks)
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.binds != 0 || lifecycle.ends != 0 || lifecycle.retain != 0 {
		t.Fatalf("executor touched the lifecycle: binds=%d ends=%d retain=%d", lifecycle.binds, lifecycle.ends, lifecycle.retain)
	}
}

func TestCodexWebsocketBridgeCancelInterruptsBlockedWrite(t *testing.T) {
	release := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		// Never read, so a large request blocks the client write.
		<-release
	}))
	defer server.Close()
	defer close(release)

	writing := make(chan struct{}, 1)
	testWebsocketWritePayloadHook = func(*websocket.Conn) {
		select {
		case writing <- struct{}{}:
		default:
		}
	}
	defer func() { testWebsocketWritePayloadHook = nil }()

	exec := NewCodexAutoExecutor(bridgeTestConfig(100))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	big := strings.Repeat("a", 64<<20)
	req := cliproxyexecutor.Request{Model: "gpt-6-luna", Payload: []byte(`{"model":"gpt-6-luna","stream":true,"input":"` + big + `"}`)}
	done := make(chan error, 1)
	go func() {
		result, errExec := exec.ExecuteStream(ctx, bridgeTestOAuth(server.URL), req, bridgeTestOpts())
		if errExec == nil && result != nil {
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					errExec = chunk.Err
				}
			}
		}
		done <- errExec
	}()
	select {
	case <-writing:
	case <-time.After(10 * time.Second):
		t.Fatal("request write never started")
	}
	cancel()
	select {
	case errExec := <-done:
		if errExec == nil {
			t.Fatal("cancelled bridged request returned no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not interrupt the blocked websocket write")
	}
}

type captureBridgeUsagePlugin struct {
	records chan usage.Record
}

func (p *captureBridgeUsagePlugin) HandleUsage(_ context.Context, record usage.Record) {
	select {
	case p.records <- record:
	default:
	}
}

func collectBridgeUsage(t *testing.T, records <-chan usage.Record, model string, want int) []usage.Record {
	t.Helper()
	var out []usage.Record
	timeout := time.After(3 * time.Second)
	for len(out) < want {
		select {
		case record := <-records:
			if record.Provider == "codex" && (record.Model == model || record.AdditionalModel) && record.AuthID == "auth-bridge-1" {
				out = append(out, record)
			}
		case <-timeout:
			t.Fatalf("collected %d usage records for %s, want %d", len(out), model, want)
		}
	}
	return out
}

func TestCodexWebsocketBridgeUsageMatchesHTTP(t *testing.T) {
	plugin := &captureBridgeUsagePlugin{records: make(chan usage.Record, 64)}
	usage.RegisterPlugin(plugin)
	up := newBridgeUpstream(t, func([]byte) []string { return []string{bridgeCreatedFrame, bridgeCompletedFrame} })

	run := func(cfg *config.Config, model string) ([]byte, []usage.Record) {
		exec := NewCodexAutoExecutor(cfg)
		req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"` + model + `","stream":true,"input":"hi","tools":[{"type":"image_generation"}]}`)}
		result, errExec := exec.ExecuteStream(logging.WithRequestID(context.Background(), "req-usage"), bridgeTestOAuth(up.server.URL), req, bridgeTestOpts())
		if errExec != nil {
			t.Fatalf("ExecuteStream() error = %v", errExec)
		}
		chunks, errStream := collectBridgeChunks(t, result)
		if errStream != nil {
			t.Fatalf("stream error = %v", errStream)
		}
		var completed []byte
		for _, chunk := range chunks {
			for _, line := range bytes.Split(chunk, []byte("\n")) {
				if bytes.HasPrefix(line, []byte("data:")) && gjson.GetBytes(bytes.TrimSpace(line[5:]), "type").String() == "response.completed" {
					completed = bytes.TrimSpace(line[5:])
				}
			}
		}
		if completed == nil {
			t.Fatalf("no response.completed in %q", chunks)
		}
		return completed, collectBridgeUsage(t, plugin.records, model, 2)
	}

	httpCompleted, httpRecords := run(&config.Config{}, "gpt-usage-http")
	if up.sseBodies.Load() != 1 {
		t.Fatalf("http run did not use SSE")
	}
	bridgeCompleted, bridgeRecords := run(bridgeTestConfig(100), "gpt-usage-bridge")
	if up.upgrades.Load() != 1 {
		t.Fatalf("bridge run did not use websocket")
	}
	if got, want := gjson.GetBytes(bridgeCompleted, "response.usage").Raw, gjson.GetBytes(httpCompleted, "response.usage").Raw; got != want {
		t.Fatalf("downstream usage differs:\nbridge %s\nhttp   %s", got, want)
	}
	summarize := func(records []usage.Record) map[bool]usage.Detail {
		out := map[bool]usage.Detail{}
		for _, record := range records {
			out[record.AdditionalModel] = record.Detail
		}
		return out
	}
	httpSummary, bridgeSummary := summarize(httpRecords), summarize(bridgeRecords)
	for _, additional := range []bool{false, true} {
		h, b := httpSummary[additional], bridgeSummary[additional]
		if h.InputTokens != b.InputTokens || h.OutputTokens != b.OutputTokens || h.CachedTokens != b.CachedTokens || h.ReasoningTokens != b.ReasoningTokens || h.TotalTokens != b.TotalTokens {
			t.Fatalf("usage (additional=%v) differs: bridge %+v http %+v", additional, b, h)
		}
	}
	if bridgeSummary[true].TotalTokens != 42 {
		t.Fatalf("image tool usage not published on the bridge: %+v", bridgeRecords)
	}
}

func TestCodexWebsocketBridgeRestoresConfusedIdentity(t *testing.T) {
	var sentKey atomic.Value
	up := newBridgeUpstream(t, func(request []byte) []string {
		key := gjson.GetBytes(request, "prompt_cache_key").String()
		sentKey.Store(key)
		created := `{"type":"response.created","response":{"id":"resp-1","model":"gpt-6-luna","status":"in_progress","prompt_cache_key":"` + key + `"}}`
		return []string{created, bridgeCompletedFrame}
	})
	cfg := bridgeTestConfig(100)
	cfg.Routing.SessionAffinity = true
	cfg.Codex.IdentityConfuse = true
	exec := NewCodexAutoExecutor(cfg)
	req := cliproxyexecutor.Request{Model: "gpt-6-luna", Payload: []byte(`{"model":"gpt-6-luna","stream":true,"input":"hi","prompt_cache_key":"cache-original"}`)}
	result, errExec := exec.ExecuteStream(context.Background(), bridgeTestOAuth(up.server.URL), req, bridgeTestOpts())
	if errExec != nil {
		t.Fatalf("ExecuteStream() error = %v", errExec)
	}
	chunks, errStream := collectBridgeChunks(t, result)
	if errStream != nil {
		t.Fatalf("stream error = %v", errStream)
	}
	if key, _ := sentKey.Load().(string); key == "" || key == "cache-original" {
		t.Fatalf("upstream prompt_cache_key = %q, want a confused value", key)
	}
	for _, chunk := range chunks {
		if !strings.Contains(string(chunk), "response.created") {
			continue
		}
		data := chunk[bytes.Index(chunk, []byte("data:"))+5:]
		if got := gjson.GetBytes(bytes.TrimSpace(data), "response.prompt_cache_key").String(); got != "cache-original" {
			t.Fatalf("downstream prompt_cache_key = %q, want cache-original", got)
		}
		return
	}
	t.Fatalf("no response.created downstream: %q", chunks)
}

func TestCodexWebsocketBridgeFiltersUnderIdentityRewrite(t *testing.T) {
	for _, buffering := range []bool{false, true} {
		up := newBridgeUpstream(t, bridgeFullFrames)
		cfg := bridgeTestConfig(100)
		cfg.Routing.SessionAffinity = true
		cfg.Codex.IdentityConfuse = true
		cfg.Codex.StreamBootstrapBuffering = buffering
		exec := NewCodexAutoExecutor(cfg)
		// A prompt_cache_key equal to the event prefix makes identity handling rewrite the type.
		req := cliproxyexecutor.Request{Model: "gpt-6-luna", Payload: []byte(`{"model":"gpt-6-luna","stream":true,"input":"hi","prompt_cache_key":"codex"}`)}
		result, errExec := exec.ExecuteStream(context.Background(), bridgeTestOAuth(up.server.URL), req, bridgeTestOpts())
		if errExec != nil {
			t.Fatalf("buffering=%v: ExecuteStream() error = %v", buffering, errExec)
		}
		chunks, errStream := collectBridgeChunks(t, result)
		if errStream != nil {
			t.Fatalf("buffering=%v: stream error = %v", buffering, errStream)
		}
		for _, chunk := range chunks {
			text := string(chunk)
			if strings.Contains(text, "rate_limits") || strings.Contains(text, "secret-turn-state") || strings.Contains(text, "websocket_timing") {
				t.Fatalf("buffering=%v: websocket-only event leaked downstream: %s", buffering, text)
			}
		}
	}
}

func TestCodexWebsocketBridgeLogsFramesAsHTTPResponse(t *testing.T) {
	up := newBridgeUpstream(t, bridgeFullFrames)
	cfg := bridgeTestConfig(100)
	cfg.RequestLog = true
	exec := NewCodexAutoExecutor(cfg)
	ctx := contextWithGinHeaders(nil)
	ginCtx := ctx.Value("gin").(*gin.Context)
	req := cliproxyexecutor.Request{Model: "gpt-6-luna", Payload: []byte(`{"model":"gpt-6-luna","stream":true,"input":"hi"}`)}
	result, errExec := exec.ExecuteStream(ctx, bridgeTestOAuth(up.server.URL), req, bridgeTestOpts())
	if errExec != nil {
		t.Fatalf("ExecuteStream() error = %v", errExec)
	}
	if _, errStream := collectBridgeChunks(t, result); errStream != nil {
		t.Fatalf("stream error = %v", errStream)
	}
	if timeline, exists := ginCtx.Get("API_WEBSOCKET_TIMELINE"); exists {
		if data, _ := timeline.([]byte); bytes.Contains(data, []byte("response.output_text.delta")) {
			t.Fatalf("bridged frames went to the websocket timeline: %s", data)
		}
	}
	response, _ := ginCtx.Get("API_RESPONSE")
	if data, _ := response.([]byte); !bytes.Contains(data, []byte("response.output_text.delta")) {
		t.Fatalf("bridged frames missing from the HTTP response log: %q", data)
	}
}

func TestCodexWebsocketBridgeClassifiesErrorUnderIdentityRewrite(t *testing.T) {
	const rateLimited = `{"type":"error","status":429,"error":{"type":"rate_limit_error","message":"slow down"},"headers":{"retry-after":"30"}}`
	for _, buffering := range []bool{false, true} {
		up := newBridgeUpstream(t, func([]byte) []string { return []string{rateLimited} })
		cfg := bridgeTestConfig(100)
		cfg.Routing.SessionAffinity = true
		cfg.Codex.IdentityConfuse = true
		cfg.Codex.StreamBootstrapBuffering = buffering
		exec := NewCodexAutoExecutor(cfg)
		// A prompt_cache_key equal to "error" makes identity handling rewrite the frame type.
		req := cliproxyexecutor.Request{Model: "gpt-6-luna", Payload: []byte(`{"model":"gpt-6-luna","stream":true,"input":"hi","prompt_cache_key":"error"}`)}
		result, errExec := exec.ExecuteStream(context.Background(), bridgeTestOAuth(up.server.URL), req, bridgeTestOpts())
		if errExec == nil {
			chunks, errExec2 := collectBridgeChunks(t, result)
			if len(chunks) != 0 {
				t.Fatalf("buffering=%v: rejection delivered as stream data: %q", buffering, chunks)
			}
			errExec = errExec2
		}
		var se statusErr
		if !errors.As(errExec, &se) || se.StatusCode() != http.StatusTooManyRequests || !se.IsCredentialScoped() {
			t.Fatalf("buffering=%v: error = %T %v, want account-wide 429", buffering, errExec, errExec)
		}
	}
}

func TestCodexWebsocketBridgeTerminalAndQuotaSurviveIdentityRewrite(t *testing.T) {
	for _, buffering := range []bool{false, true} {
		// The upstream keeps the connection open after response.completed, so the stream only
		// ends if the executor recognises the terminal frame.
		up := newBridgeUpstream(t, func([]byte) []string {
			return []string{bridgeRateLimitsFrame, bridgeCreatedFrame, bridgeDeltaFrame, bridgeCompletedFrame}
		})
		cfg := bridgeTestConfig(100)
		cfg.Routing.SessionAffinity = true
		cfg.Codex.IdentityConfuse = true
		cfg.Codex.StreamBootstrapBuffering = buffering
		exec := NewCodexAutoExecutor(cfg)
		ctx := logging.WithResponseHeadersHolder(context.Background())
		// A prompt_cache_key equal to "completed" would rewrite the terminal event type.
		req := cliproxyexecutor.Request{Model: "gpt-6-luna", Payload: []byte(`{"model":"gpt-6-luna","stream":true,"input":"hi","prompt_cache_key":"completed"}`)}
		result, errExec := exec.ExecuteStream(ctx, bridgeTestOAuth(up.server.URL), req, bridgeTestOpts())
		if errExec != nil {
			t.Fatalf("buffering=%v: ExecuteStream() error = %v", buffering, errExec)
		}
		chunks, errStream := collectBridgeChunks(t, result)
		if errStream != nil {
			t.Fatalf("buffering=%v: stream error = %v", buffering, errStream)
		}
		if len(chunks) == 0 || !strings.Contains(string(chunks[len(chunks)-1]), `"type":"response.completed"`) {
			t.Fatalf("buffering=%v: last chunk is not response.completed: %q", buffering, chunks)
		}
		if len(logging.GetResponseHeaders(ctx)) == 0 {
			t.Fatalf("buffering=%v: quota event was not captured", buffering)
		}
	}
}

type codexStreamExecutor interface {
	ExecuteStream(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
}

// The identity replacement must not rename protocol fields on any transport: a client
// prompt_cache_key of "completed" used to rewrite response.completed and hang the stream.
func TestCodexIdentityConfuseKeepsTerminalOnAllTransports(t *testing.T) {
	frames := func([]byte) []string { return []string{bridgeCreatedFrame, bridgeDeltaFrame, bridgeCompletedFrame} }
	cfg := &config.Config{Routing: config.RoutingConfig{SessionAffinity: true}, Codex: config.CodexConfig{IdentityConfuse: true}}
	payload := []byte(`{"model":"gpt-6-luna","stream":true,"input":"hi","prompt_cache_key":"completed"}`)

	run := func(name string, ctx context.Context, exec codexStreamExecutor, opts cliproxyexecutor.Options, baseURL string) {
		t.Helper()
		result, errExec := exec.ExecuteStream(ctx, bridgeTestOAuth(baseURL), cliproxyexecutor.Request{Model: "gpt-6-luna", Payload: payload}, opts)
		if errExec != nil {
			t.Fatalf("%s: ExecuteStream() error = %v", name, errExec)
		}
		chunks, errStream := collectBridgeChunks(t, result)
		if errStream != nil {
			t.Fatalf("%s: stream error = %v", name, errStream)
		}
		if len(chunks) == 0 || !strings.Contains(string(chunks[len(chunks)-1]), "response.completed") {
			t.Fatalf("%s: last chunk is not response.completed: %q", name, chunks)
		}
	}

	httpUp := newBridgeUpstream(t, frames)
	run("http", context.Background(), NewCodexExecutor(cfg), bridgeTestOpts(), httpUp.server.URL)

	nativeUp := newBridgeUpstream(t, frames)
	nativeOpts := bridgeTestOpts()
	run("downstream websocket", cliproxyexecutor.WithDownstreamWebsocket(context.Background()), NewCodexWebsocketsExecutor(cfg), nativeOpts, nativeUp.server.URL)
}

// Error bodies follow the HTTP contract on the bridge: client identity values quoted by the
// upstream are replaced with their confused form before the error is built.
func TestCodexWebsocketBridgeErrorBodyUsesConfusedIdentity(t *testing.T) {
	const rejected = `{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"bad prompt cache-original"}}`
	up := newBridgeUpstream(t, func([]byte) []string { return []string{rejected} })
	cfg := bridgeTestConfig(100)
	cfg.Routing.SessionAffinity = true
	cfg.Codex.IdentityConfuse = true
	exec := NewCodexAutoExecutor(cfg)
	req := cliproxyexecutor.Request{Model: "gpt-6-luna", Payload: []byte(`{"model":"gpt-6-luna","stream":true,"input":"hi","prompt_cache_key":"cache-original"}`)}
	result, errExec := exec.ExecuteStream(context.Background(), bridgeTestOAuth(up.server.URL), req, bridgeTestOpts())
	if errExec == nil {
		_, errExec = collectBridgeChunks(t, result)
	}
	if errExec == nil {
		t.Fatal("rejection not returned as an error")
	}
	confused := codexIdentityConfuseUUID("auth-bridge-1", "prompt-cache", "cache-original")
	if strings.Contains(errExec.Error(), "cache-original") || !strings.Contains(errExec.Error(), confused) {
		t.Fatalf("error body = %q, want the confused identity %q", errExec.Error(), confused)
	}
}

func TestCodexWebsocketBridgeForwardsOnlyHTTPSessionHeader(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{name: "underscore spelling dropped", headers: map[string]string{"session_id": "client-session-42"}},
		{name: "canonical spelling kept", headers: map[string]string{"Session-Id": "client-session-43"}, want: "client-session-43"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := withCodexWebsocketBridge(contextWithGinHeaders(tc.headers))
			ginCtx := ctx.Value("gin").(*gin.Context)
			// contextWithGinHeaders canonicalises keys; keep the raw underscore spelling.
			if raw, ok := tc.headers["session_id"]; ok {
				ginCtx.Request.Header = http.Header{"session_id": []string{raw}}
			}
			headers := applyCodexWebsocketHeaders(ctx, http.Header{}, bridgeTestOAuth(""), "oauth-token", &config.Config{}, false, nil)
			got := codexSessionHeaderValue(headers)
			if tc.want == "" && strings.Contains(got, "client-session") {
				t.Fatalf("raw client session forwarded: %q", got)
			}
			if tc.want != "" && got != tc.want {
				t.Fatalf("session header = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCodexWebsocketBridgeKeepsRetryAfterUnderIdentityRewrite(t *testing.T) {
	const rateLimited = `{"type":"error","status":429,"error":{"type":"rate_limit_error","message":"slow down"},"headers":{"retry-after":"3600"}}`
	for _, buffering := range []bool{false, true} {
		up := newBridgeUpstream(t, func([]byte) []string { return []string{rateLimited} })
		cfg := bridgeTestConfig(100)
		cfg.Routing.SessionAffinity = true
		cfg.Codex.IdentityConfuse = true
		cfg.Codex.StreamBootstrapBuffering = buffering
		exec := NewCodexAutoExecutor(cfg)
		req := cliproxyexecutor.Request{Model: "gpt-6-luna", Payload: []byte(`{"model":"gpt-6-luna","stream":true,"input":"hi","prompt_cache_key":"3600"}`)}
		before := time.Now()
		result, errExec := exec.ExecuteStream(context.Background(), bridgeTestOAuth(up.server.URL), req, bridgeTestOpts())
		if errExec == nil {
			_, errExec = collectBridgeChunks(t, result)
		}
		var se statusErr
		if !errors.As(errExec, &se) {
			t.Fatalf("buffering=%v: error = %T %v, want statusErr", buffering, errExec, errExec)
		}
		quota, ok := se.QuotaDetail()
		if !ok || quota.ResetAt.Before(before.Add(59*time.Minute)) {
			t.Fatalf("buffering=%v: ResetAt = %v (ok=%v), want about one hour", buffering, quota.ResetAt, ok)
		}
	}
}
