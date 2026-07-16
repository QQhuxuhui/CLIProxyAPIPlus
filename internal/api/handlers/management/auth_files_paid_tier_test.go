package management

import (
	"context"
	"testing"
	"time"

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

func TestBuildAuthFileEntry_PaidTierFromPersistedDisplayPlan(t *testing.T) {
	if errConfigure := coreauth.ConfigureAntigravityPlanStore(context.Background(), nil); errConfigure != nil {
		t.Fatalf("clear plan store: %v", errConfigure)
	}
	t.Cleanup(func() {
		_ = coreauth.ConfigureAntigravityPlanStore(context.Background(), nil)
	})
	coreauth.SetAntigravityDisplayPlan("acct-persisted-tier", "persisted-pro", time.Now())

	auth := &coreauth.Auth{
		ID:       "acct-persisted-tier",
		Index:    "persisted-index",
		Provider: "antigravity",
		Attributes: map[string]string{
			"path": "x",
		},
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	entry := h.buildAuthFileEntry(auth)
	if entry["paid_tier"] != "persisted-pro" {
		t.Fatalf("paid_tier = %v, want persisted-pro", entry["paid_tier"])
	}
}

func TestBuildAuthFileEntry_LivePaidTierPrecedesPersistedDisplayPlan(t *testing.T) {
	if errConfigure := coreauth.ConfigureAntigravityPlanStore(context.Background(), nil); errConfigure != nil {
		t.Fatalf("clear plan store: %v", errConfigure)
	}
	t.Cleanup(func() {
		_ = coreauth.ConfigureAntigravityPlanStore(context.Background(), nil)
	})
	const authID = "acct-live-tier-precedence"
	coreauth.SetAntigravityCreditsHint(authID, coreauth.AntigravityCreditsHint{Known: true, PaidTierID: "live-pro"})
	coreauth.SetAntigravityDisplayPlan(authID, "persisted-pro", time.Now())

	auth := &coreauth.Auth{
		ID:       authID,
		Index:    "live-index",
		Provider: "antigravity",
		Attributes: map[string]string{
			"path": "x",
		},
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	entry := h.buildAuthFileEntry(auth)
	if entry["paid_tier"] != "live-pro" {
		t.Fatalf("paid_tier = %v, want live-pro", entry["paid_tier"])
	}
}
