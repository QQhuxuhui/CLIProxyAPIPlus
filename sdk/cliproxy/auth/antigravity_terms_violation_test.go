package auth

import (
	"context"
	"testing"
	"time"
)

const antigravityTermsViolationBody = `{"error":{"code":403,"message":"This service has been disabled in this account for violation of Terms of Service. Please submit an appeal to continue using this product.","status":"PERMISSION_DENIED"}}`

func termsViolationTestManager(provider string) (*Manager, *memoryAuthTestStore) {
	store := &memoryAuthTestStore{auths: map[string]*Auth{}}
	m := NewManager(store, nil, nil)
	m.auths["a"] = &Auth{ID: "a", Provider: provider, Status: StatusActive, Metadata: map[string]any{"access_token": "t"}}
	return m, store
}

func TestAntigravityTermsViolationDisablesAccount(t *testing.T) {
	m, store := termsViolationTestManager("antigravity")
	m.MarkResult(context.Background(), Result{AuthID: "a", Provider: "antigravity", Model: "gemini-3-pro-image",
		Error: &Error{HTTPStatus: 403, Message: antigravityTermsViolationBody}})
	a := m.auths["a"]
	if !a.Disabled || a.Status != StatusDisabled || a.Metadata["disabled"] != true {
		t.Fatalf("banned antigravity account not disabled: disabled=%v status=%v meta=%v", a.Disabled, a.Status, a.Metadata["disabled"])
	}
	// The ban covers every model, not only the one that failed.
	for _, model := range []string{"gemini-3-pro-image", "gemini-3.1-flash-image", ""} {
		if blocked, _, _ := isAuthBlockedForModel(a, model, time.Now().Add(24*time.Hour)); !blocked {
			t.Fatalf("model %q still selectable after the ban", model)
		}
	}
	if saved := store.auths["a"]; saved == nil || !saved.Disabled {
		t.Fatal("disabled state was not persisted")
	}
}

func TestTermsViolationDoesNotDisableOtherCases(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		status   int
		message  string
	}{
		{"codex with the same text", "codex", 403, antigravityTermsViolationBody},
		{"gemini-cli with the same text", "gemini-cli", 403, antigravityTermsViolationBody},
		{"antigravity other 403", "antigravity", 403, `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`},
		{"antigravity 401 with the text", "antigravity", 401, antigravityTermsViolationBody},
		{"antigravity 429", "antigravity", 429, `{"error":{"code":429,"message":"Resource exhausted"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := termsViolationTestManager(tc.provider)
			m.MarkResult(context.Background(), Result{AuthID: "a", Provider: tc.provider, Model: "gemini-3-pro-image",
				Error: &Error{HTTPStatus: tc.status, Message: tc.message}})
			a := m.auths["a"]
			if a.Disabled || a.Status == StatusDisabled || a.Metadata["disabled"] == true {
				t.Fatalf("account disabled for %s", tc.name)
			}
		})
	}
}
