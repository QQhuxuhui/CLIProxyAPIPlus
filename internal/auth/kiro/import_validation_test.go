package kiro

import (
	"strings"
	"testing"
)

func TestValidateKiroJSONImportItem(t *testing.T) {
	tests := []struct {
		name            string
		item            KiroJSONImportItem
		wantType        string
		wantProvider    string
		wantErrContains string
	}{
		{
			name:         "social defaults to google",
			item:         KiroJSONImportItem{RefreshToken: "aor-token"},
			wantType:     "social",
			wantProvider: "Google",
		},
		{
			name: "idc defaults to builderid",
			item: KiroJSONImportItem{
				RefreshToken: "aor-token",
				ClientID:     "cid",
				ClientSecret: "csec",
			},
			wantType:     "idc",
			wantProvider: "BuilderId",
		},
		{
			name:         "provider is canonicalized",
			item:         KiroJSONImportItem{RefreshToken: " aor-token ", Provider: "github"},
			wantType:     "social",
			wantProvider: "GitHub",
		},
		{
			name: "partial credentials are rejected",
			item: KiroJSONImportItem{
				RefreshToken: "aor-token",
				ClientID:     "cid",
			},
			wantErrContains: "clientSecret",
		},
		{
			name: "social token cannot use idc provider",
			item: KiroJSONImportItem{
				RefreshToken: "aor-token",
				Provider:     "BuilderId",
			},
			wantErrContains: "需要 clientId 和 clientSecret",
		},
		{
			name: "idc token cannot use social provider",
			item: KiroJSONImportItem{
				RefreshToken: "aor-token",
				Provider:     "Google",
				ClientID:     "cid",
				ClientSecret: "csec",
			},
			wantErrContains: "不应包含 clientId/clientSecret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateKiroJSONImportItem(tt.item, 0)
			if tt.wantErrContains != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErrContains)
				}
				if !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErrContains, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.AccountType != tt.wantType {
				t.Fatalf("expected account type %q, got %q", tt.wantType, got.AccountType)
			}
			if got.Provider != tt.wantProvider {
				t.Fatalf("expected provider %q, got %q", tt.wantProvider, got.Provider)
			}
			if strings.TrimSpace(got.Item.RefreshToken) != got.Item.RefreshToken {
				t.Fatalf("expected refresh token to be trimmed, got %q", got.Item.RefreshToken)
			}
		})
	}
}
