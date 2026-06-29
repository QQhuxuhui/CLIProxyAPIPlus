package management

import (
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestBuildModelStatesEntry_ExposesQuota(t *testing.T) {
	reset := time.Date(2026, 7, 3, 13, 11, 31, 0, time.UTC)
	auth := &coreauth.Auth{
		ID:       "auth-1",
		Provider: "antigravity",
		ModelStates: map[string]*coreauth.ModelState{
			"gemini-pro-agent": {
				Unavailable:    true,
				NextRetryAfter: reset,
				Quota: coreauth.QuotaState{
					Exceeded:      true,
					NextRecoverAt: reset,
					ResetAt:       reset,
					ReasonCode:    "QUOTA_EXHAUSTED",
					UpstreamModel: "gemini-pro-agent",
				},
			},
			"gemini-3-pro": {}, // 无配额状态，应被忽略
		},
	}
	entries := buildModelStatesEntry(auth)
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1 (only exhausted model)", len(entries))
	}
	e := entries[0]
	if e["model"] != "gemini-pro-agent" {
		t.Errorf("model = %v", e["model"])
	}
	if e["quota_exceeded"] != true {
		t.Errorf("quota_exceeded = %v", e["quota_exceeded"])
	}
	if e["reason_code"] != "QUOTA_EXHAUSTED" {
		t.Errorf("reason_code = %v", e["reason_code"])
	}
	if got, ok := e["reset_at"].(time.Time); !ok || !got.Equal(reset) {
		t.Errorf("reset_at = %v, want %v", e["reset_at"], reset)
	}
	if e["upstream_model"] != "gemini-pro-agent" {
		t.Errorf("upstream_model = %v, want gemini-pro-agent", e["upstream_model"])
	}
	if got, ok := e["next_retry_after"].(time.Time); !ok || !got.Equal(reset) {
		t.Errorf("next_retry_after = %v, want %v", e["next_retry_after"], reset)
	}
}
