package handlers

import (
	"fmt"
	"strings"
	"testing"
)

func TestSSEValidationErrorDoesNotEchoPayload(t *testing.T) {
	for _, payload := range []string{"private-system-prompt", strings.Repeat("private", 100), "broken\nsecret"} {
		err := validateSSEFrameDataJSON([]byte("data: " + payload + "\n\n"))
		if err == nil {
			t.Fatal("expected malformed JSON error")
		}
		// Only data lines contribute to the diagnostic length.
		expectedPayload, _ := sseJSONValidationDataPayload([]byte("data: " + payload + "\n\n"))
		want := fmt.Sprintf("invalid SSE data JSON (len=%d)", len(expectedPayload))
		if err.Error() != want {
			t.Fatalf("diagnostic echoed data: %q; want %q", err, want)
		}
	}
	for _, frame := range []string{"data: {}\n\n", "data: [DONE]\n\n", ": ping\n\n"} {
		if err := validateSSEFrameDataJSON([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
}
