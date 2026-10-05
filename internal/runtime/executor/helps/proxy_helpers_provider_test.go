package helps

import (
	"context"
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
