package webimage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestGenerateRunsWebConversationProtocol(t *testing.T) {
	var mu sync.Mutex
	paths := make([]string, 0, 8)
	prepareParentMessageID := ""
	v2ProofToken := ""
	imageBytes := []byte("fake-png-data")

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()

		if strings.HasPrefix(r.URL.Path, "/backend-api/") {
			if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
				t.Errorf("%s Authorization = %q", r.URL.Path, got)
			}
			if got := r.Header.Get("OAI-Device-ID"); got == "" {
				t.Errorf("%s missing OAI-Device-ID", r.URL.Path)
			}
			if got := r.Header.Get("OAI-Session-ID"); got == "" {
				t.Errorf("%s missing OAI-Session-ID", r.URL.Path)
			}
			if got := r.Header.Get("ChatGPT-Account-ID"); got != "account-id" {
				t.Errorf("%s ChatGPT-Account-ID = %q", r.URL.Path, got)
			}
		}

		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, `<!doctype html><title>ChatGPT</title>`)
		case "/backend-api/f/conversation/prepare":
			if got := r.Header.Get("OpenAI-Sentinel-Chat-Requirements-Token"); got != "final-requirements-token" {
				t.Errorf("prepare requirements token = %q", got)
			}
			if got := r.Header.Get("OpenAI-Sentinel-Proof-Token"); got == "" || got != v2ProofToken {
				t.Errorf("prepare proof token = %q", got)
			}
			var body map[string]any
			if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
				t.Errorf("decode prepare body: %v", errDecode)
			}
			if body["action"] != "next" {
				t.Errorf("prepare action = %v", body["action"])
			}
			if body["model"] != "internal-image-model" {
				t.Errorf("prepare model = %v", body["model"])
			}
			if body["client_prepare_state"] != "success" {
				t.Errorf("prepare client_prepare_state = %v", body["client_prepare_state"])
			}
			partialQuery, _ := body["partial_query"].(map[string]any)
			if fmt.Sprint(partialQuery["content"]) == "" || !strings.Contains(fmt.Sprint(partialQuery["content"]), "draw a blue sphere") {
				t.Errorf("prepare partial_query = %#v", partialQuery)
			}
			if !strings.Contains(fmt.Sprint(body["system_hints"]), "picture_v2") {
				t.Errorf("prepare system_hints = %#v", body["system_hints"])
			}
			parentMessageID, _ := body["parent_message_id"].(string)
			if parentMessageID == "" {
				t.Error("prepare parent_message_id is empty")
			}
			mu.Lock()
			prepareParentMessageID = parentMessageID
			mu.Unlock()
			_, _ = io.WriteString(w, `{"conduit_token":"prepared-conduit"}`)
		case "/backend-api/sentinel/chat-requirements/prepare":
			var body map[string]any
			if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
				t.Errorf("decode Sentinel prepare body: %v", errDecode)
			}
			token := fmt.Sprint(body["p"])
			if !strings.HasPrefix(token, requirementsTokenPrefix) {
				t.Errorf("Sentinel prepare p = %q", token)
			} else {
				encoded := strings.TrimPrefix(token, requirementsTokenPrefix)
				decoded, errDecode := base64.StdEncoding.DecodeString(encoded)
				if errDecode != nil {
					t.Errorf("decode Sentinel prepare p: %v", errDecode)
				} else {
					var vector []any
					if errJSON := json.Unmarshal(decoded, &vector); errJSON != nil {
						t.Errorf("decode Sentinel prepare vector: %v", errJSON)
					} else if len(vector) != 18 || vector[4] != config.DefaultWebImageUserAgent {
						t.Errorf("Sentinel prepare vector = %#v", vector)
					}
				}
			}
			_, _ = io.WriteString(w, `{"prepare_token":"prepare-token","proofofwork":{"required":true,"seed":"seed","difficulty":"ffff"},"turnstile":{"required":false}}`)
		case "/backend-api/sentinel/chat-requirements/finalize":
			var body map[string]any
			if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
				t.Errorf("decode finalize body: %v", errDecode)
			}
			v2ProofToken = fmt.Sprint(body["proofofwork"])
			if body["prepare_token"] != "prepare-token" || !strings.HasPrefix(v2ProofToken, proofTokenPrefix) || strings.HasSuffix(v2ProofToken, "~S") {
				t.Errorf("finalize body = %#v", body)
			}
			encodedProof := strings.TrimPrefix(v2ProofToken, proofTokenPrefix)
			decodedProof, errProofDecode := base64.StdEncoding.DecodeString(encodedProof)
			if errProofDecode != nil {
				t.Errorf("decode V2 proof: %v", errProofDecode)
			} else {
				var proofVector []any
				if errProofJSON := json.Unmarshal(decodedProof, &proofVector); errProofJSON != nil || len(proofVector) != 13 || proofVector[4] != config.DefaultWebImageUserAgent {
					t.Errorf("V2 proof vector = %#v, error = %v", proofVector, errProofJSON)
				}
			}
			if _, hasLegacyProof := body["proof_token"]; hasLegacyProof {
				t.Errorf("finalize legacy proof_token = %v", body["proof_token"])
			}
			_, _ = io.WriteString(w, `{"token":"final-requirements-token"}`)
		case "/backend-api/f/conversation":
			if got := r.Header.Get("X-Conduit-Token"); got != "prepared-conduit" {
				t.Errorf("conversation X-Conduit-Token = %q", got)
			}
			if got := r.Header.Get("OpenAI-Sentinel-Chat-Requirements-Token"); got != "final-requirements-token" {
				t.Errorf("conversation requirements token = %q", got)
			}
			if got := r.Header.Get("OpenAI-Sentinel-Chat-Requirements-Prepare-Token"); got != "" {
				t.Errorf("conversation legacy prepare token = %q", got)
			}
			if got := r.Header.Get("OpenAI-Sentinel-Proof-Token"); got == "" || got != v2ProofToken {
				t.Errorf("conversation proof token = %q", got)
			}
			var body map[string]any
			if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
				t.Errorf("decode conversation body: %v", errDecode)
			}
			if body["model"] != "internal-image-model" {
				t.Errorf("conversation model = %v", body["model"])
			}
			mu.Lock()
			wantParentMessageID := prepareParentMessageID
			mu.Unlock()
			if body["parent_message_id"] != wantParentMessageID {
				t.Errorf("conversation parent_message_id = %v, want %q", body["parent_message_id"], wantParentMessageID)
			}
			if body["client_prepare_state"] != "sent" {
				t.Errorf("conversation client_prepare_state = %v", body["client_prepare_state"])
			}
			if !strings.Contains(fmt.Sprint(body["system_hints"]), "picture_v2") {
				t.Errorf("conversation system_hints = %#v", body["system_hints"])
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"conversation_id\":\"conv-1\"}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case "/backend-api/conversation/conv-1":
			if got := r.Header.Get("OpenAI-Sentinel-Chat-Requirements-Token"); got != "final-requirements-token" {
				t.Errorf("poll requirements token = %q", got)
			}
			if got := r.Header.Get("OpenAI-Sentinel-Proof-Token"); got == "" || got != v2ProofToken {
				t.Errorf("poll proof token = %q", got)
			}
			_, _ = io.WriteString(w, `{"mapping":{"node":{"message":{"content":{"content_type":"image_asset_pointer","asset_pointer":"file-service://file-1"}}}}}`)
		case "/backend-api/files/file-1/download":
			_, _ = fmt.Fprintf(w, `{"download_url":%q}`, server.URL+"/image.png?sig=redacted")
		case "/image.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(imageBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	cfg.WebImageBaseModel = "internal-image-model"
	cfg.WebImagePollInterval = "1ms"
	executor := NewExecutor(cfg, WithBaseURL(server.URL), WithBrowserVectorFactory(func(*Session) BrowserVector {
		return fixedBrowserVector()
	}))
	defer executor.Close()
	credentials := Credentials{
		AccessToken: "access-token",
		AccountID:   "account-id",
		AuthID:      "auth-id",
	}
	session, errSession := executor.sessions.Get(credentials)
	if errSession != nil {
		t.Fatalf("session error = %v", errSession)
	}
	session.createdAt = time.Now().Add(-10 * time.Minute)

	results, meta, errGenerate := executor.Generate(context.Background(), credentials, "draw a blue sphere")
	if errGenerate != nil {
		t.Fatalf("Generate() error = %v", errGenerate)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if got, errDecode := base64.StdEncoding.DecodeString(results[0].Base64Data); errDecode != nil || string(got) != string(imageBytes) {
		t.Fatalf("decoded image = %q, error = %v", got, errDecode)
	}
	if results[0].OutputFormat != "png" {
		t.Fatalf("OutputFormat = %q, want png", results[0].OutputFormat)
	}
	if meta == nil || meta.CreatedAt == 0 {
		t.Fatalf("meta = %+v", meta)
	}

	wantPaths := []string{
		"GET /",
		"POST /backend-api/sentinel/chat-requirements/prepare",
		"POST /backend-api/sentinel/chat-requirements/finalize",
		"POST /backend-api/f/conversation/prepare",
		"POST /backend-api/f/conversation",
		"GET /backend-api/conversation/conv-1",
		"GET /backend-api/files/file-1/download",
		"GET /image.png",
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(paths, "\n") != strings.Join(wantPaths, "\n") {
		t.Fatalf("paths =\n%s\nwant:\n%s", strings.Join(paths, "\n"), strings.Join(wantPaths, "\n"))
	}
}

func TestGenerateRequestUploadsReferenceImageAndSendsMultimodalContent(t *testing.T) {
	inputBytes := webImageTestPNG(t, 4, 6)
	outputBytes := []byte("edited-image")
	paths := make([]string, 0, 12)

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, `<!doctype html>`)
		case "/backend-api/files":
			var body map[string]any
			if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
				t.Errorf("decode file create body: %v", errDecode)
			}
			if body["file_name"] != "reference.png" || body["file_size"] != float64(len(inputBytes)) || body["use_case"] != "multimodal" || body["mime_type"] != "image/png" {
				t.Errorf("file create body = %#v", body)
			}
			_, _ = fmt.Fprintf(w, `{"status":"success","file_id":"file_reference","upload_url":%q}`, server.URL+"/upload/file_reference")
		case "/upload/file_reference":
			if r.Method != http.MethodPut {
				t.Errorf("upload method = %s", r.Method)
			}
			if got := r.Header.Get("X-Ms-Blob-Type"); got != "BlockBlob" {
				t.Errorf("X-Ms-Blob-Type = %q", got)
			}
			got, errRead := io.ReadAll(r.Body)
			if errRead != nil || !bytes.Equal(got, inputBytes) {
				t.Errorf("upload bytes = %d, error = %v", len(got), errRead)
			}
			w.WriteHeader(http.StatusCreated)
		case "/backend-api/files/process_upload_stream":
			var body map[string]any
			if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
				t.Errorf("decode process body: %v", errDecode)
			}
			if body["file_id"] != "file_reference" || body["use_case"] != "multimodal" || body["index_for_retrieval"] != false {
				t.Errorf("process body = %#v", body)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: file-processing\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"file.processing.file_ready\",\"file_id\":\"file_reference\"}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"file.processing.completed\"}\n\n")
		case "/backend-api/sentinel/chat-requirements/prepare":
			_, _ = io.WriteString(w, `{"prepare_token":"prepare","proofofwork":{"required":false},"turnstile":{"required":false}}`)
		case "/backend-api/sentinel/chat-requirements/finalize":
			_, _ = io.WriteString(w, `{"token":"requirements"}`)
		case "/backend-api/f/conversation/prepare":
			_, _ = io.WriteString(w, `{"conduit_token":"conduit"}`)
		case "/backend-api/f/conversation":
			var body map[string]any
			if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
				t.Errorf("decode conversation body: %v", errDecode)
			}
			messages, _ := body["messages"].([]any)
			if len(messages) != 1 {
				t.Errorf("messages = %#v", messages)
			} else {
				message, _ := messages[0].(map[string]any)
				content, _ := message["content"].(map[string]any)
				parts, _ := content["parts"].([]any)
				if content["content_type"] != "multimodal_text" || len(parts) != 2 {
					t.Errorf("content = %#v", content)
				} else {
					pointer, _ := parts[0].(map[string]any)
					if pointer["content_type"] != "image_asset_pointer" || pointer["asset_pointer"] != "sediment://file_reference" || pointer["width"] != float64(4) || pointer["height"] != float64(6) {
						t.Errorf("pointer = %#v", pointer)
					}
					if parts[1] != "make it watercolor" {
						t.Errorf("prompt part = %#v", parts[1])
					}
				}
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"conversation_id\":\"edit-conversation\"}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case "/backend-api/conversation/edit-conversation":
			_, _ = io.WriteString(w, `{"parts":["sediment://file_reference","file-service://file-output"]}`)
		case "/backend-api/files/file-output/download":
			_, _ = fmt.Fprintf(w, `{"download_url":%q}`, server.URL+"/edited.png")
		case "/edited.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(outputBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	cfg.WebImageBaseModel = "internal-image-model"
	cfg.WebImagePollInterval = "1ms"
	executor := NewExecutor(cfg, WithBaseURL(server.URL), WithBrowserVectorFactory(func(*Session) BrowserVector { return fixedBrowserVector() }))
	defer executor.Close()

	results, _, errGenerate := executor.GenerateRequest(context.Background(), Credentials{AccessToken: "token", AuthID: "auth"}, Request{
		Prompt: "make it watercolor",
		Images: []InputImage{{Filename: "reference.png", MIMEType: "image/png", Data: inputBytes, Width: 4, Height: 6}},
	})
	if errGenerate != nil {
		t.Fatalf("GenerateRequest() error = %v", errGenerate)
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(results[0].Base64Data)
	if errDecode != nil || !bytes.Equal(decoded, outputBytes) {
		t.Fatalf("decoded output = %q, error = %v", decoded, errDecode)
	}

	wantPaths := []string{
		"GET /",
		"POST /backend-api/files",
		"PUT /upload/file_reference",
		"POST /backend-api/files/process_upload_stream",
		"POST /backend-api/sentinel/chat-requirements/prepare",
		"POST /backend-api/sentinel/chat-requirements/finalize",
		"POST /backend-api/f/conversation/prepare",
		"POST /backend-api/f/conversation",
		"GET /backend-api/conversation/edit-conversation",
		"GET /backend-api/files/file-output/download",
		"GET /edited.png",
	}
	if strings.Join(paths, "\n") != strings.Join(wantPaths, "\n") {
		t.Fatalf("paths =\n%s\nwant:\n%s", strings.Join(paths, "\n"), strings.Join(wantPaths, "\n"))
	}
}

func webImageTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{G: 255, A: 255})
	var buffer bytes.Buffer
	if errEncode := png.Encode(&buffer, img); errEncode != nil {
		t.Fatalf("png.Encode() error = %v", errEncode)
	}
	return buffer.Bytes()
}

func TestValidateUploadURLAllowsAzureBlobAndRejectsUnrelatedHost(t *testing.T) {
	executor := NewExecutor(&config.Config{})
	defer executor.Close()
	allowed, _ := url.Parse("https://account.blob.core.windows.net/container/image.png?sig=redacted")
	if errValidate := executor.validateUploadURL(allowed); errValidate != nil {
		t.Fatalf("validateUploadURL(azure) error = %v", errValidate)
	}
	rejected, _ := url.Parse("https://example.com/image.png")
	if errValidate := executor.validateUploadURL(rejected); errValidate == nil {
		t.Fatal("validateUploadURL(unrelated) error = nil")
	}
}

func TestParseUploadProcessingStreamChecksAllStatusFieldsDeterministically(t *testing.T) {
	readyOnly := "data: {\"type\":\"file.processing.file_ready\",\"status\":\"processing\"}\n\n"
	if errParse := parseUploadProcessingStream(strings.NewReader(readyOnly)); errParse != nil {
		t.Fatalf("ready-only stream error = %v", errParse)
	}

	failed := "data: {\"type\":\"file.processing.file_ready\",\"status\":\"failed\"}\n\n"
	errParse := parseUploadProcessingStream(strings.NewReader(failed))
	var statusError *StatusError
	if !errorsAs(errParse, &statusError) || statusError.Kind != ErrorKindUpstream {
		t.Fatalf("failed stream error = %#v", errParse)
	}
}

func TestPutInputImageDoesNotClassifySignedUploadStatusAsAccountFailure(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"signed upload failed"}`)
			}))
			defer server.Close()

			executor := NewExecutor(&config.Config{}, WithBaseURL(server.URL))
			defer executor.Close()
			session, errSession := executor.sessions.Get(Credentials{AuthID: "auth"})
			if errSession != nil {
				t.Fatalf("session error = %v", errSession)
			}
			errUpload := executor.putInputImage(context.Background(), session, server.URL, "image/png", []byte("image"))
			var statusError *StatusError
			if !errorsAs(errUpload, &statusError) {
				t.Fatalf("upload error = %#v", errUpload)
			}
			if statusError.StatusCode() != http.StatusBadGateway || statusError.Kind != ErrorKindUpstream {
				t.Fatalf("status error = %+v", statusError)
			}
			if !statusError.RequestScoped() {
				t.Fatalf("signed upload error is not request-scoped: %+v", statusError)
			}
		})
	}
}

func TestValidateInputImagesEnforcesCountAndAggregateByteLimits(t *testing.T) {
	cfg := &config.Config{}
	cfg.WebImageMaxBytes = 4
	executor := NewExecutor(cfg)
	defer executor.Close()

	errBytes := executor.validateInputImages([]InputImage{
		{Data: []byte("123"), Width: 1, Height: 1},
		{Data: []byte("45"), Width: 1, Height: 1},
	})
	var statusError *StatusError
	if !errorsAs(errBytes, &statusError) || statusError.Kind != ErrorKindOversize {
		t.Fatalf("byte limit error = %#v", errBytes)
	}

	images := make([]InputImage, config.WebImageMaxInputImages+1)
	for index := range images {
		images[index] = InputImage{Data: []byte("1"), Width: 1, Height: 1}
	}
	errCount := executor.validateInputImages(images)
	statusError = nil
	if !errorsAs(errCount, &statusError) || statusError.Kind != ErrorKindOversize {
		t.Fatalf("count limit error = %#v", errCount)
	}
}

func TestMergeGenerationStateExcludesUploadedReferencePointers(t *testing.T) {
	state := generationState{uploadedImages: []uploadedImage{{FileID: "file_reference", AssetPointer: "sediment://file_reference"}}}
	mergeGenerationState(&state, map[string]any{"parts": []any{"sediment://file_reference", "file_reference", "file-service://file-output"}})
	if len(state.assetRefs) != 1 || state.assetRefs[0] != "file-service://file-output" {
		t.Fatalf("assetRefs = %#v", state.assetRefs)
	}
}

func TestPrepareRequirementsFallsBackToLegacySentinel(t *testing.T) {
	paths := make([]string, 0, 3)
	prepareP := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/backend-api/sentinel/chat-requirements/prepare":
			var body map[string]any
			if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
				t.Errorf("decode Sentinel prepare body: %v", errDecode)
			}
			prepareP = fmt.Sprint(body["p"])
			_, _ = io.WriteString(w, `{"prepare_token":"prepare","proofofwork":{"required":true,"seed":"v2-seed","difficulty":"ffff"},"turnstile":{"required":true}}`)
		case "/backend-api/sentinel/chat-requirements":
			var body map[string]any
			if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
				t.Errorf("decode legacy Sentinel body: %v", errDecode)
			}
			if token := fmt.Sprint(body["p"]); !strings.HasPrefix(token, requirementsTokenPrefix) {
				t.Errorf("legacy Sentinel p = %q", token)
			} else if token != prepareP {
				t.Errorf("legacy Sentinel p differs from prepare p")
			}
			_, _ = io.WriteString(w, `{"token":"legacy-requirements-token","arkose":{"required":false},"turnstile":{"required":true},"proofofwork":{"required":true,"seed":"legacy-seed","difficulty":"ffff"}}`)
		case "/backend-api/f/conversation":
			if got := r.Header.Get("OpenAI-Sentinel-Chat-Requirements-Token"); got != "legacy-requirements-token" {
				t.Errorf("conversation requirements token = %q", got)
			}
			proof := r.Header.Get("OpenAI-Sentinel-Proof-Token")
			if !strings.HasPrefix(proof, proofTokenPrefix) || strings.HasSuffix(proof, "~S") {
				t.Errorf("conversation legacy proof token = %q", proof)
			}
			if got := r.Header.Get("OpenAI-Sentinel-Chat-Requirements-Prepare-Token"); got != "" {
				t.Errorf("conversation stale prepare token = %q", got)
			}
			if got := r.Header.Get("OpenAI-Sentinel-Turnstile-Token"); got != "" {
				t.Errorf("conversation stale turnstile token = %q", got)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"conversation_id\":\"legacy-conversation\"}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	cfg.WebImageBaseModel = "internal-image-model"
	executor := NewExecutor(cfg, WithBaseURL(server.URL), WithBrowserVectorFactory(func(*Session) BrowserVector { return fixedBrowserVector() }))
	defer executor.Close()
	credentials := Credentials{AccessToken: "token", AuthID: "auth"}
	session, errSession := executor.sessions.Get(credentials)
	if errSession != nil {
		t.Fatalf("session error = %v", errSession)
	}
	state := generationState{prompt: "draw", turnTraceID: newTurnTraceID(), clientPrepareState: "success"}
	deadline := time.Now().Add(time.Minute)

	if errRequirements := executor.prepareRequirements(context.Background(), session, credentials, &state, deadline); errRequirements != nil {
		t.Fatalf("prepareRequirements() error = %v", errRequirements)
	}
	if errConversation := executor.startConversation(context.Background(), session, credentials, &state, deadline); errConversation != nil {
		t.Fatalf("startConversation() error = %v", errConversation)
	}
	if state.conversationID != "legacy-conversation" {
		t.Fatalf("conversation ID = %q", state.conversationID)
	}
	wantPaths := []string{
		"/backend-api/sentinel/chat-requirements/prepare",
		"/backend-api/sentinel/chat-requirements",
		"/backend-api/f/conversation",
	}
	if strings.Join(paths, "\n") != strings.Join(wantPaths, "\n") {
		t.Fatalf("paths = %v, want %v", paths, wantPaths)
	}
}

func TestPrepareRequirementsPreservesChallengeWhenLegacyUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/sentinel/chat-requirements/prepare":
			_, _ = io.WriteString(w, `{"prepare_token":"prepare","proofofwork":{"required":false},"turnstile":{"required":true}}`)
		case "/backend-api/sentinel/chat-requirements":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	executor := NewExecutor(cfg, WithBaseURL(server.URL), WithBrowserVectorFactory(func(*Session) BrowserVector { return fixedBrowserVector() }))
	defer executor.Close()
	credentials := Credentials{AccessToken: "token", AuthID: "auth"}
	session, errSession := executor.sessions.Get(credentials)
	if errSession != nil {
		t.Fatalf("session error = %v", errSession)
	}
	errRequirements := executor.prepareRequirements(context.Background(), session, credentials, &generationState{}, time.Now().Add(time.Minute))
	var statusError *StatusError
	if !errorsAs(errRequirements, &statusError) || statusError.StatusCode() != http.StatusServiceUnavailable || statusError.Kind != ErrorKindChallenge || statusError.Stage != "Sentinel prepare" {
		t.Fatalf("prepareRequirements() error = %#v", errRequirements)
	}
}

func TestGenerateRejectsLegacyArkoseRequirement(t *testing.T) {
	legacyCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, `<!doctype html>`)
		case "/backend-api/sentinel/chat-requirements/prepare":
			_, _ = io.WriteString(w, `{"prepare_token":"prepare","proofofwork":{"required":false},"turnstile":{"required":true}}`)
		case "/backend-api/sentinel/chat-requirements":
			legacyCalled = true
			_, _ = io.WriteString(w, `{"token":"legacy","arkose":{"required":true},"proofofwork":{"required":false}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	cfg.WebImageBaseModel = "internal-image-model"
	executor := NewExecutor(cfg, WithBaseURL(server.URL), WithBrowserVectorFactory(func(*Session) BrowserVector { return fixedBrowserVector() }))
	defer executor.Close()

	_, _, errGenerate := executor.Generate(context.Background(), Credentials{AccessToken: "token", AuthID: "auth"}, "draw")
	var statusError *StatusError
	if !legacyCalled || !errorsAs(errGenerate, &statusError) || statusError.StatusCode() != http.StatusServiceUnavailable || statusError.Kind != ErrorKindChallenge {
		t.Fatalf("Generate() error = %#v", errGenerate)
	}
}

func TestGenerateRejectsMissingFinalRequirementsToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, `<!doctype html>`)
		case "/backend-api/sentinel/chat-requirements/prepare":
			_, _ = io.WriteString(w, `{"prepare_token":"prepare","proofofwork":{"required":true,"seed":"seed","difficulty":"ffff"},"turnstile":{"required":false}}`)
		case "/backend-api/sentinel/chat-requirements/finalize":
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	cfg.WebImageBaseModel = "internal-image-model"
	executor := NewExecutor(cfg, WithBaseURL(server.URL), WithBrowserVectorFactory(func(*Session) BrowserVector { return fixedBrowserVector() }))
	defer executor.Close()

	_, _, errGenerate := executor.Generate(context.Background(), Credentials{AccessToken: "token", AuthID: "auth"}, "draw")
	var statusError *StatusError
	if !errorsAs(errGenerate, &statusError) || statusError.Stage != "Sentinel finalize" || statusError.Kind != ErrorKindProtocol {
		t.Fatalf("Generate() error = %#v", errGenerate)
	}
}

func TestDownloadRejectsOversizeImage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = io.WriteString(w, "too-large")
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	cfg.WebImageMaxBytes = 3
	executor := NewExecutor(cfg, WithBaseURL(server.URL))
	defer executor.Close()
	session, errSession := executor.sessions.Get(Credentials{AuthID: "auth"})
	if errSession != nil {
		t.Fatalf("session error = %v", errSession)
	}

	_, _, errDownload := executor.downloadImage(context.Background(), session, server.URL)
	var statusError *StatusError
	if !errorsAs(errDownload, &statusError) || statusError.Kind != ErrorKindOversize {
		t.Fatalf("downloadImage() error = %#v", errDownload)
	}
}

func TestFindAssetRefExtractsEmbeddedPointer(t *testing.T) {
	payload := map[string]any{
		"message": map[string]any{
			"content": map[string]any{
				"parts": []any{"generated image: file-service://file_embedded-1"},
			},
		},
	}
	if got := findAssetRef(payload); got != "file-service://file_embedded-1" {
		t.Fatalf("findAssetRef() = %q", got)
	}
}

func TestFindAssetRefsPreservesMultiplePointers(t *testing.T) {
	payload := map[string]any{
		"parts": []any{
			"sediment://first",
			"file-service://second",
			"sediment://first",
		},
	}
	want := []string{"sediment://first", "file-service://second"}
	if got := findAssetRefs(payload); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("findAssetRefs() = %#v, want %#v", got, want)
	}
}

func TestPollConversationRetriesRateLimit(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"slow down"}`)
			return
		}
		_, _ = io.WriteString(w, `{"message":{"content":{"parts":["file-service://file-rate-limit"]}}}`)
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	cfg.WebImagePollInterval = "1ms"
	executor := NewExecutor(cfg, WithBaseURL(server.URL))
	defer executor.Close()
	credentials := Credentials{AccessToken: "token", AuthID: "auth"}
	session, errSession := executor.sessions.Get(credentials)
	if errSession != nil {
		t.Fatalf("session error = %v", errSession)
	}
	state := generationState{conversationID: "conversation", chatRequirementsToken: "requirements"}
	if errPoll := executor.pollConversation(context.Background(), session, credentials, &state, time.Now().Add(time.Minute)); errPoll != nil {
		t.Fatalf("pollConversation() error = %v", errPoll)
	}
	if attempts != 2 || len(state.assetRefs) != 1 || state.assetRefs[0] != "file-service://file-rate-limit" {
		t.Fatalf("attempts = %d, assetRefs = %#v", attempts, state.assetRefs)
	}
}

func TestPollConversationWaitsForSedimentCompletion(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		_, _ = io.WriteString(w, `{"status":"finished_successfully","parts":["sediment://initial","sediment://final"]}`)
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	cfg.WebImagePollInterval = "1ms"
	executor := NewExecutor(cfg, WithBaseURL(server.URL))
	defer executor.Close()
	credentials := Credentials{AccessToken: "token", AuthID: "auth"}
	session, errSession := executor.sessions.Get(credentials)
	if errSession != nil {
		t.Fatalf("session error = %v", errSession)
	}
	state := generationState{conversationID: "conversation", assetRefs: []string{"sediment://initial"}}
	if errPoll := executor.pollConversation(context.Background(), session, credentials, &state, time.Now().Add(time.Minute)); errPoll != nil {
		t.Fatalf("pollConversation() error = %v", errPoll)
	}
	if attempts != 1 || len(state.assetRefs) != 2 || state.assetRefs[1] != "sediment://final" {
		t.Fatalf("attempts = %d, assetRefs = %#v", attempts, state.assetRefs)
	}
}

func TestPollConversationHonorsRetryAfter(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	cfg.WebImagePollInterval = "1ms"
	executor := NewExecutor(cfg, WithBaseURL(server.URL))
	defer executor.Close()
	credentials := Credentials{AccessToken: "token", AuthID: "auth"}
	session, errSession := executor.sessions.Get(credentials)
	if errSession != nil {
		t.Fatalf("session error = %v", errSession)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	errPoll := executor.pollConversation(ctx, session, credentials, &generationState{conversationID: "conversation"}, time.Now().Add(time.Minute))
	if !errors.Is(errPoll, context.DeadlineExceeded) || attempts != 1 {
		t.Fatalf("pollConversation() error = %v, attempts = %d", errPoll, attempts)
	}
}

func TestPollConversationClassifiesCompletedQuotaMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"finished_successfully","message":{"content":{"parts":["You've hit the Free plan limit for image generations requests."]}}}`)
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	cfg.WebImagePollInterval = "1ms"
	executor := NewExecutor(cfg, WithBaseURL(server.URL))
	defer executor.Close()
	credentials := Credentials{AccessToken: "token", AuthID: "auth"}
	session, errSession := executor.sessions.Get(credentials)
	if errSession != nil {
		t.Fatalf("session error = %v", errSession)
	}
	state := generationState{conversationID: "conversation", chatRequirementsToken: "requirements"}
	errPoll := executor.pollConversation(context.Background(), session, credentials, &state, time.Now().Add(20*time.Millisecond))
	var statusError *StatusError
	if !errorsAs(errPoll, &statusError) || statusError.Kind != ErrorKindRateLimit {
		t.Fatalf("pollConversation() error = %#v", errPoll)
	}
}

func TestDownloadRejectsPlainHTTPInProduction(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	executor := NewExecutor(cfg)
	defer executor.Close()
	session, errSession := executor.sessions.Get(Credentials{AuthID: "auth"})
	if errSession != nil {
		t.Fatalf("session error = %v", errSession)
	}

	_, _, errDownload := executor.downloadImage(context.Background(), session, "http://example.com/image.png")
	var statusError *StatusError
	if !errorsAs(errDownload, &statusError) || statusError.Kind != ErrorKindProtocol {
		t.Fatalf("downloadImage() error = %#v", errDownload)
	}
}

func TestResolveAndDownloadAuthenticatesSameOriginRelativeURL(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/files/file/download":
			_, _ = io.WriteString(w, `{"download_url":"/image.png"}`)
		case "/image.png":
			if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("ChatGPT-Account-ID") != "account" || r.Header.Get("OpenAI-Sentinel-Proof-Token") != "proof" {
				t.Errorf("same-origin headers = %#v", r.Header)
			}
			w.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(w, "image")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	executor := NewExecutor(cfg, WithBaseURL(server.URL))
	defer executor.Close()
	credentials := Credentials{AccessToken: "token", AccountID: "account", AuthID: "auth"}
	session, errSession := executor.sessions.Get(credentials)
	if errSession != nil {
		t.Fatalf("session error = %v", errSession)
	}
	state := generationState{chatRequirementsToken: "requirements", proofToken: "proof", assetRefs: []string{"file-service://file"}}
	encoded, _, errDownload := executor.resolveAndDownload(context.Background(), session, credentials, &state, time.Now().Add(time.Minute))
	if errDownload != nil {
		t.Fatalf("resolveAndDownload() error = %v", errDownload)
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(encoded)
	if errDecode != nil || string(decoded) != "image" {
		t.Fatalf("decoded image = %q, error = %v", decoded, errDecode)
	}
}

func TestDownloadRejectsEmptyImage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	executor := NewExecutor(cfg, WithBaseURL(server.URL))
	defer executor.Close()
	session, errSession := executor.sessions.Get(Credentials{AuthID: "auth"})
	if errSession != nil {
		t.Fatalf("session error = %v", errSession)
	}
	_, _, errDownload := executor.downloadImage(context.Background(), session, server.URL)
	var statusError *StatusError
	if !errorsAs(errDownload, &statusError) || statusError.Kind != ErrorKindProtocol {
		t.Fatalf("downloadImage() error = %#v", errDownload)
	}
}

func TestResolveAndDownloadRejectsEmptyDescriptorImage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	executor := NewExecutor(cfg, WithBaseURL(server.URL))
	defer executor.Close()
	credentials := Credentials{AccessToken: "token", AuthID: "auth"}
	session, errSession := executor.sessions.Get(credentials)
	if errSession != nil {
		t.Fatalf("session error = %v", errSession)
	}
	state := generationState{assetRefs: []string{"file-service://file"}}
	_, _, errDownload := executor.resolveAndDownload(context.Background(), session, credentials, &state, time.Now().Add(time.Minute))
	var statusError *StatusError
	if !errorsAs(errDownload, &statusError) || statusError.Kind != ErrorKindProtocol {
		t.Fatalf("resolveAndDownload() error = %#v", errDownload)
	}
}

func TestDownloadStripsSensitiveHeadersForExternalAsset(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	executor := NewExecutor(cfg)
	defer executor.Close()
	credentials := Credentials{AccessToken: "token", AccountID: "account", AuthID: "auth"}
	session, errSession := executor.sessions.Get(credentials)
	if errSession != nil {
		t.Fatalf("session error = %v", errSession)
	}
	session.Client.Transport = webImageRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		for _, header := range []string{"Authorization", "ChatGPT-Account-ID", "OpenAI-Sentinel-Chat-Requirements-Token", "OpenAI-Sentinel-Proof-Token"} {
			if value := request.Header.Get(header); value != "" {
				t.Errorf("external %s = %q", header, value)
			}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(strings.NewReader("image")),
			Request:    request,
		}, nil
	})
	state := generationState{chatRequirementsToken: "requirements", proofToken: "proof"}
	encoded, _, errDownload := executor.downloadImageWithAuth(context.Background(), session, credentials, &state, "https://files.oaiusercontent.com/image.png", nil)
	if errDownload != nil {
		t.Fatalf("downloadImageWithAuth() error = %v", errDownload)
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(encoded)
	if errDecode != nil || string(decoded) != "image" {
		t.Fatalf("decoded image = %q, error = %v", decoded, errDecode)
	}
}

func TestDoJSONPreservesContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	executor := NewExecutor(cfg, WithBaseURL(server.URL))
	defer executor.Close()
	session, errSession := executor.sessions.Get(Credentials{AuthID: "auth"})
	if errSession != nil {
		t.Fatalf("session error = %v", errSession)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, errRequest := executor.doJSON(ctx, session, Credentials{}, http.MethodGet, "/", nil, nil, "test")
	if !errors.Is(errRequest, context.Canceled) {
		t.Fatalf("doJSON() error = %#v", errRequest)
	}
}

func errorsAs(err error, target any) bool {
	return errors.As(err, target)
}

type webImageRoundTripFunc func(*http.Request) (*http.Response, error)

func (function webImageRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
