package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// Codex credentials serve two upstream lines with separate quotas: the OAuth
// text line and the chatgpt.com web-image line. A text quota cooldown
// (auth-level "credential_quota") must not block web-image models and vice
// versa, while credential invalidation stays shared by both lines.

// codexWebImageModelKey reports whether model is the internal selection model
// of the web-image line. Client-facing names from web-image-models are mapped
// to this one key by the images handler before a credential is selected.
func codexWebImageModelKey(model string) bool {
	return strings.EqualFold(canonicalModelKey(model), internalconfig.DefaultWebImageModel)
}

// isCodexWebImageModel reports whether model belongs to the web-image quota
// domain of a codex credential.
func isCodexWebImageModel(auth *Auth, model string) bool {
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") && codexWebImageModelKey(model)
}

// isCodexWebImageSelection reports whether a request selects a codex
// credential for a web-image model.
func isCodexWebImageSelection(providers []string, model string) bool {
	if !codexWebImageModelKey(model) {
		return false
	}
	for _, provider := range providers {
		if strings.EqualFold(strings.TrimSpace(provider), "codex") {
			return true
		}
	}
	return false
}

// codexTextQuotaActive reports whether the credential-wide quota cooldown,
// which for codex always comes from the text line, is still running.
func codexTextQuotaActive(auth *Auth, now time.Time) bool {
	return auth != nil && auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now)
}

var codexRevocationMarkers = []string{
	"token_revoked",
	"token_invalidated",
	"invalidated oauth token",
	"token has been invalidated",
}

// codexRevocationResult reports whether a result error is an explicit upstream
// token revocation. Either line reports it for the whole credential.
func codexRevocationResult(err *Error) bool {
	if err == nil || err.StatusCode() != http.StatusUnauthorized {
		return false
	}
	text := strings.ToLower(err.Code + " " + err.Message)
	for _, marker := range codexRevocationMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

type rateExemptContextKey struct{}
type resultAccessTokenKey struct{}

// withCodexWebImageRateExemption takes a web-image request out of the codex
// text RPM cap and the model-agnostic pool availability hint.
func withCodexWebImageRateExemption(ctx context.Context, providers []string, model string) context.Context {
	if !isCodexWebImageSelection(providers, model) {
		return ctx
	}
	return context.WithValue(nonNilContext(ctx), rateExemptContextKey{}, true)
}

func rateExempt(ctx context.Context) bool {
	return ctx != nil && ctx.Value(rateExemptContextKey{}) == true
}

// rateCapSkipped reports whether the RPM cap does not apply to this request.
func rateCapSkipped(ctx context.Context) bool {
	return rateBypassed(ctx) || rateExempt(ctx)
}

// withResultAccessToken records the access token a request was executed with,
// so a late revocation answer for a replaced token cannot revoke the new one.
func withResultAccessToken(ctx context.Context, auth *Auth) context.Context {
	token := authAccessToken(auth)
	if token == "" {
		return ctx
	}
	return context.WithValue(nonNilContext(ctx), resultAccessTokenKey{}, token)
}

// resultFromReplacedToken reports whether the result belongs to an access
// token that is no longer the credential's current one.
func resultFromReplacedToken(ctx context.Context, auth *Auth) bool {
	if ctx == nil {
		return false
	}
	token, _ := ctx.Value(resultAccessTokenKey{}).(string)
	return token != "" && token != authAccessToken(auth)
}
