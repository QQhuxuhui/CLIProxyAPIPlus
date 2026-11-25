// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements a Codex executor that uses the Responses API WebSocket transport.
package executor

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/client/grokbuild"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/webimage"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// CodexWebsocketsExecutor executes Codex Responses requests using a WebSocket transport.
//
// It preserves the existing CodexExecutor HTTP implementation as a fallback for endpoints
// not available over WebSocket (e.g. /responses/compact) and for websocket upgrade failures.
type CodexWebsocketsExecutor struct {
	*CodexExecutor

	store *codexWebsocketSessionStore
}

func NewCodexWebsocketsExecutor(cfg *config.Config) *CodexWebsocketsExecutor {
	return &CodexWebsocketsExecutor{
		CodexExecutor: NewCodexExecutor(cfg),
		store:         globalCodexWebsocketSessionStore,
	}
}

// CodexAutoExecutor routes Codex requests to the websocket transport when:
//  1. The downstream transport is websocket and the selected auth enables websockets, or
//  2. codex.http-websocket-bridge selects an HTTP-downstream streaming /responses request
//     (see shouldBridgeCodexStreamToWebsocket).
//
// Every other request uses the HTTP implementation.
type CodexAutoExecutor struct {
	httpExec     *CodexExecutor
	wsExec       *CodexWebsocketsExecutor
	webImageExec codexWebImageGenerator
}

func NewCodexAutoExecutor(cfg *config.Config) *CodexAutoExecutor {
	return &CodexAutoExecutor{
		httpExec:     NewCodexExecutor(cfg),
		wsExec:       NewCodexWebsocketsExecutor(cfg),
		webImageExec: webimage.NewExecutor(cfg),
	}
}

func (e *CodexAutoExecutor) Identifier() string { return "codex" }

func (e *CodexAutoExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if e == nil || e.httpExec == nil {
		return nil
	}
	return e.httpExec.PrepareRequest(req, auth)
}

func (e *CodexAutoExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.HttpRequest(ctx, auth, req)
}

func (e *CodexAutoExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if isCodexWebImageRequest(opts) {
		return e.executeWebImage(ctx, auth, req, opts)
	}
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: executor is nil")
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) && codexWebsocketsEnabled(auth) {
		return e.wsExec.Execute(ctx, auth, req, opts)
	}
	if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		return cliproxyexecutor.Response{}, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
	}
	return e.httpExec.Execute(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if isCodexWebImageRequest(opts) {
		return nil, badRequestErr(fmt.Errorf("web image streaming is not supported"))
	}
	if e == nil || e.httpExec == nil || e.wsExec == nil {
		return nil, fmt.Errorf("codex auto executor: executor is nil")
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) && codexWebsocketsEnabled(auth) {
		return e.wsExec.ExecuteStream(ctx, auth, req, opts)
	}
	if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		return nil, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
	}
	if shouldBridgeCodexStreamToWebsocket(ctx, e.httpExec.cfg, auth, opts) {
		return e.wsExec.ExecuteStream(withCodexWebsocketBridge(ctx), auth, req, opts)
	}
	return e.httpExec.ExecuteStream(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if e == nil || e.httpExec == nil {
		return nil, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.Refresh(ctx, auth)
}

func (e *CodexAutoExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.httpExec == nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex auto executor: http executor is nil")
	}
	return e.httpExec.CountTokens(ctx, auth, req, opts)
}

func (e *CodexAutoExecutor) CloseExecutionSession(sessionID string) {
	if e == nil {
		return
	}
	if sessionID == cliproxyauth.CloseAllExecutionSessionsID {
		if closer, ok := e.webImageExec.(interface{ Close() }); ok {
			closer.Close()
		}
	}
	if e.wsExec != nil {
		e.wsExec.CloseExecutionSession(sessionID)
	}
}

func (e *CodexAutoExecutor) UpstreamDisconnectChan(sessionID string) <-chan error {
	if e == nil || e.wsExec == nil {
		return nil
	}
	return e.wsExec.UpstreamDisconnectChan(sessionID)
}

func codexWebsocketsEnabled(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false
	}
	raw, ok := auth.Metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}

type codexWebsocketBridgeContextKey struct{}

// withCodexWebsocketBridge marks a context whose HTTP-downstream request is served over the
// upstream websocket transport by the HTTP websocket bridge.
func withCodexWebsocketBridge(ctx context.Context) context.Context {
	return context.WithValue(ctx, codexWebsocketBridgeContextKey{}, true)
}

func isCodexWebsocketBridge(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	bridged, _ := ctx.Value(codexWebsocketBridgeContextKey{}).(bool)
	return bridged
}

// shouldBridgeCodexStreamToWebsocket reports whether an HTTP-downstream streaming request should
// use the upstream websocket transport, which has no 15-minute stream cap. Only plain
// /responses traffic on Codex OAuth credentials is bridged; compact, image, Grok client and
// API key requests keep the HTTP transport.
func shouldBridgeCodexStreamToWebsocket(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, opts cliproxyexecutor.Options) bool {
	if cfg == nil || !cfg.Codex.HTTPWebsocketBridge.Enable || cfg.Codex.HTTPWebsocketBridge.Percent <= 0 {
		return false
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) {
		return false
	}
	if opts.SourceFormat != sdktranslator.FormatOpenAIResponse || cliproxyexecutor.ResponseFormatOrSource(opts) != sdktranslator.FormatOpenAIResponse {
		return false
	}
	if opts.Alt != "" {
		return false
	}
	if isCodexOpenAIImageRequest(opts) || codexIsImagesEndpointPath(helps.PayloadRequestPath(opts)) {
		return false
	}
	if grokbuild.IsGrokClientContext(ctx, opts.Headers) {
		return false
	}
	if auth == nil || codexAuthUsesAPIKey(auth) {
		return false
	}
	if token, _ := codexCreds(auth); strings.TrimSpace(token) == "" {
		return false
	}
	return helps.CodexWebsocketBridgeBucketHit(logging.GetRequestID(ctx), cfg.Codex.HTTPWebsocketBridge.Percent)
}

// prepareCodexWebsocketBridgeBody applies the request normalization of the HTTP/SSE path to a
// bridged websocket request, in place of the websocket-only parallel_tool_calls handling.
func prepareCodexWebsocketBridgeBody(body []byte, auth *cliproxyauth.Auth, headers http.Header) []byte {
	body = applyCodexAuthServiceTier(body, auth)
	body = helps.CleanCodexBridgeResponsesBody(body)
	return normalizeCodexParallelToolCalls(body, headers)
}

// parseCodexWebsocketStreamError parses an upstream websocket error frame. On the bridge the
// result follows the HTTP error contract the HTTP downstream already relies on (status and body
// normalization, account-wide 429 cooldown from Retry-After) and carries no upstream headers,
// so nothing reaches the client through passthrough headers. The websocket connection limit
// has no HTTP counterpart and keeps its websocket classification.
func parseCodexWebsocketStreamError(bridge bool, payload []byte, modelLevelCooling bool) (error, bool) {
	wsErr, ok := parseCodexWebsocketErrorWithCooling(payload, modelLevelCooling)
	if !ok || !bridge {
		return wsErr, ok
	}
	if isCodexWebsocketConnectionLimitError(payload) {
		if withHeaders, isWithHeaders := wsErr.(statusErrWithHeaders); isWithHeaders {
			return withHeaders.statusErr, true
		}
		return wsErr, true
	}
	status := int(gjson.GetBytes(payload, "status").Int())
	if status == 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	return newCodexStatusErrWithHeaders(status, buildCodexWebsocketErrorPayload(payload, status), parseCodexWebsocketErrorHeaders(payload), modelLevelCooling), true
}
