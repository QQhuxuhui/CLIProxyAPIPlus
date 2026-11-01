package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexAccountConcurrencyDefaultsAndLegacyKey(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, "provider-in-flight-limit:\n  codex: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Codex.AccountConcurrency
	if got.Enabled || got.DefaultLimit != nil || got.EffectiveDefaultLimit() != 2 {
		t.Fatalf("defaults = %+v", got)
	}
}

func TestCodexAccountConcurrencyDistinguishesMissingAndZero(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, "codex:\n  account-concurrency:\n    enabled: true\n    default-limit: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Codex.AccountConcurrency; !got.Enabled || got.EffectiveDefaultLimit() != 0 {
		t.Fatalf("zero = %+v", got)
	}
}

func TestCodexAccountConcurrencyRejectsNegativeDefault(t *testing.T) {
	if _, err := LoadConfig(writeConfig(t, "codex:\n  account-concurrency:\n    default-limit: -1\n")); err == nil {
		t.Fatal("expected an error")
	}
}

// A first save onto a file with no codex section must keep the explicit 0, even though the
// section is otherwise all zero values and the yaml merge prunes those.
func TestCodexAccountConcurrencyExplicitZeroSurvivesFirstSave(t *testing.T) {
	path := writeConfig(t, "port: 8317\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	cfg.Codex.AccountConcurrency = CodexAccountConcurrencyConfig{DefaultLimit: &zero}
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Codex.AccountConcurrency.DefaultLimit; got == nil || *got != 0 {
		data, _ := os.ReadFile(path)
		t.Fatalf("explicit zero lost:\n%s", data)
	}
}

func TestCodexAccountConcurrencyOmittedDefaultIsNotWritten(t *testing.T) {
	path := writeConfig(t, "port: 8317\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), "account-concurrency") {
		t.Fatalf("config = %s %v", data, err)
	}
}
