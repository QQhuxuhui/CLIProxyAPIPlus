package configaccess

import (
	"context"
	"net/http"
	"testing"

	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
)

// TestAuthenticateValidKeyViaAuthorization verifies a configured key still
// authenticates and preserves the Result shape (Principal + Metadata source)
// after the switch to a constant-time comparison.
func TestAuthenticateValidKeyViaAuthorization(t *testing.T) {
	p := newProvider("", []string{"secret-key"})
	req, err := http.NewRequest(http.MethodGet, "http://example.com/v1/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret-key")

	result, authErr := p.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("expected success, got auth error: %v", authErr)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Principal != "secret-key" {
		t.Fatalf("Principal = %q, want %q", result.Principal, "secret-key")
	}
	if result.Metadata["source"] != "authorization" {
		t.Fatalf("Metadata source = %q, want %q", result.Metadata["source"], "authorization")
	}
}

// TestAuthenticateWrongKeyReturnsInvalidCredential ensures an unknown key is
// rejected with an InvalidCredential auth error.
func TestAuthenticateWrongKeyReturnsInvalidCredential(t *testing.T) {
	p := newProvider("", []string{"secret-key"})
	req, err := http.NewRequest(http.MethodGet, "http://example.com/v1/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer wrong-key")

	result, authErr := p.Authenticate(context.Background(), req)
	if result != nil {
		t.Fatalf("expected nil result, got %+v", result)
	}
	if authErr == nil {
		t.Fatal("expected auth error, got nil")
	}
	if !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeInvalidCredential) {
		t.Fatalf("auth error code = %q, want %q", authErr.Code, sdkaccess.AuthErrorCodeInvalidCredential)
	}
}

// TestAuthenticateSkipsEmptyCandidates verifies that an empty leading candidate
// (no Authorization header) is skipped and a later, non-empty candidate carrying
// the valid key still authenticates with the correct source.
func TestAuthenticateSkipsEmptyCandidates(t *testing.T) {
	p := newProvider("", []string{"secret-key"})
	req, err := http.NewRequest(http.MethodGet, "http://example.com/v1beta/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// No Authorization header (empty candidate); key arrives via X-Goog-Api-Key.
	req.Header.Set("X-Goog-Api-Key", "secret-key")

	result, authErr := p.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("expected success, got auth error: %v", authErr)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Metadata["source"] != "x-goog-api-key" {
		t.Fatalf("Metadata source = %q, want %q", result.Metadata["source"], "x-goog-api-key")
	}
}
