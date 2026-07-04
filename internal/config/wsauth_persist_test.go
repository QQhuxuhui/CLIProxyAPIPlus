package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSaveConfigPreserveComments_PersistsExplicitWebsocketAuthFalse guards the
// S05 opt-out: because ws-auth is tri-state (an omitted key means enabled), an
// explicit `ws-auth: false` must survive a save even when the key was absent from
// the on-disk config — otherwise disabling it via the management API would silently
// revert to enabled on the next reload. Fails if isKnownDefaultValue drops the
// bool-false ws-auth key as a zero/default.
func TestSaveConfigPreserveComments_PersistsExplicitWebsocketAuthFalse(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	// config.yaml intentionally omits ws-auth.
	if err := os.WriteFile(configPath, []byte("debug: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	disabled := false
	cfg := &Config{WebsocketAuth: &disabled}
	if err := SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments: %v", err)
	}

	saved, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read back config: %v", err)
	}
	if !strings.Contains(string(saved), "ws-auth: false") {
		t.Fatalf("explicit ws-auth: false was not persisted; file:\n%s", saved)
	}

	reloaded, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if WebsocketAuthEnabled(reloaded) {
		t.Fatalf("ws-auth reverted to enabled after save+reload; opt-out did not persist")
	}
}

// TestSaveConfigPreserveComments_OmitsUnsetWebsocketAuth is the control: a nil
// ws-auth pointer (never set) stays absent from the file and reloads as enabled.
func TestSaveConfigPreserveComments_OmitsUnsetWebsocketAuth(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("debug: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg := &Config{} // WebsocketAuth nil
	if err := SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments: %v", err)
	}

	saved, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read back config: %v", err)
	}
	if strings.Contains(string(saved), "ws-auth") {
		t.Fatalf("unset ws-auth should not be written; file:\n%s", saved)
	}

	reloaded, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if !WebsocketAuthEnabled(reloaded) {
		t.Fatalf("unset ws-auth should default to enabled")
	}
}
