package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCodexFastSessionIDsConfigLoadAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	load := func(content string) *Config {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	cfg := load("codex:\n  fast-session-ids:\n    enable: true\n    percent: 150\n    seed:\n      - \" a \"\n      - b\n      - a\n      - \"\"\n")
	got := cfg.Codex.FastSessionIDs
	if !got.Enable || got.Percent != 100 || !slices.Equal(got.Seed, []string{"a", "b"}) {
		t.Fatalf("fast-session-ids = %+v", got)
	}

	cfg = load("codex:\n  fast-session-ids:\n    enable: true\n    percent: 10\n    seed: [c]\n")
	got = cfg.Codex.FastSessionIDs
	if got.Percent != 10 || !slices.Equal(got.Seed, []string{"c"}) {
		t.Fatalf("reloaded fast-session-ids = %+v", got)
	}

	cfg = load("codex:\n  identity-confuse: false\n")
	if got = cfg.Codex.FastSessionIDs; got.Enable || got.Percent != 0 || len(got.Seed) != 0 {
		t.Fatalf("default fast-session-ids = %+v", got)
	}
}

func TestCodexFastSessionIDsConfigNormalizeProberAndRollback(t *testing.T) {
	cfg := CodexFastSessionIDsConfig{}
	cfg.Normalize()
	if cfg.ProbeIntervalSeconds != 0 || cfg.ProbeCandidates != 6 || cfg.ProbeBaselines != 5 || cfg.PoolSize != 12 || cfg.BaselineThresholdMs != 2500 || cfg.AdmitRatio != 0.4 || cfg.Consecutive != 2 || cfg.ProbeModel != DefaultCodexFastSessionProbeModel {
		t.Fatalf("defaults = %+v", cfg)
	}
	if r := cfg.Rollback; r.Enable || r.WindowMinutes != 10 || r.MinSamples != 30 || r.TTFTWorsePercent != 20 || r.FailureWorsePoints != 5 {
		t.Fatalf("rollback defaults = %+v", r)
	}

	cfg = CodexFastSessionIDsConfig{ProbeIntervalSeconds: 5, ProbeBaselines: 1, ProbeCandidates: 500, PoolSize: 1000, AdmitRatio: 1.5, Consecutive: 99, ProbeModel: " m ", Rollback: CodexFastSessionRollbackConfig{WindowMinutes: 120}}
	cfg.Normalize()
	if cfg.ProbeIntervalSeconds != 60 || cfg.ProbeBaselines != 3 || cfg.ProbeCandidates != 50 || cfg.PoolSize != 100 || cfg.AdmitRatio != 0.4 || cfg.Consecutive != 10 || cfg.ProbeModel != "m" || cfg.Rollback.WindowMinutes != 60 {
		t.Fatalf("bounds = %+v", cfg)
	}
	cfg = CodexFastSessionIDsConfig{ProbeIntervalSeconds: -1}
	cfg.Normalize()
	if cfg.ProbeIntervalSeconds != 0 {
		t.Fatalf("negative interval = %d", cfg.ProbeIntervalSeconds)
	}
}
