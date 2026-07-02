package management

import (
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestBuildModelStatesEntry_TimeAwareFiltering(t *testing.T) {
	now := time.Now()
	pastTime := now.Add(-time.Minute)
	futureTime := now.Add(90 * time.Hour)

	auth := &coreauth.Auth{
		ID:       "auth-filter",
		Provider: "test",
		ModelStates: map[string]*coreauth.ModelState{
			// Past-reset: quota exceeded but recovery time already elapsed -> EXCLUDED
			"past-reset-model": {
				Unavailable: true,
				Quota: coreauth.QuotaState{
					Exceeded:      true,
					NextRecoverAt: pastTime,
				},
				NextRetryAfter: pastTime,
			},
			// Future-reset: quota exceeded, recovery in the future -> INCLUDED
			"future-reset-model": {
				Quota: coreauth.QuotaState{
					Exceeded:      true,
					NextRecoverAt: futureTime,
					ResetAt:       futureTime,
				},
				NextRetryAfter: futureTime,
			},
			// Zero NextRecoverAt (indefinite) -> INCLUDED
			"indefinite-quota-model": {
				Quota: coreauth.QuotaState{
					Exceeded:      true,
					NextRecoverAt: time.Time{}, // zero
				},
			},
			// Clean model: no quota, not unavailable, zero retry -> EXCLUDED
			"clean-model": {},
		},
	}

	entries := buildModelStatesEntry(auth)

	// Build a lookup map for easy assertions
	byModel := make(map[string]map[string]interface{})
	for _, e := range entries {
		model, _ := e["model"].(string)
		byModel[model] = e
	}

	// past-reset-model now appears with pending_verification=true (elapsed recovery time, unverified)
	if e, found := byModel["past-reset-model"]; !found {
		t.Errorf("past-reset-model should be included with pending_verification but was absent")
	} else {
		if e["pending_verification"] != true {
			t.Errorf("past-reset-model pending_verification = %v, want true (recovery time elapsed, unverified)", e["pending_verification"])
		}
		if e["quota_exceeded"] != false {
			t.Errorf("past-reset-model quota_exceeded = %v, want false (pending verification)", e["quota_exceeded"])
		}
	}

	// clean-model must NOT appear
	if _, found := byModel["clean-model"]; found {
		t.Errorf("clean-model should be excluded but was included")
	}

	// future-reset-model must appear with quota_exceeded=true and reset_at set
	if e, found := byModel["future-reset-model"]; !found {
		t.Errorf("future-reset-model should be included but was absent")
	} else {
		if e["quota_exceeded"] != true {
			t.Errorf("future-reset-model quota_exceeded = %v, want true", e["quota_exceeded"])
		}
		if _, ok := e["reset_at"]; !ok {
			t.Errorf("future-reset-model missing reset_at field")
		}
	}

	// indefinite-quota-model must appear with quota_exceeded=true
	if e, found := byModel["indefinite-quota-model"]; !found {
		t.Errorf("indefinite-quota-model should be included but was absent")
	} else {
		if e["quota_exceeded"] != true {
			t.Errorf("indefinite-quota-model quota_exceeded = %v, want true", e["quota_exceeded"])
		}
	}
}

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
			"gemini-3-pro": {}, // no quota status, should be ignored
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

func TestBuildModelStatesEntry_PendingVerification(t *testing.T) {
	elapsed := time.Now().Add(-30 * time.Minute)
	auth := &coreauth.Auth{
		ID: "auth-pending",
		ModelStates: map[string]*coreauth.ModelState{
			"gemini-3-pro-image": {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: elapsed,
				Quota: coreauth.QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: elapsed,
					ResetAt:       elapsed,
					ReasonCode:    "QUOTA_EXHAUSTED",
					UpstreamModel: "gemini-3-pro-image",
				},
			},
		},
	}

	entries := buildModelStatesEntry(auth)
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1 (elapsed-but-unverified quota must stay visible)", len(entries))
	}
	entry := entries[0]
	if entry["pending_verification"] != true {
		t.Errorf("pending_verification = %v, want true", entry["pending_verification"])
	}
	if entry["quota_exceeded"] != false {
		t.Errorf("quota_exceeded = %v, want false (recovery unverified, not confirmed exceeded)", entry["quota_exceeded"])
	}
	if _, ok := entry["reset_at"]; !ok {
		t.Error("reset_at missing; the elapsed reset time must remain visible")
	}
}
