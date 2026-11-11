package executor

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestApplyCodexAuthServiceTier(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		auth     *cliproxyauth.Auth
		wantTier string
	}{
		{
			name:     "nil auth",
			body:     `{"model":"gpt-4o"}`,
			auth:     nil,
			wantTier: "",
		},
		{
			name: "nil attributes",
			body: `{"model":"gpt-4o"}`,
			auth: &cliproxyauth.Auth{
				Attributes: nil,
			},
			wantTier: "",
		},
		{
			name: "no service_tier attribute",
			body: `{"model":"gpt-4o"}`,
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{"plan_type": "plus"},
			},
			wantTier: "",
		},
		{
			name: "service_tier priority injects",
			body: `{"model":"gpt-4o"}`,
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{"service_tier": "priority"},
			},
			wantTier: "priority",
		},
		{
			name: "overrides existing service_tier",
			body: `{"model":"gpt-4o","service_tier":"auto"}`,
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{"service_tier": "priority"},
			},
			wantTier: "priority",
		},
		{
			name: "non-priority value does not inject",
			body: `{"model":"gpt-4o"}`,
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{"service_tier": "auto"},
			},
			wantTier: "",
		},
		{
			name: "preserves existing when no override",
			body: `{"model":"gpt-4o","service_tier":"auto"}`,
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{},
			},
			wantTier: "auto",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := applyCodexAuthServiceTier([]byte(tt.body), tt.auth)
			got := gjson.GetBytes(result, "service_tier").String()
			if tt.wantTier == "" {
				if gjson.GetBytes(result, "service_tier").Exists() && tt.body != `{"model":"gpt-4o","service_tier":"auto"}` {
					t.Errorf("expected no service_tier, got %q", got)
				}
			} else if got != tt.wantTier {
				t.Errorf("expected service_tier=%q, got %q", tt.wantTier, got)
			}
		})
	}
}
