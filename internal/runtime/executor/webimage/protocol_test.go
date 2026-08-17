package webimage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
		case "/backend-api/conversation/init":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Conduit-Token", "conduit-token")
			_, _ = io.WriteString(w, `{}`)
		case "/backend-api/f/conversation/prepare":
			if got := r.Header.Get("X-Conduit-Token"); got != "conduit-token" {
				t.Errorf("prepare X-Conduit-Token = %q", got)
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
			if body["client_prepare_state"] != "none" {
				t.Errorf("prepare client_prepare_state = %v", body["client_prepare_state"])
			}
			if _, hasMessages := body["messages"]; hasMessages {
				t.Errorf("prepare messages = %v, want omitted", body["messages"])
			}
			parentMessageID, _ := body["parent_message_id"].(string)
			if parentMessageID == "" {
				t.Error("prepare parent_message_id is empty")
			}
			mu.Lock()
			prepareParentMessageID = parentMessageID
			mu.Unlock()
			_, _ = io.WriteString(w, `{"conduit_token":"prepared-conduit","nested":{"state":"queued"}}`)
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
					var vector BrowserVector
					if errJSON := json.Unmarshal(decoded, &vector); errJSON != nil {
						t.Errorf("decode Sentinel prepare vector: %v", errJSON)
					} else if len(vector) <= 9 || vector[3] != float64(1) || vector[9].(float64) > 1000 {
						t.Errorf("Sentinel prepare vector nonce/elapsed = %v/%v", vector[3], vector[9])
					}
				}
			}
			_, _ = io.WriteString(w, `{"prepare_token":"prepare-token","proofofwork":{"required":true,"seed":"seed","difficulty":"ffff"},"turnstile":{"required":false}}`)
		case "/backend-api/sentinel/chat-requirements/finalize":
			var body map[string]any
			if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
				t.Errorf("decode finalize body: %v", errDecode)
			}
			if body["prepare_token"] != "prepare-token" || !strings.HasPrefix(fmt.Sprint(body["proofofwork"]), proofTokenPrefix) {
				t.Errorf("finalize body = %#v", body)
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
			if got := r.Header.Get("OpenAI-Sentinel-Proof-Token"); got != "" {
				t.Errorf("conversation legacy proof token = %q", got)
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
			if body["client_prepare_state"] != "success" {
				t.Errorf("conversation client_prepare_state = %v", body["client_prepare_state"])
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"conversation_id\":\"conv-1\",\"message\":{\"content\":{\"content_type\":\"image_asset_pointer\",\"asset_pointer\":\"sediment://file-1\"}}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case "/backend-api/conversation/conv-1/stream_status":
			_, _ = io.WriteString(w, `{"status":"finished_successfully"}`)
		case "/backend-api/conversation/conv-1":
			_, _ = io.WriteString(w, `{"mapping":{"node":{"message":{"content":{"content_type":"image_asset_pointer","asset_pointer":"sediment://file-1"}}}}}`)
		case "/backend-api/files/download/file-1":
			if got := r.URL.Query().Get("conversation_id"); got != "conv-1" {
				t.Errorf("download conversation_id = %q", got)
			}
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
		"POST /backend-api/conversation/init",
		"POST /backend-api/f/conversation/prepare",
		"POST /backend-api/sentinel/chat-requirements/prepare",
		"POST /backend-api/sentinel/chat-requirements/finalize",
		"POST /backend-api/f/conversation",
		"GET /backend-api/conversation/conv-1/stream_status",
		"GET /backend-api/conversation/conv-1",
		"GET /backend-api/files/download/file-1",
		"GET /image.png",
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(paths, "\n") != strings.Join(wantPaths, "\n") {
		t.Fatalf("paths =\n%s\nwant:\n%s", strings.Join(paths, "\n"), strings.Join(wantPaths, "\n"))
	}
}

func TestGenerateRejectsTurnstileRequirement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/conversation/init":
			w.Header().Set("X-Conduit-Token", "conduit")
			_, _ = io.WriteString(w, `{}`)
		case "/backend-api/f/conversation/prepare":
			_, _ = io.WriteString(w, `{}`)
		case "/backend-api/sentinel/chat-requirements/prepare":
			_, _ = io.WriteString(w, `{"prepare_token":"prepare","proofofwork":{"required":false},"turnstile":{"required":true}}`)
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
	if !errorsAs(errGenerate, &statusError) || statusError.StatusCode() != http.StatusServiceUnavailable || statusError.Kind != ErrorKindChallenge {
		t.Fatalf("Generate() error = %#v", errGenerate)
	}
}

func TestGenerateRejectsMissingFinalRequirementsToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/conversation/init":
			w.Header().Set("X-Conduit-Token", "conduit")
			_, _ = io.WriteString(w, `{}`)
		case "/backend-api/f/conversation/prepare":
			_, _ = io.WriteString(w, `{}`)
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

func errorsAs(err error, target any) bool {
	return errors.As(err, target)
}
