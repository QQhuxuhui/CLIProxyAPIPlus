package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexEgressPoolConfigSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\ncodex:\n  # keep me\n  model-level-cooling: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Codex.EgressPool = CodexEgressPoolConfig{Enable: true, Proxies: []CodexEgressProxyConfig{{URL: "http://u:p@a:1", MaxConcurrency: 30}, {URL: "http://u:p@b:1", Name: "b"}}}
	cfg.Codex.EgressPool.Normalize()
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Codex.EgressPool.Proxies = cfg.Codex.EgressPool.Proxies[1:]
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	p := got.Codex.EgressPool
	if !p.Enable || len(p.Proxies) != 1 || p.Proxies[0].URL != "http://u:p@b:1" || p.Proxies[0].Name != "b" || !got.Codex.ModelLevelCooling {
		t.Fatalf("reloaded = %+v", p)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# keep me") {
		t.Fatalf("comment lost:\n%s", raw)
	}
}
