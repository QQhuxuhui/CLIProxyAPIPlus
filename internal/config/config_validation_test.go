package config

import (
	"os"
	"path/filepath"
	"testing"
)

func boolPtr(v bool) *bool {
	return &v
}

func writeConfigYAML(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}

func snapshotCacheSim() CacheSimValues {
	v := GetCacheSimulation()
	return CacheSimValues{
		Enabled:          v.Enabled,
		ReadRatioMin:     v.ReadRatioMin,
		ReadRatioMax:     v.ReadRatioMax,
		CreationRatioMin: v.CreationRatioMin,
		CreationRatioMax: v.CreationRatioMax,
		MinInputTokens:   v.MinInputTokens,
	}
}

func assertCacheSimEqual(t *testing.T, got CacheSimValues, want CacheSimValues) {
	t.Helper()

	if got != want {
		t.Fatalf("cache simulation mismatch: got=%+v want=%+v", got, want)
	}
}

func TestLoadConfigOptionalForValidation_DoesNotMutateGlobalCacheSimulation(t *testing.T) {
	UpdateCacheSimulation(CacheSimulationConfig{
		Enabled:          boolPtr(true),
		ReadRatioMin:     0.11,
		ReadRatioMax:     0.22,
		CreationRatioMin: 0.01,
		CreationRatioMax: 0.02,
		MinInputTokens:   2222,
	})
	t.Cleanup(func() {
		UpdateCacheSimulation(CacheSimulationConfig{})
	})

	before := snapshotCacheSim()

	configPath := writeConfigYAML(t, `cache-simulation:
  enabled: false
  read-ratio-min: 0.8
  read-ratio-max: 0.9
  creation-ratio-min: 0.004
  creation-ratio-max: 0.01
  min-input-tokens: 9999
`)

	if _, err := LoadConfigOptionalForValidation(configPath, false); err != nil {
		t.Fatalf("LoadConfigOptionalForValidation failed: %v", err)
	}

	after := snapshotCacheSim()
	assertCacheSimEqual(t, after, before)
}

func TestLoadConfigOptional_AppliesGlobalCacheSimulation(t *testing.T) {
	UpdateCacheSimulation(CacheSimulationConfig{})
	t.Cleanup(func() {
		UpdateCacheSimulation(CacheSimulationConfig{})
	})

	configPath := writeConfigYAML(t, `cache-simulation:
  enabled: false
  min-input-tokens: 9999
`)

	if _, err := LoadConfigOptional(configPath, false); err != nil {
		t.Fatalf("LoadConfigOptional failed: %v", err)
	}

	after := snapshotCacheSim()
	if after.Enabled {
		t.Fatalf("expected enabled=false, got true")
	}
	if after.MinInputTokens != 9999 {
		t.Fatalf("expected min-input-tokens=9999, got %d", after.MinInputTokens)
	}
}
