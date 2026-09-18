package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfigOptionalAppliesWebImageDefaults guards the file-based load path
// against drifting from ParseConfigBytes: a config that only turns the feature on
// must still get the free-only policy, model alias and timeouts.
func TestLoadConfigOptionalAppliesWebImageDefaults(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("web-image-generation: true\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	cfg, errLoad := LoadConfigOptional(configPath, false)
	if errLoad != nil {
		t.Fatalf("LoadConfigOptional() error = %v", errLoad)
	}
	if !cfg.WebImageGeneration {
		t.Fatal("WebImageGeneration = false, want true")
	}
	if !cfg.WebImageFreeOnly {
		t.Fatal("WebImageFreeOnly = false, want default true")
	}
	if len(cfg.WebImageModels) != 1 || cfg.WebImageModels[0] != DefaultWebImageModel {
		t.Fatalf("WebImageModels = %v, want [%s]", cfg.WebImageModels, DefaultWebImageModel)
	}
	if cfg.WebImagePollTimeout != DefaultWebImagePollTimeout || cfg.WebImageMaxConcurrency != DefaultWebImageMaxConcurrency {
		t.Fatalf("web image timeouts/limits not defaulted: poll=%q concurrency=%d", cfg.WebImagePollTimeout, cfg.WebImageMaxConcurrency)
	}
	if cfg.UsageStatsRetentionDays != 90 {
		t.Fatalf("UsageStatsRetentionDays = %d, want 90", cfg.UsageStatsRetentionDays)
	}
}
