package util

import "testing"

// TestMaskSensitiveHeaderValue_MasksCookies asserts that Cookie/Set-Cookie header
// values (which carry bearer-equivalent session credentials) are masked, closing
// the S04 gap where credential cookies were forwarded to the Home control plane in
// cleartext. Non-sensitive headers must still pass through unchanged.
func TestMaskSensitiveHeaderValue_MasksCookies(t *testing.T) {
	const secretCookie = "session=abcdef0123456789-SECRET"

	if got := MaskSensitiveHeaderValue("Cookie", secretCookie); got == secretCookie {
		t.Fatalf("Cookie value was not masked: %q", got)
	}
	if got := MaskSensitiveHeaderValue("Set-Cookie", secretCookie); got == secretCookie {
		t.Fatalf("Set-Cookie value was not masked: %q", got)
	}
	// Sanity: masked form is the HideAPIKey rendering, not the raw secret.
	if got, want := MaskSensitiveHeaderValue("Cookie", secretCookie), HideAPIKey(secretCookie); got != want {
		t.Fatalf("Cookie mask = %q, want %q", got, want)
	}
	// Non-sensitive headers are untouched.
	if got := MaskSensitiveHeaderValue("Content-Type", "application/json"); got != "application/json" {
		t.Fatalf("non-sensitive header was altered: %q", got)
	}
}
