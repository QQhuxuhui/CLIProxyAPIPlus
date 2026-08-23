package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/webimage"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type fakeCodexWebImageGenerator struct {
	mu          sync.Mutex
	credentials webimage.Credentials
	request     webimage.Request
	results     []webimage.ImageResult
	meta        *webimage.Meta
	err         error
	calls       int
	failOnCall  int
	// blockUntilCanceled makes non-failing calls wait for ctx cancellation and
	// return ctx.Err(), exercising root-cause-over-Canceled error selection.
	blockUntilCanceled bool
	closed             bool
}

func (f *fakeCodexWebImageGenerator) Close() {
	f.closed = true
}

func (f *fakeCodexWebImageGenerator) GenerateRequest(ctx context.Context, credentials webimage.Credentials, request webimage.Request) ([]webimage.ImageResult, *webimage.Meta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.credentials = credentials
	f.request = request
	if f.failOnCall > 0 && f.calls == f.failOnCall {
		return nil, nil, errors.New("upstream exploded")
	}
	if f.blockUntilCanceled {
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		return nil, nil, ctx.Err()
	}
	return f.results, f.meta, f.err
}

func TestCodexAutoExecutorWebImageFansOutForN(t *testing.T) {
	generator := &fakeCodexWebImageGenerator{
		results: []webimage.ImageResult{{Base64Data: "aW1hZ2U=", OutputFormat: "png"}},
		meta:    &webimage.Meta{CreatedAt: 123},
	}
	executor := &CodexAutoExecutor{webImageExec: generator}
	resp, err := executor.executeWebImage(context.Background(), &cliproxyauth.Auth{ID: "auth-1"},
		cliproxyexecutor.Request{Payload: []byte(`{"model":"gpt-image-web","prompt":"two cats","n":3}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(webimage.SourceFormat), Metadata: map[string]any{"request_path": "/v1/images/generations"}})
	if err != nil {
		t.Fatalf("executeWebImage: %v", err)
	}
	if generator.calls != 3 {
		t.Fatalf("n=3 should run the web generation 3 times, got %d", generator.calls)
	}
	if got := len(gjson.GetBytes(resp.Payload, "data").Array()); got != 3 {
		t.Fatalf("data should carry 3 images, got %d", got)
	}
}

func TestCodexAutoExecutorWebImageFanOutIsAtomic(t *testing.T) {
	generator := &fakeCodexWebImageGenerator{
		results:            []webimage.ImageResult{{Base64Data: "aW1hZ2U=", OutputFormat: "png"}},
		failOnCall:         2,
		blockUntilCanceled: true,
	}
	executor := &CodexAutoExecutor{webImageExec: generator}
	_, err := executor.executeWebImage(context.Background(), &cliproxyauth.Auth{ID: "auth-1"},
		cliproxyexecutor.Request{Payload: []byte(`{"model":"gpt-image-web","prompt":"two cats","n":2}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(webimage.SourceFormat), Metadata: map[string]any{"request_path": "/v1/images/generations"}})
	if err == nil || !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("a failed generation must fail the whole request with its own error, got %v", err)
	}
}

func TestCodexAutoExecutorRejectsMultipleResultsPerFanOutCall(t *testing.T) {
	generator := &fakeCodexWebImageGenerator{results: []webimage.ImageResult{
		{Base64Data: "aW1hZ2U=", OutputFormat: "png"},
		{Base64Data: "aW1hZ2U=", OutputFormat: "png"},
	}}
	executor := &CodexAutoExecutor{webImageExec: generator}
	_, _, errGenerate := executor.generateWebImages(context.Background(), webimage.Credentials{AccessToken: "token"}, webimage.Request{Prompt: "draw"}, 2)
	if errGenerate == nil {
		t.Fatal("each fan-out invocation must return exactly one image")
	}
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

func TestBuildCodexWebImagePromptDoesNotForceOrientationForNearSquareSize(t *testing.T) {
	prompt := buildCodexWebImagePrompt("edit", "1469x1461", "", "", "", true)
	if !strings.Contains(prompt, "1469x1461") || !strings.Contains(prompt, "Preserve") {
		t.Fatalf("prompt = %q", prompt)
	}
	for _, unexpected := range []string{"landscape", "portrait", "square composition"} {
		if strings.Contains(prompt, unexpected) {
			t.Fatalf("prompt = %q, unexpected %q", prompt, unexpected)
		}
	}
}

// input_fidelity=high is translated into a faithfulness directive for edits;
// other values and generations leave the prompt untouched.
func TestBuildCodexWebImagePromptInputFidelity(t *testing.T) {
	prompt := buildCodexWebImagePrompt("edit", "", "", "high", "", true)
	if !strings.Contains(prompt, "Reproduce the reference image as faithfully as possible") {
		t.Fatalf("prompt = %q, missing fidelity directive", prompt)
	}
	if !strings.Contains(prompt, "outside the requested edit region") {
		t.Fatalf("prompt = %q, fidelity directive must protect only the unchanged region", prompt)
	}
	prompt = buildCodexWebImagePrompt("edit", "", "", " HIGH ", "", true)
	if !strings.Contains(prompt, "Reproduce the reference image as faithfully as possible") {
		t.Fatalf("prompt = %q, fidelity directive must be case-insensitive", prompt)
	}
	for name, tc := range map[string]struct {
		fidelity string
		isEdit   bool
	}{
		"low fidelity":       {fidelity: "low", isEdit: true},
		"empty fidelity":     {fidelity: "", isEdit: true},
		"high on generation": {fidelity: "high", isEdit: false},
	} {
		if p := buildCodexWebImagePrompt("draw", "", "", tc.fidelity, "", tc.isEdit); strings.Contains(p, "faithfully") {
			t.Fatalf("%s: prompt = %q, unexpected fidelity directive", name, p)
		}
	}
}

// Explicit background modes are translated into prompt directives; transparent
// applies to both generations and edits. Response metadata comes from output.
func TestBuildCodexWebImagePromptTransparentBackground(t *testing.T) {
	for _, isEdit := range []bool{false, true} {
		prompt := buildCodexWebImagePrompt("draw a sticker", "", "", "", "transparent", isEdit)
		if !strings.Contains(prompt, "fully transparent background") {
			t.Fatalf("isEdit=%v prompt = %q, missing transparency directive", isEdit, prompt)
		}
	}
	prompt := buildCodexWebImagePrompt("draw", "", "", "", " TRANSPARENT ", false)
	if !strings.Contains(prompt, "fully transparent background") {
		t.Fatalf("prompt = %q, transparency directive must be case-insensitive", prompt)
	}
	for _, background := range []string{"", "auto"} {
		if p := buildCodexWebImagePrompt("draw", "", "", "", background, false); strings.Contains(p, "transparent") {
			t.Fatalf("background=%q: prompt = %q, unexpected transparency directive", background, p)
		}
	}
	prompt = buildCodexWebImagePrompt("draw", "", "", "", "opaque", false)
	if !strings.Contains(prompt, "fully opaque background") || !strings.Contains(prompt, "transparent pixels") {
		t.Fatalf("opaque background prompt = %q, missing opacity directive", prompt)
	}
}

func TestCodexAutoExecutorReportsActualBackground(t *testing.T) {
	generator := &fakeCodexWebImageGenerator{results: []webimage.ImageResult{{Base64Data: encodeTestOutputPNG(t, false), OutputFormat: "png"}}}
	executor := &CodexAutoExecutor{webImageExec: generator}
	req := cliproxyexecutor.Request{Payload: []byte(`{"prompt":"draw a sticker","background":"transparent"}`)}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString(webimage.SourceFormat),
		Metadata:     map[string]any{cliproxyexecutor.RequestPathMetadataKey: codexImagesGenerationsPath},
	}

	response, errExecute := executor.Execute(context.Background(), &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "token"}}, req, opts)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := gjson.GetBytes(response.Payload, "background").String(); got != "opaque" {
		t.Fatalf("background = %q, body=%s", got, response.Payload)
	}
	if !strings.Contains(generator.request.Prompt, "fully transparent background") {
		t.Fatalf("prompt = %q, missing transparency directive", generator.request.Prompt)
	}

	// Actual transparency wins even when the request asks for opaque output.
	generator.results = []webimage.ImageResult{{Base64Data: encodeTestOutputPNG(t, true), OutputFormat: "png"}}
	transparent := cliproxyexecutor.Request{Payload: []byte(`{"prompt":"draw","background":"opaque"}`)}
	response, errExecute = executor.Execute(context.Background(), &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "token"}}, transparent, opts)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := gjson.GetBytes(response.Payload, "background").String(); got != "transparent" {
		t.Fatalf("background = %q, body=%s", got, response.Payload)
	}

	// WebP is a supported output format and must receive the same alpha check.
	generator.results = []webimage.ImageResult{{Base64Data: "UklGRkQAAABXRUJQVlA4WAoAAAAQAAAAAQAAAQAAQUxQSAUAAAAAAAAAAABWUDggGAAAADABAJ0BKgIAAgACADQlpAADcAD++/1QAA==", OutputFormat: "webp"}}
	response, errExecute = executor.Execute(context.Background(), &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "token"}}, transparent, opts)
	if errExecute != nil {
		t.Fatalf("Execute() WebP error = %v", errExecute)
	}
	if got := gjson.GetBytes(response.Payload, "background").String(); got != "transparent" {
		t.Fatalf("WebP background = %q, body=%s", got, response.Payload)
	}

	// Invalid output bytes and out-of-contract requests must not invent metadata.
	generator.results = []webimage.ImageResult{{Base64Data: "AA==", OutputFormat: "png"}}
	garbage := cliproxyexecutor.Request{Payload: []byte(`{"prompt":"draw","background":"green"}`)}
	response, errExecute = executor.Execute(context.Background(), &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "token"}}, garbage, opts)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if gjson.GetBytes(response.Payload, "background").Exists() {
		t.Fatalf("garbage background must not be echoed: %s", response.Payload)
	}
}

func TestCodexAutoExecutorRejectsIncompatibleOutputEncodingAtExecutorBoundary(t *testing.T) {
	generator := &fakeCodexWebImageGenerator{results: []webimage.ImageResult{{Base64Data: encodeTestOutputPNG(t, true), OutputFormat: "png"}}}
	executor := &CodexAutoExecutor{webImageExec: generator}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString(webimage.SourceFormat),
		Metadata:     map[string]any{cliproxyexecutor.RequestPathMetadataKey: codexImagesGenerationsPath},
	}
	for _, payload := range []string{
		`{"prompt":"draw","background":"transparent","output_format":"jpeg"}`,
		`{"prompt":"draw","output_compression":50}`,
		`{"prompt":"draw","output_format":"png","output_compression":50}`,
	} {
		_, errExecute := executor.Execute(context.Background(), &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "token"}}, cliproxyexecutor.Request{
			Payload: []byte(payload),
		}, opts)
		if errExecute == nil {
			t.Fatalf("payload %s should be rejected", payload)
		}
	}
	if generator.calls != 0 {
		t.Fatalf("invalid requests reached generator %d times", generator.calls)
	}
}

func TestCodexAutoExecutorRejectsMalformedNAtExecutorBoundary(t *testing.T) {
	for _, rawN := range []string{`"2"`, `1.5`, `0`, `11`} {
		generator := &fakeCodexWebImageGenerator{results: []webimage.ImageResult{{Base64Data: "aW1hZ2U=", OutputFormat: "png"}}}
		executor := &CodexAutoExecutor{webImageExec: generator}
		_, errExecute := executor.executeWebImage(context.Background(), &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "token"}}, cliproxyexecutor.Request{
			Payload: []byte(`{"prompt":"draw","n":` + rawN + `}`),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(webimage.SourceFormat), Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: codexImagesGenerationsPath}})
		if errExecute == nil {
			t.Fatalf("n=%s should be rejected by executor", rawN)
		}
	}
}

func TestCodexImageStreamHonorsRequestedOutputFormatForAllFrames(t *testing.T) {
	partial := []byte(`{"type":"response.image_generation_call.partial_image","partial_image_b64":"` + encodeTestOutputPNG(t, false) + `","partial_image_index":0,"output_format":"png"}`)
	frame := codexBuildImagePartialFrame(partial, "b64_json", "image_generation", "jpeg", 80)
	if !strings.Contains(string(frame), `"output_format":"jpeg"`) {
		t.Fatalf("partial frame missing requested output format: %s", frame)
	}
	partialB64 := gjson.GetBytes(frame, "b64_json").String()
	if partialB64 == "" || sniffImageBase64Format(t, partialB64) != "jpeg" {
		t.Fatalf("partial frame is not JPEG: %s", frame)
	}

	completed := codexBuildImageCompletedFrame(codexImageCallResult{Result: encodeTestOutputPNG(t, false), OutputFormat: "png"}, nil, "b64_json", "image_generation")
	if !strings.Contains(string(completed), `"output_format":"png"`) {
		t.Fatalf("completed frame must expose actual output format on fallback: %s", completed)
	}
}

func sniffImageBase64Format(t *testing.T, value string) string {
	t.Helper()
	raw, errDecode := base64.StdEncoding.DecodeString(value)
	if errDecode != nil {
		t.Fatalf("decode image: %v", errDecode)
	}
	_, format, errConfig := image.DecodeConfig(bytes.NewReader(raw))
	if errConfig != nil {
		t.Fatalf("decode image config: %v", errConfig)
	}
	return format
}

func encodeTestOutputPNG(t *testing.T, transparent bool) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	if transparent {
		img.SetNRGBA(0, 0, color.NRGBA{})
	}
	var buffer bytes.Buffer
	if errEncode := png.Encode(&buffer, img); errEncode != nil {
		t.Fatalf("png.Encode() error = %v", errEncode)
	}
	return base64.StdEncoding.EncodeToString(buffer.Bytes())
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
	for _, want := range []string{"high", "2x3", "Preserve", "dimensions", "aspect ratio"} {
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
