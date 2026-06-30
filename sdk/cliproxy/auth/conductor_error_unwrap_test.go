package auth

import (
	"fmt"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// TestRetryAfterFromError_UnwrapsWrappedError verifies that retryAfterFromError
// resolves the retry-after duration even when the error is wrapped with %w.
func TestRetryAfterFromError_UnwrapsWrappedError(t *testing.T) {
	d := 5 * time.Second
	inner := &retryAfterStatusError{
		status:     429,
		message:    "rate limited",
		retryAfter: d,
	}

	// Bare (unwrapped) – must work both before and after fix.
	got := retryAfterFromError(inner)
	if got == nil {
		t.Fatal("retryAfterFromError(bare): expected non-nil duration, got nil")
	}
	if *got != d {
		t.Fatalf("retryAfterFromError(bare): want %v, got %v", d, *got)
	}

	// Wrapped with %w – this is the new behaviour being hardened.
	wrapped := fmt.Errorf("upstream: %w", inner)
	got = retryAfterFromError(wrapped)
	if got == nil {
		t.Fatal("retryAfterFromError(wrapped): expected non-nil duration, got nil (errors.As not used?)")
	}
	if *got != d {
		t.Fatalf("retryAfterFromError(wrapped): want %v, got %v", d, *got)
	}
}

// TestQuotaDetailFromError_UnwrapsWrappedError verifies that quotaDetailFromError
// resolves the quota detail even when the error is wrapped with %w.
func TestQuotaDetailFromError_UnwrapsWrappedError(t *testing.T) {
	reset := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)
	inner := streamQuotaErr{reset: reset}

	// Bare (unwrapped) – must work both before and after fix.
	qd, ok := quotaDetailFromError(inner)
	if !ok {
		t.Fatal("quotaDetailFromError(bare): expected ok=true, got false")
	}
	want := cliproxyexecutor.QuotaDetail{Model: "gemini-pro-agent", ResetAt: reset, ReasonCode: "QUOTA_EXHAUSTED"}
	if qd != want {
		t.Fatalf("quotaDetailFromError(bare): want %+v, got %+v", want, qd)
	}

	// Wrapped with %w – this is the new behaviour being hardened.
	wrapped := fmt.Errorf("upstream: %w", inner)
	qd, ok = quotaDetailFromError(wrapped)
	if !ok {
		t.Fatal("quotaDetailFromError(wrapped): expected ok=true, got false (errors.As not used?)")
	}
	if qd != want {
		t.Fatalf("quotaDetailFromError(wrapped): want %+v, got %+v", want, qd)
	}
}
