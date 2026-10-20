package auth

import (
	"testing"
	"time"
)

func TestProviderTransientCooldownOverride(t *testing.T) {
	defer SetProviderTransientErrorCooldownSeconds(nil)
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	codex := &Auth{Provider: "codex"}
	other := &Auth{Provider: "antigravity"}

	SetProviderTransientErrorCooldownSeconds(map[string]int{"Codex ": 5})
	if got := transientRetryAfterForAuth(codex, now, nil, false); !got.Equal(now.Add(5 * time.Second)) {
		t.Fatalf("codex override = %v, want +5s", got)
	}
	if got := transientRetryAfterForAuth(other, now, nil, false); !got.Equal(recoverableFailureRetryAfterWithHint(now, nil, false)) {
		t.Fatalf("other providers must keep the global cooldown, got %v", got)
	}
	hint := 9 * time.Second
	if got := transientRetryAfterForAuth(codex, now, &hint, false); !got.Equal(now.Add(hint)) {
		t.Fatalf("upstream retry hint must win, got %v", got)
	}
	if got := transientRetryAfterForAuth(codex, now, nil, true); !got.IsZero() {
		t.Fatalf("disableCooling must win, got %v", got)
	}
	SetProviderTransientErrorCooldownSeconds(map[string]int{"codex": -1})
	if got := transientRetryAfterForAuth(codex, now, nil, false); !got.IsZero() {
		t.Fatalf("negative override disables the cooldown, got %v", got)
	}
	SetProviderTransientErrorCooldownSeconds(map[string]int{"codex": 0})
	if got := transientRetryAfterForAuth(codex, now, nil, false); !got.Equal(recoverableFailureRetryAfterWithHint(now, nil, false)) {
		t.Fatalf("zero override falls back to the global rule, got %v", got)
	}
}
