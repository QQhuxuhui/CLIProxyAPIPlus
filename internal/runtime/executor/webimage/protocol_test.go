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

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestGenerateRunsWebConversationProtocol(t *testing.T) {
	var mu sync.Mutex
	paths := make([]string, 0, 8)
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
			_, _ = io.WriteString(w, `{"client_prepare_state":"ready"}`)
		case "/backend-api/sentinel/chat-requirements/prepare":
			_, _ = io.WriteString(w, `{"prepare_token":"prepare-token","proofofwork":{"required":true,"seed":"seed","difficulty":"ffff"},"turnstile":{"required":false}}`)
		case "/backend-api/sentinel/chat-requirements/finalize":
			var body map[string]any
			if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
				t.Errorf("decode finalize body: %v", errDecode)
			}
			if body["prepare_token"] != "prepare-token" || !strings.HasPrefix(fmt.Sprint(body["proof_token"]), proofTokenPrefix) {
				t.Errorf("finalize body = %#v", body)
			}
			_, _ = io.WriteString(w, `{"prepare_token":"prepare-token","proof_token":"final-proof-token","turnstile_token":""}`)
		case "/backend-api/f/conversation":
			if got := r.Header.Get("OpenAI-Sentinel-Chat-Requirements-Prepare-Token"); got != "prepare-token" {
				t.Errorf("conversation prepare token = %q", got)
			}
			if got := r.Header.Get("OpenAI-Sentinel-Proof-Token"); got != "final-proof-token" {
				t.Errorf("conversation proof token = %q", got)
			}
			var body map[string]any
			if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
				t.Errorf("decode conversation body: %v", errDecode)
			}
			if body["model"] != "internal-image-model" {
				t.Errorf("conversation model = %v", body["model"])
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

	results, meta, errGenerate := executor.Generate(context.Background(), Credentials{
		AccessToken: "access-token",
		AccountID:   "account-id",
		AuthID:      "auth-id",
	}, "draw a blue sphere")
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
