package auth

import (
	"testing"
	"time"
)

func TestAuthUnavailableLogAllowedThrottlesPerModel(t *testing.T) {
	model := "throttle-test-model-" + t.Name()
	other := "throttle-test-other-" + t.Name()
	base := time.Unix(1_800_000_000, 0)
	m := NewManager(nil, nil, nil)

	if !m.authUnavailableLogAllowed(model, base) {
		t.Fatal("first summary for a model must be allowed")
	}
	if m.authUnavailableLogAllowed(model, base.Add(authUnavailableLogInterval/2)) {
		t.Fatal("summary inside the interval must be throttled")
	}
	if !m.authUnavailableLogAllowed(other, base.Add(time.Millisecond)) {
		t.Fatal("throttle must be per model")
	}
	if !m.authUnavailableLogAllowed(model, base.Add(authUnavailableLogInterval)) {
		t.Fatal("summary after the interval must be allowed again")
	}
	if fresh := NewManager(nil, nil, nil); !fresh.authUnavailableLogAllowed(model, base) {
		t.Fatal("throttle state must be per manager")
	}
	if m.authUnavailableLogAllowed(model+"(high)", base.Add(authUnavailableLogInterval+time.Millisecond)) {
		t.Fatal("thinking suffix variants must share the canonical model slot")
	}
}
