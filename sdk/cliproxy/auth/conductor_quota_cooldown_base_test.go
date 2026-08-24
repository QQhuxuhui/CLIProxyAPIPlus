package auth

import (
	"testing"
	"time"
)

func TestNextQuotaCooldownDefaultBase(t *testing.T) {
	SetQuotaCooldownBaseSeconds(0)
	t.Cleanup(func() { SetQuotaCooldownBaseSeconds(0) })

	cooldown, level := nextQuotaCooldown(0, false)
	if cooldown != time.Second {
		t.Fatalf("expected legacy 1s base, got %s", cooldown)
	}
	if level != 1 {
		t.Fatalf("expected level 1, got %d", level)
	}
}

func TestNextQuotaCooldownConfiguredBase(t *testing.T) {
	SetQuotaCooldownBaseSeconds(60)
	t.Cleanup(func() { SetQuotaCooldownBaseSeconds(0) })

	cooldown, level := nextQuotaCooldown(0, false)
	if cooldown != time.Minute {
		t.Fatalf("expected 60s base, got %s", cooldown)
	}
	if level != 1 {
		t.Fatalf("expected level 1, got %d", level)
	}

	// The ladder doubles from the configured base and still honors the cap.
	cooldown, level = nextQuotaCooldown(level, false)
	if cooldown != 2*time.Minute {
		t.Fatalf("expected 2m at level 1, got %s", cooldown)
	}
	for i := 0; i < 16; i++ {
		cooldown, level = nextQuotaCooldown(level, false)
	}
	if cooldown != quotaBackoffMax {
		t.Fatalf("expected cooldown capped at %s, got %s", quotaBackoffMax, cooldown)
	}
}

func TestNextQuotaCooldownConfiguredBaseDisabled(t *testing.T) {
	SetQuotaCooldownBaseSeconds(60)
	t.Cleanup(func() { SetQuotaCooldownBaseSeconds(0) })

	cooldown, level := nextQuotaCooldown(3, true)
	if cooldown != 0 {
		t.Fatalf("expected no cooldown when cooling disabled, got %s", cooldown)
	}
	if level != 3 {
		t.Fatalf("expected level unchanged, got %d", level)
	}
}
