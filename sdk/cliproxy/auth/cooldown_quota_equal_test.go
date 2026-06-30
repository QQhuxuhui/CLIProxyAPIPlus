package auth

import (
	"testing"
	"time"
)

// TestCooldownQuotaEqual verifies that all seven fields are compared.
func TestCooldownQuotaEqual(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	other := base.Add(time.Hour)

	full := QuotaState{
		Exceeded:      true,
		Reason:        "quota",
		BackoffLevel:  2,
		NextRecoverAt: base,
		ResetAt:       base,
		ReasonCode:    "QUOTA_EXHAUSTED",
		UpstreamModel: "gemini-pro",
	}

	// Identical – must be equal.
	if !cooldownQuotaEqual(full, full) {
		t.Fatal("identical QuotaState should be equal")
	}

	// Differs only in ResetAt – must NOT be equal.
	diffResetAt := full
	diffResetAt.ResetAt = other
	if cooldownQuotaEqual(full, diffResetAt) {
		t.Fatal("QuotaStates differing in ResetAt should not be equal")
	}

	// Differs only in ReasonCode – must NOT be equal.
	diffReasonCode := full
	diffReasonCode.ReasonCode = "DIFFERENT"
	if cooldownQuotaEqual(full, diffReasonCode) {
		t.Fatal("QuotaStates differing in ReasonCode should not be equal")
	}

	// Differs only in UpstreamModel – must NOT be equal.
	diffUpstreamModel := full
	diffUpstreamModel.UpstreamModel = "other-model"
	if cooldownQuotaEqual(full, diffUpstreamModel) {
		t.Fatal("QuotaStates differing in UpstreamModel should not be equal")
	}
}
