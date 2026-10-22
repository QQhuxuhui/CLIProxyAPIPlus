package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRequestRateCapConfigLoadAndClone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("provider-request-rate-cap:\n  codex:\n    enabled: true\n    headroom: 0.75\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.ProviderRequestRateCap["codex"]
	if !c.Enabled || c.FailFast || c.InitialPerMinute != 20 || c.MinPerMinute != 5 || c.MaxPerMinute != 60 || c.ProbeInterval != "5m" || c.Headroom != .75 {
		t.Fatalf("defaults: %+v", c)
	}
	clone := cfg.CloneForRuntime()
	delete(clone.ProviderRequestRateCap, "codex")
	if cfg.ProviderRequestRateCap["codex"] != c {
		t.Fatal("clone shares map with live config")
	}
	if err := os.WriteFile(path, []byte("provider-request-rate-cap:\n  codex:\n    enabled: false\n    fail-fast: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("accepted inconsistent fail-fast configuration")
	}
}
