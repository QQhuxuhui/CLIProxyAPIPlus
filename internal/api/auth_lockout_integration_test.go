package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// doModelsRequest issues a GET /v1/models request carrying apiKey as a bearer
// token, originating from clientIP, and returns the response recorder.
func doModelsRequest(server *Server, apiKey, clientIP string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	// With no trusted proxies configured (the test default), gin derives ClientIP
	// from RemoteAddr, so this pins the request to a specific source IP.
	req.RemoteAddr = clientIP + ":5555"
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	return rr
}

// TestDataPlaneAuthLockoutBansAfterFiveFailures asserts that five failed
// data-plane authentication attempts from one IP lock that IP out: the sixth
// request is rejected with 429 even when it carries a now-valid key, while a
// clean IP with a valid key is unaffected.
//
// NOTE: this test fails if the lockout wiring is reverted to the plain
// AuthMiddleware — without the lockout the sixth (valid-key) request would
// authenticate and return 200 instead of 429.
func TestDataPlaneAuthLockoutBansAfterFiveFailures(t *testing.T) {
	server := newTestServer(t)
	defer server.authLockout.Stop()

	const badIP = "203.0.113.7"

	// Five invalid-credential requests from the same IP.
	for i := 0; i < 5; i++ {
		rr := doModelsRequest(server, "wrong-key", badIP)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status = %d, want %d (body=%s)", i+1, rr.Code, http.StatusUnauthorized, rr.Body.String())
		}
	}

	// Sixth request from the banned IP, this time with a VALID key: it must still
	// be rejected with 429 before authentication is attempted.
	rr := doModelsRequest(server, "test-key", badIP)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("6th request (valid key, banned IP): status = %d, want %d (body=%s)", rr.Code, http.StatusTooManyRequests, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header on lockout 429 response")
	}

	// A valid key from a different, clean IP must still succeed.
	cleanRR := doModelsRequest(server, "test-key", "198.51.100.20")
	if cleanRR.Code != http.StatusOK {
		t.Fatalf("clean IP with valid key: status = %d, want %d (body=%s)", cleanRR.Code, http.StatusOK, cleanRR.Body.String())
	}
}

// TestDataPlaneAuthLockoutIgnoresMissingCredentials asserts that keyless
// (missing-credential) requests do NOT feed the lockout: only an actually
// supplied wrong key is a brute-force attempt. This guards against an
// unauthenticated DoS where keyless probes from a shared egress IP (CGNAT/NAT)
// could otherwise ban all co-located clients. It fails if RecordFailure is gated
// on HTTP 401 (which NoCredentials also maps to) instead of the invalid-credential
// error code: the sixth keyless request would return 429 instead of 401.
func TestDataPlaneAuthLockoutIgnoresMissingCredentials(t *testing.T) {
	server := newTestServer(t)
	defer server.authLockout.Stop()

	const ip = "203.0.113.99"

	// Ten keyless requests from one IP — not brute-force, must not accumulate.
	for i := 0; i < 10; i++ {
		if rr := doModelsRequest(server, "", ip); rr.Code != http.StatusUnauthorized {
			t.Fatalf("keyless request %d: status = %d, want %d (body=%s)", i+1, rr.Code, http.StatusUnauthorized, rr.Body.String())
		}
	}

	// A valid key from the SAME IP must still authenticate: keyless probes did
	// not ban the shared egress IP.
	if rr := doModelsRequest(server, "test-key", ip); rr.Code != http.StatusOK {
		t.Fatalf("valid key after keyless probes: status = %d, want %d (body=%s)", rr.Code, http.StatusOK, rr.Body.String())
	}
}

// TestDataPlaneAuthLockoutSuccessResetsCounter asserts that a successful
// authentication resets the per-IP failure counter, so an IP that intersperses
// a success never reaches the ban threshold.
func TestDataPlaneAuthLockoutSuccessResetsCounter(t *testing.T) {
	server := newTestServer(t)
	defer server.authLockout.Stop()

	const ip = "203.0.113.42"

	// Four failures (one short of the ban threshold).
	for i := 0; i < 4; i++ {
		if rr := doModelsRequest(server, "wrong-key", ip); rr.Code != http.StatusUnauthorized {
			t.Fatalf("pre-reset failure %d: status = %d, want %d", i+1, rr.Code, http.StatusUnauthorized)
		}
	}

	// A success resets the counter.
	if rr := doModelsRequest(server, "test-key", ip); rr.Code != http.StatusOK {
		t.Fatalf("reset request: status = %d, want %d (body=%s)", rr.Code, http.StatusOK, rr.Body.String())
	}

	// Four more failures: without the reset this would be eight total and the IP
	// would already be banned.
	for i := 0; i < 4; i++ {
		if rr := doModelsRequest(server, "wrong-key", ip); rr.Code != http.StatusUnauthorized {
			t.Fatalf("post-reset failure %d: status = %d, want %d", i+1, rr.Code, http.StatusUnauthorized)
		}
	}

	// The IP must NOT be banned: a valid key still authenticates.
	if rr := doModelsRequest(server, "test-key", ip); rr.Code != http.StatusOK {
		t.Fatalf("post-reset valid request: status = %d, want %d (body=%s)", rr.Code, http.StatusOK, rr.Body.String())
	}
}
