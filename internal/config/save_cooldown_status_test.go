package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveCooldownStatusDefaultsToTrue(t *testing.T) {
	parsed, errParse := ParseConfigBytes([]byte("host: ''\n"))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() returned error: %v", errParse)
	}
	if !parsed.SaveCooldownStatus {
		t.Fatal("ParseConfigBytes() SaveCooldownStatus = false, want true when absent")
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("host: ''\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	loaded, errLoad := LoadConfigOptional(configPath, false)
	if errLoad != nil {
		t.Fatalf("LoadConfigOptional() returned error: %v", errLoad)
	}
	if !loaded.SaveCooldownStatus {
		t.Fatal("LoadConfigOptional() SaveCooldownStatus = false, want true when absent")
	}
}

func TestSaveCooldownStatusExplicitFalseOverridesDefault(t *testing.T) {
	const contents = "save-cooldown-status: false\n"
	parsed, errParse := ParseConfigBytes([]byte(contents))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() returned error: %v", errParse)
	}
	if parsed.SaveCooldownStatus {
		t.Fatal("ParseConfigBytes() SaveCooldownStatus = true, want explicit false")
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte(contents), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	loaded, errLoad := LoadConfigOptional(configPath, false)
	if errLoad != nil {
		t.Fatalf("LoadConfigOptional() returned error: %v", errLoad)
	}
	if loaded.SaveCooldownStatus {
		t.Fatal("LoadConfigOptional() SaveCooldownStatus = true, want explicit false")
	}
}
