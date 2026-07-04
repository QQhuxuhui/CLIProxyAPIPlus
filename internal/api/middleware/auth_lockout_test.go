package middleware

import (
	"testing"
	"time"
)

// TestAuthLockoutBansAfterMaxFailures asserts that reaching maxFailures failed
// attempts from a single IP bans it and surfaces a positive retry-after.
func TestAuthLockoutBansAfterMaxFailures(t *testing.T) {
	al := NewAuthLockout()
	defer al.Stop()

	const ip = "1.2.3.4"
	for i := 0; i < al.maxFailures-1; i++ {
		al.RecordFailure(ip)
		if banned, _ := al.Banned(ip); banned {
			t.Fatalf("banned after %d failures, want ban only at %d", i+1, al.maxFailures)
		}
	}

	// The maxFailures-th failure trips the ban.
	al.RecordFailure(ip)
	banned, retryAfter := al.Banned(ip)
	if !banned {
		t.Fatalf("expected IP to be banned after %d failures", al.maxFailures)
	}
	if retryAfter <= 0 {
		t.Fatalf("expected positive retry-after, got %v", retryAfter)
	}
	if retryAfter > al.banDuration {
		t.Fatalf("retry-after %v exceeds ban duration %v", retryAfter, al.banDuration)
	}
}

// TestAuthLockoutRecordSuccessResets asserts that a successful auth clears the
// accumulated failure count so a subsequent burst does not immediately ban.
func TestAuthLockoutRecordSuccessResets(t *testing.T) {
	al := NewAuthLockout()
	defer al.Stop()

	const ip = "5.6.7.8"
	for i := 0; i < al.maxFailures-1; i++ {
		al.RecordFailure(ip)
	}
	al.RecordSuccess(ip)
	if banned, _ := al.Banned(ip); banned {
		t.Fatal("IP should not be banned after a success reset")
	}

	// After the reset it must again take maxFailures failures to ban.
	for i := 0; i < al.maxFailures-1; i++ {
		al.RecordFailure(ip)
		if banned, _ := al.Banned(ip); banned {
			t.Fatalf("banned after %d post-reset failures, want ban only at %d", i+1, al.maxFailures)
		}
	}
	al.RecordFailure(ip)
	if banned, _ := al.Banned(ip); !banned {
		t.Fatalf("expected ban after %d post-reset failures", al.maxFailures)
	}
}

// TestAuthLockoutBanExpires asserts that a ban lifts once banDuration elapses.
func TestAuthLockoutBanExpires(t *testing.T) {
	al := NewAuthLockout()
	defer al.Stop()
	al.banDuration = 20 * time.Millisecond

	const ip = "9.9.9.9"
	for i := 0; i < al.maxFailures; i++ {
		al.RecordFailure(ip)
	}
	if banned, _ := al.Banned(ip); !banned {
		t.Fatal("expected IP to be banned")
	}

	time.Sleep(35 * time.Millisecond)
	if banned, _ := al.Banned(ip); banned {
		t.Fatal("expected ban to expire after banDuration")
	}
}

// TestAuthLockoutNilSafe asserts all methods are safe on a nil receiver.
func TestAuthLockoutNilSafe(t *testing.T) {
	var al *AuthLockout
	if banned, retry := al.Banned("1.1.1.1"); banned || retry != 0 {
		t.Fatalf("nil Banned = (%v, %v), want (false, 0)", banned, retry)
	}
	al.RecordFailure("1.1.1.1")
	al.RecordSuccess("1.1.1.1")
	al.Stop()
}
