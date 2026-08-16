package helps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestChatGPTWebClientChat(t *testing.T) {
	var conversationCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/backend-api/sentinel/chat-requirements/prepare":
			writeChatGPTWebTestJSON(t, w, map[string]any{
				"prepare_token": "prepare-token",
				"turnstile":     map[string]any{"required": false},
			})
		case "/backend-api/sentinel/chat-requirements/finalize":
			writeChatGPTWebTestJSON(t, w, map[string]any{"token": "requirements-token"})
		case "/backend-api/conversation":
			conversationCalls.Add(1)
			if r.Header.Get("OpenAI-Sentinel-Chat-Requirements-Token") != "requirements-token" {
				t.Errorf("requirements header = %q", r.Header.Get("OpenAI-Sentinel-Chat-Requirements-Token"))
			}
			body, errRead := io.ReadAll(r.Body)
			if errRead != nil {
				t.Errorf("read conversation body: %v", errRead)
			}
			if !strings.Contains(string(body), "hello from test") {
				t.Errorf("conversation body does not contain prompt: %s", body)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"conversation_id\":\"conversation-text\",\"message\":{\"author\":{\"role\":\"assistant\"},\"content\":{\"parts\":[\"hello from web\"]}},\"status\":\"finished_successfully\"}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client, errClient := NewChatGPTWebClient(context.Background(), nil, chatGPTWebTestAuth(server.URL, nil))
	if errClient != nil {
		t.Fatalf("NewChatGPTWebClient() error = %v", errClient)
	}
	result, errChat := client.Chat(context.Background(), "hello from test")
	if errChat != nil {
		t.Fatalf("Chat() error = %v", errChat)
	}
	if result.Text != "hello from web" {
		t.Fatalf("Chat() text = %q, want %q", result.Text, "hello from web")
	}
	if result.ConversationID != "conversation-text" {
		t.Fatalf("Chat() conversation ID = %q", result.ConversationID)
	}
	if conversationCalls.Load() != 1 {
		t.Fatalf("conversation calls = %d, want 1", conversationCalls.Load())
	}
}

func TestChatGPTWebClientGenerateImage(t *testing.T) {
	imageBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x01, 0x02}
	var bootstrapCalls atomic.Int32
	var prepareCalls atomic.Int32
	var imageCalls atomic.Int32
	var downloadCalls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			bootstrapCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		case "/backend-api/sentinel/chat-requirements/prepare":
			writeChatGPTWebTestJSON(t, w, map[string]any{
				"prepare_token": "prepare-token",
				"turnstile":     map[string]any{"required": false},
			})
		case "/backend-api/sentinel/chat-requirements/finalize":
			writeChatGPTWebTestJSON(t, w, map[string]any{"token": "requirements-token"})
		case "/backend-api/f/conversation/prepare":
			prepareCalls.Add(1)
			writeChatGPTWebTestJSON(t, w, map[string]any{"conduit_token": "conduit-token"})
		case "/backend-api/f/conversation":
			imageCalls.Add(1)
			if r.Header.Get("X-Conduit-Token") != "conduit-token" {
				t.Errorf("conduit header = %q", r.Header.Get("X-Conduit-Token"))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"conversation_id\":\"conversation-image\",\"message\":{\"author\":{\"role\":\"assistant\"},\"content\":{\"parts\":[\"created image file-service://image-1\"]}},\"status\":\"finished_successfully\"}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case "/backend-api/files/image-1/download":
			downloadCalls.Add(1)
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(imageBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client, errClient := NewChatGPTWebClient(context.Background(), nil, chatGPTWebTestAuth(server.URL, nil))
	if errClient != nil {
		t.Fatalf("NewChatGPTWebClient() error = %v", errClient)
	}
	result, errGenerate := client.GenerateImage(context.Background(), "a white dog", "1536x1024", "high")
	if errGenerate != nil {
		t.Fatalf("GenerateImage() error = %v", errGenerate)
	}
	if string(result.Image) != string(imageBytes) {
		t.Fatalf("GenerateImage() bytes = %v, want %v", result.Image, imageBytes)
	}
	if result.MIMEType != "image/png" {
		t.Fatalf("GenerateImage() MIME type = %q", result.MIMEType)
	}
	if result.ConversationID != "conversation-image" {
		t.Fatalf("GenerateImage() conversation ID = %q", result.ConversationID)
	}
	if bootstrapCalls.Load() != 1 || prepareCalls.Load() != 1 || imageCalls.Load() != 1 || downloadCalls.Load() != 1 {
		t.Fatalf("calls bootstrap=%d prepare=%d image=%d download=%d", bootstrapCalls.Load(), prepareCalls.Load(), imageCalls.Load(), downloadCalls.Load())
	}
}

func TestChatGPTWebClientExchangesCookieForAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/session":
			if r.Header.Get("Cookie") != "session=test-cookie" {
				t.Errorf("cookie header = %q", r.Header.Get("Cookie"))
			}
			writeChatGPTWebTestJSON(t, w, map[string]any{"accessToken": "cookie-access-token"})
		case "/backend-api/sentinel/chat-requirements/prepare":
			if r.Header.Get("Authorization") != "Bearer cookie-access-token" {
				t.Errorf("authorization after exchange = %q", r.Header.Get("Authorization"))
			}
			writeChatGPTWebTestJSON(t, w, map[string]any{"prepare_token": "p", "turnstile": map[string]any{"required": false}})
		case "/backend-api/sentinel/chat-requirements/finalize":
			writeChatGPTWebTestJSON(t, w, map[string]any{"token": "r"})
		case "/backend-api/conversation":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"message\":{\"author\":{\"role\":\"assistant\"},\"content\":{\"parts\":[\"ok\"]}},\"status\":\"done\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	auth := chatGPTWebTestAuth(server.URL, map[string]any{"access_token": "", "cookie": "session=test-cookie"})
	client, errClient := NewChatGPTWebClient(context.Background(), nil, auth)
	if errClient != nil {
		t.Fatalf("NewChatGPTWebClient() error = %v", errClient)
	}
	result, errChat := client.Chat(context.Background(), "hello")
	if errChat != nil {
		t.Fatalf("Chat() error = %v", errChat)
	}
	if result.Text != "ok" {
		t.Fatalf("Chat() text = %q", result.Text)
	}
}

func chatGPTWebTestAuth(baseURL string, overrides map[string]any) *cliproxyauth.Auth {
	metadata := map[string]any{
		"type":         "chatgpt-web",
		"access_token": "test-access-token",
		"base_url":     baseURL,
	}
	for key, value := range overrides {
		metadata[key] = value
	}
	return &cliproxyauth.Auth{
		ID:       "chatgpt-web-test.json",
		Provider: "chatgpt-web",
		Metadata: metadata,
	}
}

func writeChatGPTWebTestJSON(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if errEncode := json.NewEncoder(w).Encode(payload); errEncode != nil {
		t.Errorf("encode JSON response: %v", errEncode)
	}
}
