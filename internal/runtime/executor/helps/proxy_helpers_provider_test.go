package helps

import (
	"context"
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestResolveProxyURLUsesProviderProxy(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{
		ProxyURL:          "http://global:8080",
		ProviderProxyURLs: map[string]string{"antigravity": "socks5://ag:1080"},
	}}

	if got := ResolveProxyURL(cfg, &cliproxyauth.Auth{Provider: "antigravity"}); got != "socks5://ag:1080" {
		t.Fatalf("expected provider proxy, got %q", got)
	}
	if got := ResolveProxyURL(cfg, &cliproxyauth.Auth{Provider: "codex"}); got != "http://global:8080" {
		t.Fatalf("expected global proxy, got %q", got)
	}
	if got := ResolveProxyURL(cfg, &cliproxyauth.Auth{Provider: "antigravity", ProxyURL: "http://auth:1"}); got != "http://auth:1" {
		t.Fatalf("expected auth proxy, got %q", got)
	}
	if got := ResolveProxyURL(nil, &cliproxyauth.Auth{Provider: "antigravity", ProxyURL: " http://auth:1 "}); got != "http://auth:1" {
		t.Fatalf("expected trimmed auth proxy with nil cfg, got %q", got)
	}
	if got := ResolveProxyURL(cfg, nil); got != "http://global:8080" {
		t.Fatalf("expected global proxy for nil auth, got %q", got)
	}
}

func TestNewProxyAwareHTTPClientProviderProxyTransport(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{
		ProviderProxyURLs: map[string]string{"antigravity": "http://127.0.0.1:18080"},
	}}
	client := NewProxyAwareHTTPClient(context.Background(), cfg, &cliproxyauth.Auth{Provider: "antigravity"}, 0)
	if client == nil || client.Transport == nil {
		t.Fatalf("expected a proxy transport to be configured for the provider")
	}
	other := NewProxyAwareHTTPClient(context.Background(), cfg, &cliproxyauth.Auth{Provider: "codex"}, 0)
	if other == nil || other.Transport != nil {
		t.Fatalf("expected no explicit transport for a provider without proxy")
	}
}

func TestResolveProxyURLSpreadsCredentialsAcrossProviderProxies(t *testing.T) {
	proxies := []string{"http://p1:10001", "http://p2:10002", "http://p3:10003"}
	cfg := &config.Config{SDKConfig: config.SDKConfig{
		ProxyURL:          "http://global:8080",
		ProviderProxyURLs: map[string]string{"antigravity": " http://p1:10001, http://p2:10002 ,http://p3:10003,"},
	}}

	counts := map[string]int{}
	for i := 0; i < 1500; i++ {
		auth := &cliproxyauth.Auth{ID: fmt.Sprintf("anti-%d@gmail.com.json", i), Provider: "antigravity"}
		first := ResolveProxyURL(cfg, auth)
		if again := ResolveProxyURL(cfg, auth); again != first {
			t.Fatalf("credential %s moved from %q to %q", auth.ID, first, again)
		}
		counts[first]++
	}
	if len(counts) != len(proxies) {
		t.Fatalf("expected credentials on %d proxies, got %v", len(proxies), counts)
	}
	for _, p := range proxies {
		if counts[p] < 400 || counts[p] > 600 {
			t.Fatalf("uneven spread: %v", counts)
		}
	}

	if got := ResolveProxyURL(cfg, &cliproxyauth.Auth{ID: "a", Provider: "antigravity", ProxyURL: "http://auth:1"}); got != "http://auth:1" {
		t.Fatalf("expected auth proxy to win, got %q", got)
	}
	if got := ResolveProxyURL(cfg, &cliproxyauth.Auth{Provider: "antigravity"}); got != proxies[0] {
		t.Fatalf("expected first proxy without a credential ID, got %q", got)
	}
	if got := cfg.ResolveProxyURL("", "antigravity"); got != proxies[0] {
		t.Fatalf("expected first proxy for credential-less callers, got %q", got)
	}
}
