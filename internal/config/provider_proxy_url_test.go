package config

import "testing"

func TestNormalizeProviderProxyURLs(t *testing.T) {
	cfg := &SDKConfig{ProviderProxyURLs: map[string]string{
		" Antigravity ": " socks5://a:1080 ",
		"codex":         "",
		"":              "http://x",
	}}
	cfg.NormalizeProviderProxyURLs()
	if len(cfg.ProviderProxyURLs) != 1 {
		t.Fatalf("expected 1 entry, got %v", cfg.ProviderProxyURLs)
	}
	if got := cfg.ProviderProxyURLs["antigravity"]; got != "socks5://a:1080" {
		t.Fatalf("unexpected normalized value: %q", got)
	}

	empty := &SDKConfig{ProviderProxyURLs: map[string]string{"codex": " "}}
	empty.NormalizeProviderProxyURLs()
	if empty.ProviderProxyURLs != nil {
		t.Fatalf("expected nil map after dropping empty entries, got %v", empty.ProviderProxyURLs)
	}

	var nilCfg *SDKConfig
	nilCfg.NormalizeProviderProxyURLs() // must not panic
}

func TestResolveProxyURLPriority(t *testing.T) {
	cfg := &SDKConfig{
		ProxyURL:          "http://global:8080",
		ProviderProxyURLs: map[string]string{"antigravity": "socks5://ag:1080", "codex": "direct"},
	}

	tests := []struct {
		name      string
		authProxy string
		provider  string
		want      string
	}{
		{"auth wins over provider and global", "http://auth:1", "antigravity", "http://auth:1"},
		{"provider wins over global", "", "antigravity", "socks5://ag:1080"},
		{"provider match is case-insensitive", "", "AntiGravity", "socks5://ag:1080"},
		{"provider direct is honored", "", "codex", "direct"},
		{"unknown provider falls back to global", "", "claude", "http://global:8080"},
		{"empty provider falls back to global", "", "", "http://global:8080"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := cfg.ResolveProxyURL(tc.authProxy, tc.provider); got != tc.want {
				t.Fatalf("ResolveProxyURL(%q,%q) = %q, want %q", tc.authProxy, tc.provider, got, tc.want)
			}
		})
	}

	var nilCfg *SDKConfig
	if got := nilCfg.ResolveProxyURL(" http://auth:1 ", "codex"); got != "http://auth:1" {
		t.Fatalf("nil cfg should still return trimmed auth proxy, got %q", got)
	}
	if got := nilCfg.ResolveProxyURL("", "codex"); got != "" {
		t.Fatalf("nil cfg with no auth proxy should return empty, got %q", got)
	}
}
