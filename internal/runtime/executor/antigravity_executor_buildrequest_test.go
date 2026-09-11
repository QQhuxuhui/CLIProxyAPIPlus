package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestAntigravityBuildRequest_SanitizesGeminiToolSchema(t *testing.T) {
	body := buildRequestBodyFromPayload(t, "gemini-2.5-pro")

	decl := extractFirstFunctionDeclaration(t, body)
	if _, ok := decl["parametersJsonSchema"]; ok {
		t.Fatalf("parametersJsonSchema should be renamed to parameters")
	}

	params, ok := decl["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters missing or invalid type")
	}
	assertSchemaSanitizedAndPropertyPreserved(t, params)
}

func TestAntigravityBuildRequest_SanitizesAntigravityToolSchema(t *testing.T) {
	body := buildRequestBodyFromPayload(t, "claude-opus-4-6")

	decl := extractFirstFunctionDeclaration(t, body)
	params, ok := decl["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters missing or invalid type")
	}
	assertSchemaSanitizedAndPropertyPreserved(t, params)
}

func TestAntigravityBuildRequest_SkipsSchemaSanitizationWithoutToolsField(t *testing.T) {
	body := buildRequestBodyFromRawPayload(t, "gemini-3.1-flash-image", []byte(`{
		"request": {
			"contents": [
				{
					"role": "user",
					"x-debug": "keep-me",
					"parts": [
						{
							"text": "hello"
						}
					]
				}
			],
			"nonSchema": {
				"nullable": true,
				"x-extra": "keep-me"
			},
			"generationConfig": {
				"maxOutputTokens": 128
			}
		}
	}`))

	assertNonSchemaRequestPreserved(t, body)
}

func TestAntigravityBuildRequest_SkipsSchemaSanitizationWithEmptyToolsArray(t *testing.T) {
	body := buildRequestBodyFromRawPayload(t, "gemini-3.1-flash-image", []byte(`{
		"request": {
			"tools": [],
			"contents": [
				{
					"role": "user",
					"x-debug": "keep-me",
					"parts": [
						{
							"text": "hello"
						}
					]
				}
			],
			"nonSchema": {
				"nullable": true,
				"x-extra": "keep-me"
			},
			"generationConfig": {
				"maxOutputTokens": 128
			}
		}
	}`))

	assertNonSchemaRequestPreserved(t, body)
}

func TestAntigravityBuildRequest_UsesAuthProjectID(t *testing.T) {
	body := buildRequestBodyFromRawPayload(t, "gemini-3.1-pro", []byte(`{
		"request": {
			"contents": [
				{
					"role": "user",
					"parts": [{"text": "hello"}]
				}
			]
		}
	}`))

	if got, ok := body["project"].(string); !ok || got != "project-1" {
		t.Fatalf("project should come from auth metadata, got=%v", body["project"])
	}
}

func TestAntigravityBuildRequest_UsesRouteModelWhenPayloadContainsDifferentModel(t *testing.T) {
	body := buildRequestBodyFromRawPayload(t, "gemini-3-flash-agent", []byte(`{
		"model": "gemini-3.1-flash-lite",
		"request": {
			"contents": [
				{
					"role": "user",
					"parts": [{"text": "Perform a web search"}]
				}
			],
			"tools": [{"googleSearch": {}}]
		}
	}`))

	if got, ok := body["model"].(string); !ok || got != "gemini-3-flash-agent" {
		t.Fatalf("request model should stay on route model, got=%v", body["model"])
	}
}

func TestAntigravitySessionResolverUsesStableIdentityForSameConversation(t *testing.T) {
	t.Parallel()

	req := cliproxyexecutor.Request{
		Model:   "gemini-3-flash-agent",
		Payload: []byte(`{"session_id":"stable-session","request":{"contents":[{"role":"user","parts":[{"text":"first"}]}]}}`),
	}
	opts := cliproxyexecutor.Options{OriginalRequest: req.Payload}
	stableKey, firstID, stable := antigravitySessionForRequest(context.Background(), req, opts)
	if !stable || stableKey == "" || firstID == "" {
		t.Fatalf("stable session = (%q, %q, %v), want stable identity", stableKey, firstID, stable)
	}

	req.Payload = []byte(`{"session_id":"stable-session","request":{"contents":[{"role":"user","parts":[{"text":"second"}]}]}}`)
	opts.OriginalRequest = req.Payload
	secondKey, secondID, secondStable := antigravitySessionForRequest(context.Background(), req, opts)
	if !secondStable || secondKey != stableKey || secondID != firstID {
		t.Fatalf("same stable session changed: first=(%q,%q), second=(%q,%q)", stableKey, firstID, secondKey, secondID)
	}
}

func TestAntigravitySessionResolverDoesNotUseMessageHashForEphemeralIdentity(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"same"}]}]}}`)
	req := cliproxyexecutor.Request{Model: "gemini-3-flash-agent", Payload: payload}
	opts := cliproxyexecutor.Options{OriginalRequest: payload}
	firstKey, firstID, firstStable := antigravitySessionForRequest(context.Background(), req, opts)
	secondKey, secondID, secondStable := antigravitySessionForRequest(context.Background(), req, opts)
	if firstStable || secondStable || firstKey != "" || secondKey != "" {
		t.Fatalf("message-only request became stable: first=(%q,%q,%v), second=(%q,%q,%v)", firstKey, firstID, firstStable, secondKey, secondID, secondStable)
	}
	if firstID == "" || secondID == "" || firstID == secondID {
		t.Fatalf("ephemeral upstream IDs should be distinct: %q and %q", firstID, secondID)
	}
}

func TestAntigravitySessionResolverSeparatesExplicitSessionsWithSamePrompt(t *testing.T) {
	t.Parallel()

	firstPayload := []byte(`{"session_id":"session-a","request":{"contents":[{"role":"user","parts":[{"text":"same"}]}]}}`)
	secondPayload := []byte(`{"session_id":"session-b","request":{"contents":[{"role":"user","parts":[{"text":"same"}]}]}}`)
	firstReq := cliproxyexecutor.Request{Model: "gemini-3-flash-agent", Payload: firstPayload}
	secondReq := cliproxyexecutor.Request{Model: "gemini-3-flash-agent", Payload: secondPayload}
	_, firstID, firstStable := antigravitySessionForRequest(context.Background(), firstReq, cliproxyexecutor.Options{OriginalRequest: firstPayload})
	_, secondID, secondStable := antigravitySessionForRequest(context.Background(), secondReq, cliproxyexecutor.Options{OriginalRequest: secondPayload})
	if !firstStable || !secondStable || firstID == secondID {
		t.Fatalf("explicit sessions did not separate upstream IDs: first=(%q,%v) second=(%q,%v)", firstID, firstStable, secondID, secondStable)
	}
}

func TestAntigravityBuildRequestUsesResolvedSessionID(t *testing.T) {
	t.Parallel()

	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-1"}}
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)
	req, err := executor.buildRequest(context.Background(), auth, "token", "gemini-3-flash-agent", payload, false, "", "https://example.com", "-123456789")
	if err != nil {
		t.Fatal(err)
	}
	if got := requestBody(t, req)["request"].(map[string]any)["sessionId"]; got != "-123456789" {
		t.Fatalf("request.sessionId = %v, want -123456789", got)
	}
}

func TestAntigravityBuildRequestReplacesRawClientSessionID(t *testing.T) {
	t.Parallel()

	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-1"}}
	payload := []byte(`{"request":{"sessionId":"raw-client-session","contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)
	req, err := executor.buildRequest(context.Background(), auth, "token", "gemini-3-flash-agent", payload, false, "", "https://example.com", "-987654321")
	if err != nil {
		t.Fatal(err)
	}
	if got := requestBody(t, req)["request"].(map[string]any)["sessionId"]; got != "-987654321" {
		t.Fatalf("request.sessionId = %v, want hashed resolver ID -987654321", got)
	}
}

func TestAntigravitySessionResolverRejectsRequestAndUserIdentifiers(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		headers http.Header
		payload []byte
	}{
		{name: "client request id", headers: http.Header{"X-Client-Request-Id": []string{"request-1"}}, payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)},
		{name: "generic user id", payload: []byte(`{"metadata":{"user_id":"user-1"},"messages":[{"role":"user","content":"hi"}]}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := cliproxyexecutor.Request{Model: "gemini-3-flash-agent", Payload: tc.payload}
			opts := cliproxyexecutor.Options{Headers: tc.headers, OriginalRequest: tc.payload}
			stableKey, _, stable := antigravitySessionForRequest(context.Background(), req, opts)
			if stable || stableKey != "" {
				t.Fatalf("request-only identity became stable: %q", stableKey)
			}
		})
	}
}

func TestAntigravitySessionResolverScopesSameIDByAuthenticatedCaller(t *testing.T) {
	payload := []byte(`{"session_id":"shared-client-value","messages":[{"role":"user","content":"hi"}]}`)
	req := cliproxyexecutor.Request{Model: "gemini-3-flash-agent", Payload: payload}
	opts := cliproxyexecutor.Options{OriginalRequest: payload}

	ctxA := antigravityCallerContext("caller-a")
	ctxB := antigravityCallerContext("caller-b")
	keyA, upstreamA, stableA := antigravitySessionForRequest(ctxA, req, opts)
	keyB, upstreamB, stableB := antigravitySessionForRequest(ctxB, req, opts)
	if !stableA || !stableB {
		t.Fatal("explicit sessions should be stable")
	}
	if keyA == keyB || upstreamA == upstreamB {
		t.Fatalf("callers shared session scope: A=(%q,%q) B=(%q,%q)", keyA, upstreamA, keyB, upstreamB)
	}
}

func antigravityCallerContext(caller string) context.Context {
	ginContext := &gin.Context{}
	ginContext.Set("userApiKey", caller)
	return context.WithValue(context.Background(), "gin", ginContext)
}

func TestAntigravityBuildRequest_PreservesIndependentWebSearchRequestType(t *testing.T) {
	body := buildRequestBodyFromRawPayload(t, "gemini-3.1-flash-lite", []byte(`{
		"requestType": "web_search",
		"request": {
			"contents": [
				{
					"role": "user",
					"parts": [{"text": "北京天气 2026-06-12"}]
				}
			],
			"tools": [
				{
					"googleSearch": {
						"enhancedContent": {
							"imageSearch": {
								"maxResultCount": 5
							}
						}
					}
				}
			],
			"generationConfig": {
				"candidateCount": 1
			}
		}
	}`))

	if got, ok := body["requestType"].(string); !ok || got != "web_search" {
		t.Fatalf("requestType should stay web_search, got=%v", body["requestType"])
	}
	if _, ok := body["requestId"]; ok {
		t.Fatalf("web_search request should not add requestId: %v", body["requestId"])
	}
	request, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("request missing or invalid: %v", body["request"])
	}
	if _, ok := request["sessionId"]; ok {
		t.Fatalf("web_search request should not add request.sessionId: %v", request["sessionId"])
	}
	if got, ok := body["project"].(string); !ok || got != "project-1" {
		t.Fatalf("project should come from auth metadata, got=%v", body["project"])
	}
}

func TestShouldResolveAntigravityWebSearchGroundingURLsRequiresTypedWebSearchAndSearchRequest(t *testing.T) {
	original := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	translatedWithGoogleSearch := []byte(`{"requestType":"web_search","request":{"tools":[{"googleSearch":{}}]}}`)
	translatedWithoutGoogleSearch := []byte(`{"request":{"contents":[]}}`)

	if !shouldResolveAntigravityWebSearchGroundingURLs(sdktranslator.FormatClaude, original, translatedWithGoogleSearch) {
		t.Fatal("expected typed Claude web search translated to web_search request to resolve grounding URLs")
	}
	if shouldResolveAntigravityWebSearchGroundingURLs(sdktranslator.FormatClaude, original, translatedWithoutGoogleSearch) {
		t.Fatal("expected request without googleSearch to skip grounding URL resolution")
	}
	if shouldResolveAntigravityWebSearchGroundingURLs(sdktranslator.FormatOpenAI, original, translatedWithGoogleSearch) {
		t.Fatal("expected non-Claude source format to skip grounding URL resolution")
	}
}

func TestAntigravityPrepareRequestAuth_FetchesMissingProjectID(t *testing.T) {
	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{
		"access_token": "token",
		"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
	}}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist" {
			t.Fatalf("unexpected project discovery request: %s", req.URL.String())
		}
		if got := req.Header.Get("X-Goog-Api-Client"); got != "" {
			t.Fatalf("X-Goog-Api-Client = %q, want empty", got)
		}
		raw, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatalf("read discovery body: %v", errRead)
		}
		if !strings.Contains(string(raw), `"ideType":"ANTIGRAVITY"`) {
			t.Fatalf("unexpected discovery body: %s", string(raw))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"cloudaicompanionProject":"fetched-project"}`)),
		}, nil
	}))

	updated, err := executor.PrepareRequestAuth(ctx, auth)
	if err != nil {
		t.Fatalf("PrepareRequestAuth error: %v", err)
	}
	if updated == nil {
		t.Fatalf("PrepareRequestAuth returned nil auth")
	}
	if _, ok := auth.Metadata["project_id"]; ok {
		t.Fatalf("original auth metadata should not be mutated")
	}
	if got, ok := updated.Metadata["project_id"].(string); !ok || got != "fetched-project" {
		t.Fatalf("updated auth metadata project_id = %v, want fetched-project", updated.Metadata["project_id"])
	}
}

func TestAntigravityBuildRequest_RejectsMissingProjectID(t *testing.T) {
	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{}}

	_, err := executor.buildRequest(context.Background(), auth, "token", "gemini-3.1-pro", []byte(`{"request":{}}`), false, "", "https://example.com")
	if err == nil {
		t.Fatalf("buildRequest should fail when auth has no project_id")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error should expose status code, got %T", err)
	}
	if got := status.StatusCode(); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d", got, http.StatusBadRequest)
	}
}

func assertNonSchemaRequestPreserved(t *testing.T, body map[string]any) {
	t.Helper()

	request, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("request missing or invalid type")
	}

	contents, ok := request["contents"].([]any)
	if !ok || len(contents) == 0 {
		t.Fatalf("contents missing or empty")
	}
	content, ok := contents[0].(map[string]any)
	if !ok {
		t.Fatalf("content missing or invalid type")
	}
	if got, ok := content["x-debug"].(string); !ok || got != "keep-me" {
		t.Fatalf("x-debug should be preserved when no tool schema exists, got=%v", content["x-debug"])
	}

	nonSchema, ok := request["nonSchema"].(map[string]any)
	if !ok {
		t.Fatalf("nonSchema missing or invalid type")
	}
	if _, ok := nonSchema["nullable"]; !ok {
		t.Fatalf("nullable should be preserved outside schema cleanup path")
	}
	if got, ok := nonSchema["x-extra"].(string); !ok || got != "keep-me" {
		t.Fatalf("x-extra should be preserved outside schema cleanup path, got=%v", nonSchema["x-extra"])
	}

	if generationConfig, ok := request["generationConfig"].(map[string]any); ok {
		if _, ok := generationConfig["maxOutputTokens"]; ok {
			t.Fatalf("maxOutputTokens should still be removed for non-Claude requests")
		}
	}
}

func buildRequestBodyFromPayload(t *testing.T, modelName string) map[string]any {
	t.Helper()
	return buildRequestBodyFromRawPayload(t, modelName, []byte(`{
		"request": {
			"tools": [
				{
					"function_declarations": [
						{
							"name": "tool_1",
							"parametersJsonSchema": {
								"$schema": "http://json-schema.org/draft-07/schema#",
								"$id": "root-schema",
								"$comment": "root comment should be removed",
								"type": "object",
								"properties": {
									"$id": {"type": "string"},
									"arg": {
										"type": "object",
										"$comment": "nested comment should be removed",
										"prefill": "hello",
										"properties": {
											"mode": {
												"type": "string",
												"deprecated": true,
												"enum": ["a", "b"],
												"enumDescriptions": ["Alpha", "Beta"],
												"enumTitles": ["A", "B"]
											}
										}
									}
								},
								"patternProperties": {
									"^x-": {"type": "string"}
								}
							}
						}
					]
				}
			]
		}
	}`))
}

func buildRequestBodyFromRawPayload(t *testing.T, modelName string, payload []byte) map[string]any {
	t.Helper()

	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-1"}}

	req, err := executor.buildRequest(context.Background(), auth, "token", modelName, payload, false, "", "https://example.com")
	if err != nil {
		t.Fatalf("buildRequest error: %v", err)
	}

	return requestBody(t, req)
}

func requestBody(t *testing.T, req *http.Request) map[string]any {
	t.Helper()

	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read request body error: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal request body error: %v, body=%s", err, string(raw))
	}
	return body
}

func extractFirstFunctionDeclaration(t *testing.T, body map[string]any) map[string]any {
	t.Helper()

	request, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("request missing or invalid type")
	}
	tools, ok := request["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools missing or empty")
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("first tool invalid type")
	}
	decls, ok := tool["function_declarations"].([]any)
	if !ok || len(decls) == 0 {
		t.Fatalf("function_declarations missing or empty")
	}
	decl, ok := decls[0].(map[string]any)
	if !ok {
		t.Fatalf("first function declaration invalid type")
	}
	return decl
}

func assertSchemaSanitizedAndPropertyPreserved(t *testing.T, params map[string]any) {
	t.Helper()

	if _, ok := params["$id"]; ok {
		t.Fatalf("root $id should be removed from schema")
	}
	if _, ok := params["$comment"]; ok {
		t.Fatalf("root $comment should be removed from schema")
	}
	if _, ok := params["patternProperties"]; ok {
		t.Fatalf("patternProperties should be removed from schema")
	}

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or invalid type")
	}
	if _, ok := props["$id"]; !ok {
		t.Fatalf("property named $id should be preserved")
	}

	arg, ok := props["arg"].(map[string]any)
	if !ok {
		t.Fatalf("arg property missing or invalid type")
	}
	if _, ok := arg["prefill"]; ok {
		t.Fatalf("prefill should be removed from nested schema")
	}
	if _, ok := arg["$comment"]; ok {
		t.Fatalf("nested $comment should be removed from schema")
	}

	argProps, ok := arg["properties"].(map[string]any)
	if !ok {
		t.Fatalf("arg.properties missing or invalid type")
	}
	mode, ok := argProps["mode"].(map[string]any)
	if !ok {
		t.Fatalf("mode property missing or invalid type")
	}
	if _, ok := mode["enumTitles"]; ok {
		t.Fatalf("enumTitles should be removed from nested schema")
	}
	if _, ok := mode["enumDescriptions"]; ok {
		t.Fatalf("enumDescriptions should be removed from nested schema")
	}
	if _, ok := mode["deprecated"]; ok {
		t.Fatalf("deprecated should be removed from nested schema")
	}
}
