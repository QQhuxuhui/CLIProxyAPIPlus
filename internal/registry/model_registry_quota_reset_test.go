package registry

import (
	"testing"
	"time"
)

// TestSetModelQuotaExceeded_HonorsRealReset verifies that SetModelQuotaExceeded stores the
// absolute recovery time (resetAt) rather than the mark time, so that the model availability
// layer honours the real upstream reset instead of a fixed 5-minute window.
//
// Deterministic strategy: use resetAt values far in the past / far in the future so the
// assertions are immediately true — no sleeps required.
func TestSetModelQuotaExceeded_HonorsRealReset(t *testing.T) {
	t.Run("past_resetAt_immediately_recovered", func(t *testing.T) {
		// A resetAt in the past means recovery has already happened.
		// Under the old fixed-5-min code, SetModelQuotaExceeded(client, model) would store
		// time.Now() and the model would remain cooling for ~5 minutes — the only way to
		// get count==1 immediately was CleanupExpiredQuotas or manual override.
		// Under the new code, a past resetAt must make the model immediately available.
		r := newTestModelRegistry()
		r.RegisterClient("client-1", "gemini", []*ModelInfo{{ID: "m1"}})

		pastReset := time.Now().Add(-time.Hour)
		r.SetModelQuotaExceeded("client-1", "m1", pastReset)

		// The stored value must equal the resetAt we passed (not time.Now()).
		r.mutex.RLock()
		stored := r.models["m1"].QuotaExceededClients["client-1"]
		r.mutex.RUnlock()
		if stored == nil {
			t.Fatal("expected quota entry to be stored, got nil")
		}
		if !stored.Equal(pastReset) {
			t.Fatalf("stored recovery time = %v, want %v", *stored, pastReset)
		}

		// With recovery time in the past, GetModelCount must report the model as available.
		if count := r.GetModelCount("m1"); count != 1 {
			t.Fatalf("GetModelCount after past-resetAt = %d, want 1 (model should be immediately recovered)", count)
		}
	})

	t.Run("future_resetAt_still_cooling", func(t *testing.T) {
		// A resetAt far in the future must keep the model cooling.
		// The old fixed-5-min code would recover the model after 5 minutes; this value
		// (90 hours) is far beyond that, so the new code produces the distinguishing result.
		r := newTestModelRegistry()
		r.RegisterClient("client-1", "gemini", []*ModelInfo{{ID: "m2"}})

		futureReset := time.Now().Add(90 * time.Hour)
		r.SetModelQuotaExceeded("client-1", "m2", futureReset)

		// The stored value must equal the resetAt we passed.
		r.mutex.RLock()
		stored := r.models["m2"].QuotaExceededClients["client-1"]
		r.mutex.RUnlock()
		if stored == nil {
			t.Fatal("expected quota entry to be stored, got nil")
		}
		if !stored.Equal(futureReset) {
			t.Fatalf("stored recovery time = %v, want %v", *stored, futureReset)
		}

		// With recovery time 90h in the future, GetModelCount must report 0 (still cooling).
		if count := r.GetModelCount("m2"); count != 0 {
			t.Fatalf("GetModelCount after 90h-future-resetAt = %d, want 0 (model should still be cooling)", count)
		}
	})

	t.Run("zero_resetAt_falls_back_to_window", func(t *testing.T) {
		// A zero resetAt must fall back to now+modelQuotaExceededWindow (5 min).
		// The stored recovery time must be strictly in the future (> now).
		r := newTestModelRegistry()
		r.RegisterClient("client-1", "gemini", []*ModelInfo{{ID: "m3"}})

		before := time.Now()
		r.SetModelQuotaExceeded("client-1", "m3", time.Time{})
		after := time.Now()

		r.mutex.RLock()
		stored := r.models["m3"].QuotaExceededClients["client-1"]
		r.mutex.RUnlock()
		if stored == nil {
			t.Fatal("expected quota entry for zero resetAt, got nil")
		}
		// Stored value must be approximately now+5min.
		low := before.Add(modelQuotaExceededWindow)
		high := after.Add(modelQuotaExceededWindow)
		if stored.Before(low) || stored.After(high) {
			t.Fatalf("zero-resetAt stored value %v not in expected range [%v, %v]", *stored, low, high)
		}
		// Model must still be cooling (recovery 5 min away).
		if count := r.GetModelCount("m3"); count != 0 {
			t.Fatalf("GetModelCount after zero-resetAt = %d, want 0 (should still be cooling)", count)
		}
	})
}
