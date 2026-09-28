package cliproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestAntigravityModelProbeUsesPerAuthUserAgent(t *testing.T) {
	resetAntigravityCapabilityCache()
	t.Cleanup(resetAntigravityCapabilityCache)

	var mu sync.Mutex
	var agents []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		agents = append(agents, r.Header.Get("User-Agent"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"webSearchModelIds":["gemini-3-flash"]}`))
	}))
	t.Cleanup(server.Close)

	svc := &Service{cfg: &config.Config{}}
	for _, auth := range []*coreauth.Auth{
		{
			ID: "metadata-ua", Provider: "antigravity",
			Attributes: map[string]string{"base_url": server.URL},
			Metadata:   map[string]any{"access_token": "fake-token-a", "user_agent": "antigravity-code/1.103.0"},
		},
		{
			ID: "attribute-ua", Provider: "antigravity",
			Attributes: map[string]string{"base_url": server.URL, "user_agent": "attribute-ua/2"},
			Metadata:   map[string]any{"access_token": "fake-token-b", "user_agent": "ignored-ua/1"},
		},
	} {
		hints := svc.fetchAntigravityModelCapabilityHintsForAuth(context.Background(), auth)
		if _, ok := hints.WebSearchModelIDs["gemini-3-flash"]; !ok {
			t.Fatalf("missing model hint for %s", auth.ID)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(agents) != 2 {
		t.Fatalf("probe count = %d, want 2 for distinct account UAs", len(agents))
	}
	for i, want := range []string{"antigravity-code/1.103.0", "attribute-ua/2"} {
		if agents[i] != want {
			t.Errorf("probe %d UA = %q, want %q", i, agents[i], want)
		}
	}
}

func TestAntigravityModelProbeRetriesAfterUserAgentChange(t *testing.T) {
	resetAntigravityCapabilityCache()
	t.Cleanup(resetAntigravityCapabilityCache)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "old-ua/1" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"webSearchModelIds":["gemini-3-flash"]}`))
	}))
	t.Cleanup(server.Close)

	svc := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID: "same-account", Provider: "antigravity",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "fake-token", "user_agent": "old-ua/1"},
	}
	if hints := svc.fetchAntigravityModelCapabilityHintsForAuth(context.Background(), auth); len(hints.WebSearchModelIDs) != 0 {
		t.Fatal("forbidden probe unexpectedly returned model hints")
	}
	auth.Metadata["user_agent"] = "new-ua/2"
	hints := svc.fetchAntigravityModelCapabilityHintsForAuth(context.Background(), auth)
	if _, ok := hints.WebSearchModelIDs["gemini-3-flash"]; !ok {
		t.Fatal("UA change did not retry probe after a 403 response")
	}
}
