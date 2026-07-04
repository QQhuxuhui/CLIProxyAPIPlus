package management

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestBuildAuthFileEntry_PaidTier(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	auth := &coreauth.Auth{
		ID:       "acct-paid",
		Index:    "1",
		Provider: "antigravity",
		Attributes: map[string]string{
			"path": "x",
		},
	}
	coreauth.SetAntigravityCreditsHint("acct-paid", coreauth.AntigravityCreditsHint{
		Known:      true,
		PaidTierID: "test-tier-id",
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	entry := h.buildAuthFileEntry(auth)
	if entry == nil {
		t.Fatal("entry nil")
	}
	if entry["paid_tier"] != "test-tier-id" {
		t.Errorf("paid_tier = %v, want test-tier-id", entry["paid_tier"])
	}
}

func TestBuildAuthFileEntry_PaidTierOmittedWhenUnknown(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	auth := &coreauth.Auth{
		ID:       "acct-no-hint",
		Index:    "1",
		Provider: "antigravity",
		Attributes: map[string]string{
			"path": "x",
		},
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	entry := h.buildAuthFileEntry(auth)
	if entry == nil {
		t.Fatal("entry nil")
	}
	if _, ok := entry["paid_tier"]; ok {
		t.Errorf("paid_tier should be omitted when no credits hint is known, got %v", entry["paid_tier"])
	}
}
