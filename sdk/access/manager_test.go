package access

import (
	"context"
	"net/http"
	"testing"
)

// keyProvider is a stub Provider that accepts a single valid API key and
// reports an invalid-credential error for anything else.
type keyProvider struct {
	valid string
}

func (p keyProvider) Identifier() string { return "key-provider" }

func (p keyProvider) Authenticate(_ context.Context, r *http.Request) (*Result, *AuthError) {
	if r.Header.Get("X-Api-Key") == p.valid {
		return &Result{Provider: p.Identifier(), Principal: p.valid}, nil
	}
	return nil, NewInvalidCredentialError()
}

func newTestRequest(key string) *http.Request {
	req, _ := http.NewRequest(http.MethodGet, "http://example.test/v1/models", nil)
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	return req
}

// TestAuthenticateFailsClosedWithoutProviders asserts the secure default: a
// manager with zero providers and allow-anonymous disabled rejects the request
// with a missing-credentials error. This fails against the old fail-open
// behavior (return nil, nil).
func TestAuthenticateFailsClosedWithoutProviders(t *testing.T) {
	m := NewManager()

	result, authErr := m.Authenticate(context.Background(), newTestRequest(""))
	if authErr == nil {
		t.Fatalf("Authenticate() with no providers returned nil error, want fail-closed NoCredentials error")
	}
	if !IsAuthErrorCode(authErr, AuthErrorCodeNoCredentials) {
		t.Fatalf("Authenticate() error code = %q, want %q", authErr.Code, AuthErrorCodeNoCredentials)
	}
	if result != nil {
		t.Fatalf("Authenticate() result = %+v, want nil", result)
	}
}

// TestAuthenticateAllowsAnonymousWithoutProviders asserts the explicit opt-out:
// with allow-anonymous enabled and zero providers, the request is allowed
// through (nil result, nil error) preserving the legacy open-proxy behavior.
func TestAuthenticateAllowsAnonymousWithoutProviders(t *testing.T) {
	m := NewManager()
	m.SetAllowAnonymous(true)

	result, authErr := m.Authenticate(context.Background(), newTestRequest(""))
	if authErr != nil {
		t.Fatalf("Authenticate() with allow-anonymous returned error %v, want nil", authErr)
	}
	if result != nil {
		t.Fatalf("Authenticate() result = %+v, want nil", result)
	}
}

// TestAuthenticateWithProviderUnchanged asserts that with at least one provider
// configured the behavior is unchanged: a valid key authenticates and a wrong
// key produces an invalid-credential error, regardless of allow-anonymous.
func TestAuthenticateWithProviderUnchanged(t *testing.T) {
	m := NewManager()
	m.SetProviders([]Provider{keyProvider{valid: "good-key"}})

	result, authErr := m.Authenticate(context.Background(), newTestRequest("good-key"))
	if authErr != nil {
		t.Fatalf("Authenticate() with valid key returned error %v, want nil", authErr)
	}
	if result == nil || result.Principal != "good-key" {
		t.Fatalf("Authenticate() result = %+v, want principal good-key", result)
	}

	_, authErr = m.Authenticate(context.Background(), newTestRequest("bad-key"))
	if authErr == nil {
		t.Fatalf("Authenticate() with bad key returned nil error, want InvalidCredential")
	}
	if !IsAuthErrorCode(authErr, AuthErrorCodeInvalidCredential) {
		t.Fatalf("Authenticate() error code = %q, want %q", authErr.Code, AuthErrorCodeInvalidCredential)
	}
}
