package executor

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/webimage"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type fakeCodexWebImageGenerator struct {
	credentials webimage.Credentials
	prompt      string
	results     []webimage.ImageResult
	meta        *webimage.Meta
	err         error
	calls       int
	closed      bool
}

func (f *fakeCodexWebImageGenerator) Close() {
	f.closed = true
}

func (f *fakeCodexWebImageGenerator) Generate(_ context.Context, credentials webimage.Credentials, prompt string) ([]webimage.ImageResult, *webimage.Meta, error) {
	f.calls++
	f.credentials = credentials
	f.prompt = prompt
	return f.results, f.meta, f.err
}

func TestCodexAutoExecutorBridgesWebImageResponse(t *testing.T) {
	generator := &fakeCodexWebImageGenerator{
		results: []webimage.ImageResult{{Base64Data: "aW1hZ2U=", RevisedPrompt: "revised", OutputFormat: "png"}},
		meta:    &webimage.Meta{CreatedAt: 123},
	}
	executor := &CodexAutoExecutor{webImageExec: generator}
	auth := &cliproxyauth.Auth{
		ID:       "auth-id",
		Provider: "codex",
		ProxyURL: "http://proxy.example",
		Metadata: map[string]any{"access_token": "access-token", "account_id": "account-id"},
	}
	req := cliproxyexecutor.Request{Model: "gpt-image-web", Payload: []byte(`{"model":"gpt-image-web","prompt":"draw"}`)}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString(webimage.SourceFormat),
		Metadata:     map[string]any{cliproxyexecutor.RequestPathMetadataKey: codexImagesGenerationsPath},
	}

	response, errExecute := executor.Execute(context.Background(), auth, req, opts)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if generator.calls != 1 || generator.prompt != "draw" {
		t.Fatalf("generator calls = %d prompt = %q", generator.calls, generator.prompt)
	}
	if generator.credentials.AccessToken != "access-token" || generator.credentials.AccountID != "account-id" || generator.credentials.AuthID != "auth-id" || generator.credentials.ProxyURL != "http://proxy.example" {
		t.Fatalf("credentials = %+v", generator.credentials)
	}
	if got := gjson.GetBytes(response.Payload, "data.0.b64_json").String(); got != "aW1hZ2U=" {
		t.Fatalf("b64_json = %q, body=%s", got, response.Payload)
	}
	if got := gjson.GetBytes(response.Payload, "size").String(); got != "1024x1024" {
		t.Fatalf("size = %q, body=%s", got, response.Payload)
	}
	if gjson.GetBytes(response.Payload, "model").Exists() {
		t.Fatalf("internal/public model leaked into response: %s", response.Payload)
	}
}

func TestCodexAutoExecutorRejectsWebImageStreaming(t *testing.T) {
	executor := &CodexAutoExecutor{webImageExec: &fakeCodexWebImageGenerator{}}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString(webimage.SourceFormat),
		Metadata:     map[string]any{cliproxyexecutor.RequestPathMetadataKey: codexImagesGenerationsPath},
	}

	_, errStream := executor.ExecuteStream(context.Background(), &cliproxyauth.Auth{}, cliproxyexecutor.Request{}, opts)
	if errStream == nil {
		t.Fatal("ExecuteStream() error = nil")
	}
	if status, ok := errStream.(interface{ StatusCode() int }); !ok || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("ExecuteStream() error = %#v", errStream)
	}
}

func TestIsCodexWebImageRequestIsNarrow(t *testing.T) {
	base := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString(webimage.SourceFormat),
		Metadata:     map[string]any{cliproxyexecutor.RequestPathMetadataKey: codexImagesGenerationsPath},
	}
	if !isCodexWebImageRequest(base) {
		t.Fatal("expected web image generations request to match")
	}
	base.SourceFormat = sdktranslator.FromString(codexOpenAIImageSourceFormat)
	if isCodexWebImageRequest(base) {
		t.Fatal("existing OpenAI image request matched web image bridge")
	}
	base.SourceFormat = sdktranslator.FromString(webimage.SourceFormat)
	base.Metadata[cliproxyexecutor.RequestPathMetadataKey] = codexImagesEditsPath
	if isCodexWebImageRequest(base) {
		t.Fatal("image edits request matched web image bridge")
	}
}

func TestCodexAutoExecutorClosesWebImageSessionsWhenReplaced(t *testing.T) {
	generator := &fakeCodexWebImageGenerator{}
	executor := &CodexAutoExecutor{webImageExec: generator}

	executor.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)

	if !generator.closed {
		t.Fatal("web image executor was not closed")
	}
}
