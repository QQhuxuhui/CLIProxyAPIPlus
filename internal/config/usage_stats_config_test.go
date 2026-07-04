package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfig_UsageStatsKeys(t *testing.T) {
	raw := []byte("usage-stats-enabled: true\nusage-stats-retention-days: 30\n")
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !cfg.UsageStatsEnabled {
		t.Error("usage-stats-enabled did not parse to true")
	}
	if cfg.UsageStatsRetentionDays != 30 {
		t.Errorf("usage-stats-retention-days = %d, want 30", cfg.UsageStatsRetentionDays)
	}
}
