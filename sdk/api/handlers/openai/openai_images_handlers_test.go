package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

type webImageHandlerCaptureExecutor struct {
	authID string
	req    cliproxyexecutor.Request
	opts   cliproxyexecutor.Options
}

func (e *webImageHandlerCaptureExecutor) Identifier() string { return "codex" }

func (e *webImageHandlerCaptureExecutor) Execute(_ context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.authID = auth.ID
	e.req = req
	e.opts = opts
	return cliproxyexecutor.Response{Payload: []byte(`{"created":123,"data":[{"b64_json":"aW1hZ2U="}],"size":"1024x1024"}`)}, nil
}

func (e *webImageHandlerCaptureExecutor) ExecuteStream(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *webImageHandlerCaptureExecutor) Refresh(_ context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth, nil
}

func (e *webImageHandlerCaptureExecutor) CountTokens(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *webImageHandlerCaptureExecutor) HttpRequest(context.Context, *cliproxyauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func performImagesEndpointRequest(t *testing.T, endpointPath string, contentType string, body io.Reader, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST(endpointPath, handler)

	req := httptest.NewRequest(http.MethodPost, endpointPath, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

func assertUnsupportedImagesModelResponse(t *testing.T, resp *httptest.ResponseRecorder, model string) {
	t.Helper()

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}

	message := gjson.GetBytes(resp.Body.Bytes(), "error.message").String()
	expectedMessage := "Model " + model + " is not supported on " + imagesGenerationsPath + " or " + imagesEditsPath + ". Use " + gptImage15Model + ", " + defaultImagesToolModel + ", " + defaultXAIImagesModel + ", " + xaiImagesQualityModel + ", or a configured openai-compatibility image model."
	if message != expectedMessage {
		t.Fatalf("error message = %q, want %q", message, expectedMessage)
	}
	if errorType := gjson.GetBytes(resp.Body.Bytes(), "error.type").String(); errorType != "invalid_request_error" {
		t.Fatalf("error type = %q, want invalid_request_error", errorType)
	}
}

func TestImagesModelValidationAllowsGPTImageAndXAIModels(t *testing.T) {
	for _, model := range []string{"gpt-image-1.5", "codex/gpt-image-1.5", "gpt-image-2", "codex/gpt-image-2", "grok-imagine-image", "xai/grok-imagine-image", "grok-imagine-image-quality", "xai/grok-imagine-image-quality"} {
		if !isSupportedImagesModel(model) {
			t.Fatalf("expected %s to be supported", model)
		}
	}
	if isSupportedImagesModel("gpt-5.4-mini") {
		t.Fatal("expected gpt-5.4-mini to be rejected")
	}
	if isSupportedImagesModel("codex/grok-imagine-image") {
		t.Fatal("expected codex/grok-imagine-image to be rejected")
	}
}

func TestWebImageModelMatchingUsesConfiguredAliases(t *testing.T) {
	cfg := &sdkconfig.SDKConfig{WebImageConfig: sdkconfig.WebImageConfig{WebImageModels: []string{"gpt-image-web", " custom-web-image "}}}
	for _, model := range []string{"gpt-image-web", "codex/gpt-image-web", "custom-web-image", "codex/custom-web-image"} {
		if !isWebImageModel(cfg, model) {
			t.Fatalf("isWebImageModel(%q) = false", model)
		}
	}
	if isWebImageModel(cfg, "gpt-image-2") {
		t.Fatal("gpt-image-2 must not route to web image")
	}
}

func TestImagesGenerationsWebImageDisabledReturns404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	handler := &OpenAIAPIHandler{BaseAPIHandler: base}
	body := strings.NewReader(`{"model":"gpt-image-web","prompt":"draw"}`)

	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusNotFound, resp.Body.String())
	}
}

func TestImagesEditsWebImageDisabledReturns404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	handler := &OpenAIAPIHandler{BaseAPIHandler: base}

	t.Run("json", func(t *testing.T) {
		body := strings.NewReader(`{"model":"gpt-image-web","prompt":"edit"}`)
		resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", body, handler.ImagesEdits)
		if resp.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusNotFound, resp.Body.String())
		}
	})

	t.Run("multipart", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if errWrite := writer.WriteField("model", "gpt-image-web"); errWrite != nil {
			t.Fatalf("write model field: %v", errWrite)
		}
		if errWrite := writer.WriteField("prompt", "edit"); errWrite != nil {
			t.Fatalf("write prompt field: %v", errWrite)
		}
		if errClose := writer.Close(); errClose != nil {
			t.Fatalf("close multipart writer: %v", errClose)
		}

		resp := performImagesEndpointRequest(t, imagesEditsPath, writer.FormDataContentType(), &body, handler.ImagesEdits)
		if resp.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusNotFound, resp.Body.String())
		}
	})
}

func TestValidateWebImageGenerationRequest(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "valid defaults", body: `{"model":"gpt-image-web","prompt":"draw"}`},
		{name: "valid explicit", body: `{"model":"gpt-image-web","prompt":"draw","n":1,"size":"1024x1024","response_format":"b64_json"}`},
		{name: "too many", body: `{"model":"gpt-image-web","prompt":"draw","n":11}`, want: "n must be an integer between 1 and 10"},
		{name: "string n", body: `{"model":"gpt-image-web","prompt":"draw","n":"3"}`, want: "n must be an integer between 1 and 10"},
		{name: "unsupported output format", body: `{"model":"gpt-image-web","prompt":"draw","output_format":"gif"}`, want: "output_format"},
		{name: "jpg alias", body: `{"model":"gpt-image-web","prompt":"draw","output_format":"jpg"}`, want: "output_format"},
		{name: "compression without lossy format", body: `{"model":"gpt-image-web","prompt":"draw","output_compression":50}`, want: "output_compression requires output_format"},
		{name: "compression with png", body: `{"model":"gpt-image-web","prompt":"draw","output_format":"png","output_compression":50}`, want: "output_compression requires output_format"},
		{name: "transparent jpeg", body: `{"model":"gpt-image-web","prompt":"draw","background":"transparent","output_format":"jpeg"}`, want: "transparent background requires output_format"},
		{name: "transparent webp", body: `{"model":"gpt-image-web","prompt":"draw","background":"transparent","output_format":"webp"}`},
		{name: "fractional compression", body: `{"model":"gpt-image-web","prompt":"draw","output_compression":10.5}`, want: "output_compression"},
		{name: "compression too high", body: `{"model":"gpt-image-web","prompt":"draw","output_compression":101}`, want: "output_compression"},
		{name: "compression negative", body: `{"model":"gpt-image-web","prompt":"draw","output_compression":-1}`, want: "output_compression"},
		{name: "stream", body: `{"model":"gpt-image-web","prompt":"draw","stream":true}`, want: "streaming is not supported"},
		{name: "url", body: `{"model":"gpt-image-web","prompt":"draw","response_format":"url"}`, want: "response_format must be b64_json"},
		{name: "large", body: `{"model":"gpt-image-web","prompt":"draw","size":"2048x2048"}`},
		{name: "custom size and quality", body: `{"model":"gpt-image-web","prompt":"draw","size":"cinema-wide-custom","quality":"maximum-detail"}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errValidate := validateWebImageGenerationRequest([]byte(test.body))
			if test.want == "" {
				if errValidate != nil {
					t.Fatalf("validateWebImageGenerationRequest() error = %v", errValidate)
				}
				return
			}
			if errValidate == nil || !strings.Contains(errValidate.Error(), test.want) {
				t.Fatalf("validateWebImageGenerationRequest() error = %v, want containing %q", errValidate, test.want)
			}
		})
	}
}

func TestImagesEndpointsRejectIncompatibleEncodingBeforeModelRouting(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	handler := &OpenAIAPIHandler{BaseAPIHandler: base}

	generation := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", strings.NewReader(
		`{"model":"gpt-image-2","prompt":"draw","output_format":"png","output_compression":50}`,
	), handler.ImagesGenerations)
	if generation.Code != http.StatusBadRequest || !strings.Contains(generation.Body.String(), "output_compression requires output_format") {
		t.Fatalf("generation status=%d body=%s", generation.Code, generation.Body.String())
	}

	jsonEdit := performImagesEndpointRequest(t, imagesEditsPath, "application/json", strings.NewReader(
		`{"model":"gpt-image-2","prompt":"edit","images":[{"image_url":"data:image/png;base64,AA=="}],"background":"transparent","output_format":"jpeg"}`,
	), handler.ImagesEdits)
	if jsonEdit.Code != http.StatusBadRequest || !strings.Contains(jsonEdit.Body.String(), "transparent background requires output_format") {
		t.Fatalf("JSON edit status=%d body=%s", jsonEdit.Code, jsonEdit.Body.String())
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{
		"model":              "gpt-image-2",
		"prompt":             "edit",
		"output_format":      "png",
		"output_compression": "50",
	} {
		if errWrite := writer.WriteField(key, value); errWrite != nil {
			t.Fatalf("write %s: %v", key, errWrite)
		}
	}
	file, errFile := writer.CreateFormFile("image", "reference.png")
	if errFile != nil {
		t.Fatalf("create image: %v", errFile)
	}
	if _, errWrite := file.Write([]byte("image")); errWrite != nil {
		t.Fatalf("write image: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart: %v", errClose)
	}
	multipartEdit := performImagesEndpointRequest(t, imagesEditsPath, writer.FormDataContentType(), &body, handler.ImagesEdits)
	if multipartEdit.Code != http.StatusBadRequest || !strings.Contains(multipartEdit.Body.String(), "output_compression requires output_format") {
		t.Fatalf("multipart edit status=%d body=%s", multipartEdit.Code, multipartEdit.Body.String())
	}
}

func TestBuildWebImageMultipartPreservesInvalidCompressionForValidation(t *testing.T) {
	form := &multipart.Form{Value: map[string][]string{"output_compression": {"10.5"}}}
	payload, errBuild := buildWebImageMultipartEditRequest(form, "custom-web-image", "edit", nil, nil)
	if errBuild != nil {
		t.Fatalf("build multipart request: %v", errBuild)
	}
	if errValidate := validateWebImageGenerationRequest(payload); errValidate == nil || !strings.Contains(errValidate.Error(), "output_compression") {
		t.Fatalf("invalid compression should reach shared validation, got %v", errValidate)
	}
}

func TestImagesGenerationsWebImageRejectsUnsupportedShapesBeforeExecution(t *testing.T) {
	cfg := &sdkconfig.SDKConfig{WebImageConfig: sdkconfig.WebImageConfig{
		WebImageGeneration: true,
		WebImageFreeOnly:   true,
		WebImageModels:     []string{"gpt-image-web"},
		WebImageBaseModel:  "internal-image-model",
	}}
	base := handlers.NewBaseAPIHandlers(cfg, nil)
	handler := &OpenAIAPIHandler{BaseAPIHandler: base}

	for _, body := range []string{
		`{"model":"gpt-image-web","prompt":"draw","n":11}`,
		`{"model":"gpt-image-web","prompt":"draw","stream":true}`,
		`{"model":"gpt-image-web","prompt":"draw","response_format":"url"}`,
	} {
		resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", strings.NewReader(body), handler.ImagesGenerations)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, want %d: %s", body, resp.Code, http.StatusBadRequest, resp.Body.String())
		}
	}
}

func TestImagesGenerationsWebImageRoutesThroughFreeCodexAuth(t *testing.T) {
	model := "custom-web-image"
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient("web-free", "codex", []*registry.ModelInfo{{ID: internalconfig.DefaultWebImageModel}})
	modelRegistry.RegisterClient("web-plus", "codex", []*registry.ModelInfo{{ID: internalconfig.DefaultWebImageModel}})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient("web-free")
		modelRegistry.UnregisterClient("web-plus")
	})

	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	executor := &webImageHandlerCaptureExecutor{}
	manager.RegisterExecutor(executor)
	for _, auth := range []*cliproxyauth.Auth{
		{ID: "web-plus", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}},
		{ID: "web-free", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}

	cfg := &sdkconfig.SDKConfig{WebImageConfig: sdkconfig.WebImageConfig{
		WebImageGeneration: true,
		WebImageFreeOnly:   true,
		WebImageModels:     []string{model},
		WebImageBaseModel:  "internal-image-model",
	}}
	base := handlers.NewBaseAPIHandlers(cfg, manager)
	handler := &OpenAIAPIHandler{BaseAPIHandler: base}
	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", strings.NewReader(`{"model":"custom-web-image","prompt":"draw"}`), handler.ImagesGenerations)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	if executor.authID != "web-free" {
		t.Fatalf("selected auth = %q, want web-free", executor.authID)
	}
	if executor.opts.SourceFormat.String() != webImagesHandlerType {
		t.Fatalf("SourceFormat = %q, want %q", executor.opts.SourceFormat, webImagesHandlerType)
	}
	if executor.opts.ResponseFormat.String() != xaiImagesHandlerType {
		t.Fatalf("ResponseFormat = %q, want %q", executor.opts.ResponseFormat, xaiImagesHandlerType)
	}
	if executor.opts.Metadata[cliproxyexecutor.OnlyFreeAuthMetadataKey] != true {
		t.Fatalf("OnlyFreeAuthMetadataKey = %v", executor.opts.Metadata[cliproxyexecutor.OnlyFreeAuthMetadataKey])
	}
	if executor.opts.Metadata[cliproxyexecutor.AuthSelectionModelMetadataKey] != internalconfig.DefaultWebImageModel {
		t.Fatalf("AuthSelectionModelMetadataKey = %v", executor.opts.Metadata[cliproxyexecutor.AuthSelectionModelMetadataKey])
	}
}

func TestImagesEditsWebImageRoutesMultipartRequest(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient("web-edit", "codex", []*registry.ModelInfo{{ID: internalconfig.DefaultWebImageModel}})
	t.Cleanup(func() { modelRegistry.UnregisterClient("web-edit") })

	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	executor := &webImageHandlerCaptureExecutor{}
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{ID: "web-edit", Provider: "codex"}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	cfg := &sdkconfig.SDKConfig{WebImageConfig: sdkconfig.WebImageConfig{
		WebImageGeneration: true,
		WebImageModels:     []string{"custom-web-image"},
		WebImageBaseModel:  "internal-image-model",
	}}
	handler := &OpenAIAPIHandler{BaseAPIHandler: handlers.NewBaseAPIHandlers(cfg, manager)}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, value := range map[string]string{
		"model":          "custom-web-image",
		"prompt":         "turn this into a poster",
		"size":           "1600x900",
		"quality":        "maximum-detail",
		"input_fidelity": "high",
		"background":     "transparent",
	} {
		if errWrite := writer.WriteField(name, value); errWrite != nil {
			t.Fatalf("write %s: %v", name, errWrite)
		}
	}
	partHeader := make(textproto.MIMEHeader)
	partHeader.Set("Content-Disposition", `form-data; name="image"; filename="reference.png"`)
	partHeader.Set("Content-Type", "image/png")
	part, errPart := writer.CreatePart(partHeader)
	if errPart != nil {
		t.Fatalf("CreatePart() error = %v", errPart)
	}
	if _, errWrite := part.Write(maskPNG(t, 100, 100, image.Rect(0, 0, 100, 100))); errWrite != nil {
		t.Fatalf("write image: %v", errWrite)
	}
	maskPart, errMaskPart := writer.CreateFormFile("mask", "mask.png")
	if errMaskPart != nil {
		t.Fatalf("CreateFormFile(mask) error = %v", errMaskPart)
	}
	if _, errWrite := maskPart.Write(maskPNG(t, 100, 100, image.Rect(50, 0, 100, 50))); errWrite != nil {
		t.Fatalf("write mask: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	resp := performImagesEndpointRequest(t, imagesEditsPath, writer.FormDataContentType(), &body, handler.ImagesEdits)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	payload := executor.opts.OriginalRequest
	if got := gjson.GetBytes(payload, "prompt").String(); !strings.HasPrefix(got, "turn this into a poster\n\n") || !strings.Contains(got, "top-right area") {
		t.Fatalf("prompt = %q, payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "size").String(); got != "1600x900" {
		t.Fatalf("size = %q, payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "quality").String(); got != "maximum-detail" {
		t.Fatalf("quality = %q, payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "input_fidelity").String(); got != "high" {
		t.Fatalf("input_fidelity = %q, payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "background").String(); got != "transparent" {
		t.Fatalf("background = %q, payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "images.0.filename").String(); got != "reference.png" {
		t.Fatalf("filename = %q, payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "images.0.image_url").String(); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("image_url = %q, payload=%s", got, payload)
	}
	if got := executor.opts.Metadata[cliproxyexecutor.RequestPathMetadataKey]; got != imagesEditsPath {
		t.Fatalf("request path = %v, want %s", got, imagesEditsPath)
	}

	executor.req = cliproxyexecutor.Request{}
	var invalidBody bytes.Buffer
	invalidWriter := multipart.NewWriter(&invalidBody)
	for name, value := range map[string]string{"model": "custom-web-image", "prompt": "edit this"} {
		if errWrite := invalidWriter.WriteField(name, value); errWrite != nil {
			t.Fatalf("write invalid %s: %v", name, errWrite)
		}
	}
	invalidImage, errInvalidImage := invalidWriter.CreateFormFile("image", "reference.png")
	if errInvalidImage != nil {
		t.Fatalf("CreateFormFile(invalid image) error = %v", errInvalidImage)
	}
	if _, errWrite := invalidImage.Write([]byte("png-data")); errWrite != nil {
		t.Fatalf("write invalid request image: %v", errWrite)
	}
	invalidMask, errInvalidMask := invalidWriter.CreateFormFile("mask", "mask.png")
	if errInvalidMask != nil {
		t.Fatalf("CreateFormFile(invalid mask) error = %v", errInvalidMask)
	}
	if _, errWrite := invalidMask.Write([]byte("not-an-image")); errWrite != nil {
		t.Fatalf("write invalid request mask: %v", errWrite)
	}
	if errClose := invalidWriter.Close(); errClose != nil {
		t.Fatalf("close invalid multipart writer: %v", errClose)
	}
	resp = performImagesEndpointRequest(t, imagesEditsPath, invalidWriter.FormDataContentType(), &invalidBody, handler.ImagesEdits)
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "mask could not be decoded") {
		t.Fatalf("invalid mask status = %d body = %s", resp.Code, resp.Body.String())
	}
	if executor.req.Payload != nil {
		t.Fatalf("invalid multipart mask reached executor: %s", executor.req.Payload)
	}
}

func TestImagesEditsWebImageTranslatesJSONMask(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient("web-edit-json", "codex", []*registry.ModelInfo{{ID: internalconfig.DefaultWebImageModel}})
	t.Cleanup(func() { modelRegistry.UnregisterClient("web-edit-json") })

	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	executor := &webImageHandlerCaptureExecutor{}
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{ID: "web-edit-json", Provider: "codex"}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	cfg := &sdkconfig.SDKConfig{WebImageConfig: sdkconfig.WebImageConfig{
		WebImageGeneration: true,
		WebImageModels:     []string{"custom-web-image"},
		WebImageBaseModel:  "internal-image-model",
	}}
	handler := &OpenAIAPIHandler{BaseAPIHandler: handlers.NewBaseAPIHandlers(cfg, manager)}
	maskURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(maskPNG(t, 100, 100, image.Rect(50, 0, 100, 50)))
	referenceURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(maskPNG(t, 100, 100, image.Rect(0, 0, 100, 100)))
	body := `{"model":"custom-web-image","prompt":"edit this","images":[{"image_url":"` + referenceURL + `"}],"mask":"` + maskURL + `"}`

	resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", strings.NewReader(body), handler.ImagesEdits)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	payload := executor.opts.OriginalRequest
	if prompt := gjson.GetBytes(payload, "prompt").String(); !strings.Contains(prompt, "top-right area") {
		t.Fatalf("prompt = %q, payload=%s", prompt, payload)
	}
	if gjson.GetBytes(payload, "mask").Exists() {
		t.Fatalf("mask leaked into routed payload: %s", payload)
	}

	executor.req = cliproxyexecutor.Request{}
	invalidBody := `{"model":"custom-web-image","prompt":"edit this","images":[{"image_url":"` + referenceURL + `"}],"mask":"not-a-data-url"}`
	resp = performImagesEndpointRequest(t, imagesEditsPath, "application/json", strings.NewReader(invalidBody), handler.ImagesEdits)
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "mask must be provided as a data URL") {
		t.Fatalf("status = %d body = %s", resp.Code, resp.Body.String())
	}
	if executor.req.Payload != nil {
		t.Fatalf("invalid mask reached executor: %s", executor.req.Payload)
	}
}

func TestImagesEditsWebImageRejectsMultipartInputOverConfiguredLimit(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient("web-edit-limit", "codex", []*registry.ModelInfo{{ID: internalconfig.DefaultWebImageModel}})
	t.Cleanup(func() { modelRegistry.UnregisterClient("web-edit-limit") })

	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	executor := &webImageHandlerCaptureExecutor{}
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{ID: "web-edit-limit", Provider: "codex"}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	cfg := &sdkconfig.SDKConfig{WebImageConfig: sdkconfig.WebImageConfig{
		WebImageGeneration: true,
		WebImageModels:     []string{"custom-web-image"},
		WebImageBaseModel:  "internal-image-model",
		WebImageMaxBytes:   4,
	}}
	handler := &OpenAIAPIHandler{BaseAPIHandler: handlers.NewBaseAPIHandlers(cfg, manager)}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if errWrite := writer.WriteField("model", "custom-web-image"); errWrite != nil {
		t.Fatalf("write model: %v", errWrite)
	}
	if errWrite := writer.WriteField("prompt", "edit"); errWrite != nil {
		t.Fatalf("write prompt: %v", errWrite)
	}
	part, errPart := writer.CreateFormFile("image", "reference.png")
	if errPart != nil {
		t.Fatalf("CreateFormFile() error = %v", errPart)
	}
	if _, errWrite := part.Write([]byte("12345")); errWrite != nil {
		t.Fatalf("write image: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	resp := performImagesEndpointRequest(t, imagesEditsPath, writer.FormDataContentType(), &body, handler.ImagesEdits)
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "byte limit") {
		t.Fatalf("status = %d body = %s", resp.Code, resp.Body.String())
	}
	if executor.req.Payload != nil {
		t.Fatalf("executor request = %s", executor.req.Payload)
	}
}

func TestImagesModelValidationAllowsOpenAICompatImageModels(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	clientID := "test-openai-compat-image-model-validation"
	modelRegistry.RegisterClient(clientID, "openai-compatibility", []*registry.ModelInfo{
		{ID: "compat-image-model", Object: "model", OwnedBy: "compat", Type: registry.OpenAIImageModelType},
		{ID: "compat-chat-model", Object: "model", OwnedBy: "compat", Type: "openai-compatibility"},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	if !isSupportedImagesModel("compat-image-model") {
		t.Fatal("expected configured openai-compatibility image model to be supported")
	}
	if isSupportedImagesModel("compat-chat-model") {
		t.Fatal("expected non-image openai-compatibility model to be rejected")
	}
}

func TestBuildXAIImagesGenerationsRequest(t *testing.T) {
	rawJSON := []byte(`{"model":"xai/grok-imagine-image-quality","prompt":"abstract art","aspect_ratio":"landscape","resolution":"2k","n":2,"response_format":"url"}`)

	req := buildXAIImagesGenerationsRequest(rawJSON, "xai/grok-imagine-image-quality", "url")

	if got := gjson.GetBytes(req, "model").String(); got != "grok-imagine-image-quality" {
		t.Fatalf("model = %q, want grok-imagine-image-quality", got)
	}
	if got := gjson.GetBytes(req, "prompt").String(); got != "abstract art" {
		t.Fatalf("prompt = %q, want abstract art", got)
	}
	if got := gjson.GetBytes(req, "aspect_ratio").String(); got != "16:9" {
		t.Fatalf("aspect_ratio = %q, want 16:9", got)
	}
	if got := gjson.GetBytes(req, "resolution").String(); got != "2k" {
		t.Fatalf("resolution = %q, want 2k", got)
	}
	if got := gjson.GetBytes(req, "response_format").String(); got != "url" {
		t.Fatalf("response_format = %q, want url", got)
	}
	if got := gjson.GetBytes(req, "n").Int(); got != 2 {
		t.Fatalf("n = %d, want 2", got)
	}
}

func TestBuildXAIImagesEditRequest(t *testing.T) {
	req := buildXAIImagesEditRequest("grok-imagine-image", "edit it", []string{"data:image/png;base64,AA==", "https://example.com/image.png"}, "b64_json", "3:2", "1k", 0)

	if got := gjson.GetBytes(req, "model").String(); got != "grok-imagine-image" {
		t.Fatalf("model = %q, want grok-imagine-image", got)
	}
	if got := gjson.GetBytes(req, "images.0.type").String(); got != "image_url" {
		t.Fatalf("images.0.type = %q, want image_url", got)
	}
	if got := gjson.GetBytes(req, "images.0.url").String(); got != "data:image/png;base64,AA==" {
		t.Fatalf("images.0.url = %q", got)
	}
	if got := gjson.GetBytes(req, "images.1.url").String(); got != "https://example.com/image.png" {
		t.Fatalf("images.1.url = %q", got)
	}
	if gjson.GetBytes(req, "image").Exists() {
		t.Fatalf("multiple image edits must use images array: %s", string(req))
	}
}

func TestBuildXAIImagesEditRequestSingleImage(t *testing.T) {
	req := buildXAIImagesEditRequest("grok-imagine-image", "edit it", []string{"https://example.com/image.png"}, "url", "", "", 0)

	if got := gjson.GetBytes(req, "image.type").String(); got != "image_url" {
		t.Fatalf("image.type = %q, want image_url", got)
	}
	if got := gjson.GetBytes(req, "image.url").String(); got != "https://example.com/image.png" {
		t.Fatalf("image.url = %q", got)
	}
	if gjson.GetBytes(req, "images").Exists() {
		t.Fatalf("single image edit must use image object: %s", string(req))
	}
}

func TestBuildOpenAICompatImagesJSONRequestPreservesStreamForStreaming(t *testing.T) {
	req := buildOpenAICompatImagesJSONRequest([]byte(`{"model":"compat-image","prompt":"draw","stream":false}`), "upstream-image", true)

	if got := gjson.GetBytes(req, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, string(req))
	}
	if !gjson.GetBytes(req, "stream").Bool() {
		t.Fatalf("stream flag missing: %s", string(req))
	}
}

func TestBuildOpenAICompatImagesJSONRequestDropsStreamForNonStreaming(t *testing.T) {
	req := buildOpenAICompatImagesJSONRequest([]byte(`{"model":"compat-image","prompt":"draw","stream":true}`), "upstream-image", false)

	if got := gjson.GetBytes(req, "model").String(); got != "upstream-image" {
		t.Fatalf("model = %q, want upstream-image; body=%s", got, string(req))
	}
	if gjson.GetBytes(req, "stream").Exists() {
		t.Fatalf("stream flag should be removed from non-streaming request: %s", string(req))
	}
}

func TestBuildOpenAICompatImagesMultipartRequestPreservesStreamAndFileContentType(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if errWrite := writer.WriteField("model", "compat-image"); errWrite != nil {
		t.Fatalf("write model field: %v", errWrite)
	}
	if errWrite := writer.WriteField("stream", "false"); errWrite != nil {
		t.Fatalf("write stream field: %v", errWrite)
	}
	if errWrite := writer.WriteField("prompt", "edit"); errWrite != nil {
		t.Fatalf("write prompt field: %v", errWrite)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", multipart.FileContentDisposition("image", "image.png"))
	header.Set("Content-Type", "image/png")
	part, errCreate := writer.CreatePart(header)
	if errCreate != nil {
		t.Fatalf("create image field: %v", errCreate)
	}
	if _, errWrite := part.Write([]byte("png-data")); errWrite != nil {
		t.Fatalf("write image field: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	reader := multipart.NewReader(bytes.NewReader(body.Bytes()), writer.Boundary())
	form, errRead := reader.ReadForm(32 << 20)
	if errRead != nil {
		t.Fatalf("read source form: %v", errRead)
	}
	defer func() {
		if errRemove := form.RemoveAll(); errRemove != nil {
			t.Fatalf("remove source form files: %v", errRemove)
		}
	}()

	out, contentType, errBuild := buildOpenAICompatImagesMultipartRequest(form, "upstream-image", true)
	if errBuild != nil {
		t.Fatalf("buildOpenAICompatImagesMultipartRequest error: %v", errBuild)
	}
	mediaType, params, errParse := mime.ParseMediaType(contentType)
	if errParse != nil {
		t.Fatalf("parse content type: %v", errParse)
	}
	if mediaType != "multipart/form-data" {
		t.Fatalf("media type = %q, want multipart/form-data", mediaType)
	}
	rewrittenReader := multipart.NewReader(bytes.NewReader(out), params["boundary"])
	rewrittenForm, errRead := rewrittenReader.ReadForm(32 << 20)
	if errRead != nil {
		t.Fatalf("read rewritten form: %v", errRead)
	}
	defer func() {
		if errRemove := rewrittenForm.RemoveAll(); errRemove != nil {
			t.Fatalf("remove rewritten form files: %v", errRemove)
		}
	}()
	if got := rewrittenForm.Value["model"]; len(got) != 1 || got[0] != "upstream-image" {
		t.Fatalf("model values = %#v, want upstream-image", got)
	}
	if got := rewrittenForm.Value["stream"]; len(got) != 1 || got[0] != "true" {
		t.Fatalf("stream values = %#v, want true", got)
	}
	if got := rewrittenForm.Value["prompt"]; len(got) != 1 || got[0] != "edit" {
		t.Fatalf("prompt values = %#v, want edit", got)
	}
	if got := rewrittenForm.File["image"]; len(got) != 1 || got[0].Header.Get("Content-Type") != "image/png" {
		t.Fatalf("image headers = %#v, want image/png", got)
	}
}

func TestBuildImagesAPIResponseFromXAI(t *testing.T) {
	payload := []byte(`{"created":123,"data":[{"b64_json":"AA==","revised_prompt":"refined","mime_type":"image/png"}],"usage":{"total_tokens":0}}`)

	out, err := buildImagesAPIResponseFromXAI(payload, "b64_json")
	if err != nil {
		t.Fatalf("buildImagesAPIResponseFromXAI() error = %v", err)
	}

	if got := gjson.GetBytes(out, "created").Int(); got != 123 {
		t.Fatalf("created = %d, want 123", got)
	}
	if got := gjson.GetBytes(out, "data.0.b64_json").String(); got != "AA==" {
		t.Fatalf("data.0.b64_json = %q, want AA==", got)
	}
	if got := gjson.GetBytes(out, "data.0.revised_prompt").String(); got != "refined" {
		t.Fatalf("data.0.revised_prompt = %q, want refined", got)
	}
	if !gjson.GetBytes(out, "usage").Exists() {
		t.Fatalf("usage missing: %s", string(out))
	}
}

func TestImagesGenerationsRejectsUnsupportedModel(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"draw a square"}`)

	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)

	assertUnsupportedImagesModelResponse(t, resp, "gpt-5.4-mini")
}

func TestImagesEditsJSONRejectsUnsupportedModel(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"edit this","images":[{"image_url":"data:image/png;base64,AA=="}]}`)

	resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", body, handler.ImagesEdits)

	assertUnsupportedImagesModelResponse(t, resp, "gpt-5.4-mini")
}

func TestImagesEditsMultipartRejectsUnsupportedModel(t *testing.T) {
	handler := &OpenAIAPIHandler{}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "gpt-5.4-mini"); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	if err := writer.WriteField("prompt", "edit this"); err != nil {
		t.Fatalf("write prompt field: %v", err)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	resp := performImagesEndpointRequest(t, imagesEditsPath, writer.FormDataContentType(), &body, handler.ImagesEdits)

	assertUnsupportedImagesModelResponse(t, resp, "gpt-5.4-mini")
}

func TestImagesGenerations_DisableImageGeneration_Returns404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationAll}, nil)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"prompt":"draw a square"}`)

	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusNotFound, resp.Body.String())
	}
}

func TestImagesEdits_DisableImageGeneration_Returns404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationAll}, nil)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"prompt":"edit this","images":[{"image_url":"data:image/png;base64,AA=="}]}`)

	resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", body, handler.ImagesEdits)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusNotFound, resp.Body.String())
	}
}

func TestImagesGenerations_DisableImageGenerationChat_DoesNotReturn404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationChat}, nil)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"draw a square"}`)

	resp := performImagesEndpointRequest(t, imagesGenerationsPath, "application/json", body, handler.ImagesGenerations)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
}

func TestImagesEdits_DisableImageGenerationChat_DoesNotReturn404(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{DisableImageGeneration: internalconfig.DisableImageGenerationChat}, nil)
	handler := NewOpenAIAPIHandler(base)
	body := strings.NewReader(`{"model":"gpt-5.4-mini","prompt":"edit this","images":[{"image_url":"data:image/png;base64,AA=="}]}`)

	resp := performImagesEndpointRequest(t, imagesEditsPath, "application/json", body, handler.ImagesEdits)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
}
