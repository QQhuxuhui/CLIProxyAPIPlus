package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/webimage"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type fakeCodexWebImageGenerator struct {
	credentials webimage.Credentials
	request     webimage.Request
	results     []webimage.ImageResult
	meta        *webimage.Meta
	err         error
	calls       int
	closed      bool
}

func (f *fakeCodexWebImageGenerator) Close() {
	f.closed = true
}

func (f *fakeCodexWebImageGenerator) GenerateRequest(_ context.Context, credentials webimage.Credentials, request webimage.Request) ([]webimage.ImageResult, *webimage.Meta, error) {
	f.calls++
	f.credentials = credentials
	f.request = request
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
	if generator.calls != 1 || generator.request.Prompt != "draw" {
		t.Fatalf("generator calls = %d prompt = %q", generator.calls, generator.request.Prompt)
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

func TestCodexAutoExecutorConvertsSizeAndQualityToPrompt(t *testing.T) {
	generator := &fakeCodexWebImageGenerator{results: []webimage.ImageResult{{Base64Data: "AA==", OutputFormat: "png"}}}
	executor := &CodexAutoExecutor{webImageExec: generator}
	req := cliproxyexecutor.Request{Payload: []byte(`{"prompt":"draw a city","size":"1536x1024","quality":"maximum-detail"}`)}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString(webimage.SourceFormat),
		Metadata:     map[string]any{cliproxyexecutor.RequestPathMetadataKey: codexImagesGenerationsPath},
	}

	response, errExecute := executor.Execute(context.Background(), &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "token"}}, req, opts)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	for _, want := range []string{"1536x1024", "maximum-detail", "landscape"} {
		if !strings.Contains(generator.request.Prompt, want) {
			t.Fatalf("prompt = %q, want containing %q", generator.request.Prompt, want)
		}
	}
	if got := gjson.GetBytes(response.Payload, "size").String(); got != "1536x1024" {
		t.Fatalf("size = %q, body=%s", got, response.Payload)
	}
}

func TestCodexAutoExecutorForwardsArbitrarySizeWithoutInventingOrientation(t *testing.T) {
	generator := &fakeCodexWebImageGenerator{results: []webimage.ImageResult{{Base64Data: "AA==", OutputFormat: "png"}}}
	executor := &CodexAutoExecutor{webImageExec: generator}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString(webimage.SourceFormat),
		Metadata:     map[string]any{cliproxyexecutor.RequestPathMetadataKey: codexImagesGenerationsPath},
	}

	_, errExecute := executor.Execute(context.Background(), &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "token"}}, cliproxyexecutor.Request{Payload: []byte(`{"prompt":"draw","size":"cinema-wide-custom"}`)}, opts)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if !strings.Contains(generator.request.Prompt, "cinema-wide-custom") {
		t.Fatalf("prompt = %q", generator.request.Prompt)
	}
	for _, unexpected := range []string{"landscape", "portrait", "square"} {
		if strings.Contains(generator.request.Prompt, unexpected) {
			t.Fatalf("prompt = %q, unexpected %q", generator.request.Prompt, unexpected)
		}
	}
}

func TestCodexAutoExecutorBuildsReferenceImageEditRequest(t *testing.T) {
	imageData := encodeTestPNG(t, 2, 3)
	payload := []byte(`{"prompt":"make it watercolor","quality":"high","images":[{"filename":"reference.png","image_url":"data:image/png;base64,` + imageData + `"}]}`)
	generator := &fakeCodexWebImageGenerator{results: []webimage.ImageResult{{Base64Data: "AA==", OutputFormat: "png"}}}
	executor := &CodexAutoExecutor{webImageExec: generator}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString(webimage.SourceFormat),
		Metadata:     map[string]any{cliproxyexecutor.RequestPathMetadataKey: codexImagesEditsPath},
	}

	response, errExecute := executor.Execute(context.Background(), &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "token"}}, cliproxyexecutor.Request{Payload: payload}, opts)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if len(generator.request.Images) != 1 {
		t.Fatalf("images = %#v", generator.request.Images)
	}
	input := generator.request.Images[0]
	if input.Filename != "reference.png" || input.MIMEType != "image/png" || input.Width != 2 || input.Height != 3 {
		t.Fatalf("input = %+v", input)
	}
	for _, want := range []string{"high", "Preserve", "dimensions", "aspect ratio"} {
		if !strings.Contains(generator.request.Prompt, want) {
			t.Fatalf("prompt = %q, want containing %q", generator.request.Prompt, want)
		}
	}
	if got := gjson.GetBytes(response.Payload, "size").String(); got != "2x3" {
		t.Fatalf("size = %q, body=%s", got, response.Payload)
	}
}

func TestParseCodexWebImageInputsEnforcesCountAndAggregateByteLimits(t *testing.T) {
	encoded := encodeTestPNG(t, 2, 3)
	decoded, errDecode := base64.StdEncoding.DecodeString(encoded)
	if errDecode != nil {
		t.Fatalf("DecodeString() error = %v", errDecode)
	}
	payload := []byte(`{"images":[{"image_url":"data:image/png;base64,` + encoded + `"},{"image_url":"data:image/png;base64,` + encoded + `"}]}`)

	if _, errCount := parseCodexWebImageInputsWithLimits(payload, true, 1, int64(len(decoded)*2)); errCount == nil || !strings.Contains(errCount.Error(), "too many") {
		t.Fatalf("count error = %v", errCount)
	}
	if _, errBytes := parseCodexWebImageInputsWithLimits(payload, true, 2, int64(len(decoded)*2-1)); errBytes == nil || !strings.Contains(errBytes.Error(), "byte limit") {
		t.Fatalf("byte limit error = %v", errBytes)
	}
}

func encodeTestPNG(t *testing.T, width, height int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buffer bytes.Buffer
	if errEncode := png.Encode(&buffer, img); errEncode != nil {
		t.Fatalf("png.Encode() error = %v", errEncode)
	}
	return base64.StdEncoding.EncodeToString(buffer.Bytes())
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
	if !isCodexWebImageRequest(base) {
		t.Fatal("expected web image edits request to match")
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
