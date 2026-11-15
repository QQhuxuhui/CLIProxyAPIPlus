package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const (
	quotaTextModel = "gpt-6-luna"
	quotaWebModel  = "gpt-image-web"
)

func newQuotaDomainManager(t *testing.T, provider string, metadata map[string]any) (*Manager, string) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	id := "quota-domain-" + provider
	m.mu.Lock()
	m.auths[id] = &Auth{ID: id, Provider: provider, Status: StatusActive, Metadata: metadata}
	m.mu.Unlock()
	return m, id
}

func quotaMark(m *Manager, ctx context.Context, provider, id, model string, err *Error, credentialScoped bool, retryAfter time.Duration) {
	result := Result{AuthID: id, Provider: provider, Model: model, Success: err == nil, Error: err, CredentialScope: credentialScoped}
	if retryAfter > 0 {
		result.RetryAfter = &retryAfter
	}
	m.MarkResult(ctx, result)
}

func quotaTextExhausted(m *Manager, provider, id string, retryAfter time.Duration) {
	quotaMark(m, context.Background(), provider, id, quotaTextModel, &Error{Message: "usage_limit_reached", HTTPStatus: http.StatusTooManyRequests}, true, retryAfter)
}

func quotaBlocked(t *testing.T, m *Manager, id, model string) bool {
	t.Helper()
	auth, ok := m.GetByID(id)
	if !ok {
		t.Fatalf("auth %s missing", id)
	}
	blocked, _, _ := isAuthBlockedForModel(auth, model, time.Now())
	return blocked
}

func quotaAuth(t *testing.T, m *Manager, id string) *Auth {
	t.Helper()
	auth, ok := m.GetByID(id)
	if !ok {
		t.Fatalf("auth %s missing", id)
	}
	return auth
}

func TestCodexTextQuotaDoesNotBlockWebImage(t *testing.T) {
	for _, withWebState := range []bool{false, true} {
		m, id := newQuotaDomainManager(t, "codex", nil)
		if withWebState {
			quotaMark(m, context.Background(), "codex", id, quotaWebModel, nil, false, 0)
		}
		quotaTextExhausted(m, "codex", id, time.Hour)
		if !quotaBlocked(t, m, id, quotaTextModel) || !quotaBlocked(t, m, id, "gpt-6-sol") {
			t.Fatalf("withWebState=%v: text models not blocked by text quota", withWebState)
		}
		if quotaBlocked(t, m, id, quotaWebModel) {
			t.Fatalf("withWebState=%v: web image blocked by text quota", withWebState)
		}
		if state := quotaAuth(t, m, id).ModelStates[quotaWebModel]; withWebState && (state == nil || state.Unavailable || state.Quota.Exceeded) {
			t.Fatalf("text quota propagated to the web-image state: %+v", state)
		}
	}
}

func TestCodexWebImageSuccessDuringTextQuotaResetsOwnState(t *testing.T) {
	m, id := newQuotaDomainManager(t, "codex", nil)
	quotaMark(m, context.Background(), "codex", id, quotaWebModel, &Error{Message: "image limit", HTTPStatus: http.StatusTooManyRequests}, false, time.Hour)
	quotaTextExhausted(m, "codex", id, time.Hour)
	if !quotaBlocked(t, m, id, quotaWebModel) {
		t.Fatal("web image limit did not cool the web-image state")
	}
	quotaMark(m, context.Background(), "codex", id, quotaWebModel, nil, false, 0)
	if quotaBlocked(t, m, id, quotaWebModel) {
		t.Fatal("web image success did not reset its own state")
	}
	auth := quotaAuth(t, m, id)
	if auth.Quota.Reason != "credential_quota" || !auth.Quota.Exceeded || !quotaBlocked(t, m, id, quotaTextModel) {
		t.Fatalf("web image success released the text quota: %+v", auth.Quota)
	}
}

func TestCodexWebImageLimitDoesNotBlockText(t *testing.T) {
	m, id := newQuotaDomainManager(t, "codex", nil)
	quotaMark(m, context.Background(), "codex", id, quotaTextModel, nil, false, 0)
	quotaMark(m, context.Background(), "codex", id, quotaWebModel, &Error{Message: "image limit", HTTPStatus: http.StatusTooManyRequests}, false, time.Hour)
	if !quotaBlocked(t, m, id, quotaWebModel) {
		t.Fatal("web image limit did not cool web image")
	}
	if quotaBlocked(t, m, id, quotaTextModel) {
		t.Fatal("web image limit blocked text")
	}
}

func TestCodexWebImageCooldownDoesNotLengthenTextCooldown(t *testing.T) {
	m, id := newQuotaDomainManager(t, "codex", nil)
	quotaMark(m, context.Background(), "codex", id, quotaWebModel, &Error{Message: "image limit", HTTPStatus: http.StatusTooManyRequests}, false, 24*time.Hour)
	quotaTextExhausted(m, "codex", id, time.Minute)
	auth := quotaAuth(t, m, id)
	if limit := time.Now().Add(2 * time.Minute); auth.Quota.NextRecoverAt.After(limit) {
		t.Fatalf("text cooldown inherited the web-image recovery time: %v", auth.Quota.NextRecoverAt)
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, quotaTextModel, time.Now().Add(2*time.Minute)); blocked {
		t.Fatal("text still blocked after its own one-minute cooldown")
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, quotaWebModel, time.Now().Add(2*time.Minute)); !blocked {
		t.Fatal("web image cooldown was shortened")
	}
}

func TestNonCodexCredentialQuotaStillBlocksEveryModel(t *testing.T) {
	m, id := newQuotaDomainManager(t, "claude", nil)
	quotaMark(m, context.Background(), "claude", id, quotaWebModel, nil, false, 0)
	quotaTextExhausted(m, "claude", id, time.Hour)
	if !quotaBlocked(t, m, id, quotaTextModel) || !quotaBlocked(t, m, id, quotaWebModel) {
		t.Fatal("non-codex credential quota no longer blocks every model")
	}
}

func TestCodexWebImageModelsFollowConfig(t *testing.T) {
	t.Cleanup(func() { setCodexWebImageModels(nil) })
	m, id := newQuotaDomainManager(t, "codex", nil)
	custom := &internalconfig.Config{}
	custom.WebImageModels = []string{"My-Image-Web"}
	m.SetConfig(custom)
	quotaTextExhausted(m, "codex", id, time.Hour)
	if quotaBlocked(t, m, id, "my-image-web") {
		t.Fatal("configured web-image model blocked by text quota")
	}
	if !quotaBlocked(t, m, id, quotaWebModel) {
		t.Fatal("default name still treated as web image after it was replaced")
	}
	m.SetConfig(&internalconfig.Config{})
	if quotaBlocked(t, m, id, quotaWebModel) {
		t.Fatal("empty config did not restore the default web-image model")
	}
	if isCodexWebImageSelection([]string{"claude"}, quotaWebModel) || !isCodexWebImageSelection([]string{"claude", "codex"}, quotaWebModel) {
		t.Fatal("request classification ignores the provider list")
	}
}

func TestCodexWebImageLeavesRateCap(t *testing.T) {
	capConfig := internalconfig.RequestRateCapConfig{Enabled: true, FailFast: true, InitialPerMinute: 1, MinPerMinute: 1, MaxPerMinute: 5}
	webCtx := withCodexWebImageRateExemption(context.Background(), []string{"codex"}, quotaWebModel)
	if !rateCapSkipped(webCtx) || rateCapSkipped(withCodexWebImageRateExemption(context.Background(), []string{"codex"}, quotaTextModel)) {
		t.Fatal("rate exemption does not follow the selection model")
	}

	t.Run("legacy", func(t *testing.T) {
		m, id := newQuotaDomainManager(t, "codex", nil)
		m.SetConfig(&internalconfig.Config{ProviderRequestRateCap: map[string]internalconfig.RequestRateCapConfig{"codex": capConfig}})
		auth := quotaAuth(t, m, id)
		m.rateCap.start(id)
		if _, err := m.beginRateAttempt(context.Background(), auth); !isPoolCapacityError(err) {
			t.Fatalf("text attempt at the limit: want 529, got %v", err)
		}
		ctx, err := m.beginRateAttempt(webCtx, auth)
		if err != nil {
			t.Fatalf("web image attempt was capped: %v", err)
		}
		if _, counted := ctx.Value(rateAttemptContextKey{}).(*rateAttemptRef); counted {
			t.Fatal("web image attempt was counted")
		}
		if count, _, _ := m.rateCap.account(id); count != 1 {
			t.Fatalf("count = %d, want 1", count)
		}
	})

	t.Run("pooled", func(t *testing.T) {
		m := newPoolStorageManager(t, 2)
		auth, err := m.Register(context.Background(), &Auth{ID: "quota-domain-pooled", Provider: "codex"})
		if err != nil {
			t.Fatal(err)
		}
		guard, err := m.lockAuth(context.Background(), auth.ID)
		if err != nil {
			t.Fatal(err)
		}
		guard.pool.rate.configure(capConfig)
		guard.pool.rate.startLocked(auth.ID, guard.pool.rate.nowFunc())
		textReady, _ := m.pooledRateAvailableLocked(guard.pool, auth, context.Background())
		webReady, _ := m.pooledRateAvailableLocked(guard.pool, auth, webCtx)
		guard.unlock()
		if textReady || !webReady {
			t.Fatalf("pooled readiness text=%v web=%v, want false/true", textReady, webReady)
		}
		if _, err = m.beginRateAttempt(context.Background(), auth); !isPoolCapacityError(err) {
			t.Fatalf("pooled text attempt at the limit: want 529, got %v", err)
		}
		if _, err = m.beginRateAttempt(webCtx, auth); err != nil {
			t.Fatalf("pooled web image attempt was capped: %v", err)
		}
	})
}

func revocationErrors() map[string]*Error {
	return map[string]*Error{
		"web token_invalidated": {Message: "web image credential was rejected (token_invalidated)", HTTPStatus: http.StatusUnauthorized},
		"web token_revoked":     {Message: "web image credential was rejected (token_revoked)", HTTPStatus: http.StatusUnauthorized},
		"text invalidated":      {Message: "Encountered invalidated oauth token for user, failing request", HTTPStatus: http.StatusUnauthorized},
		"text code":             {Code: "token_revoked", Message: "unauthorized", HTTPStatus: http.StatusUnauthorized},
	}
}

func TestCodexRevocationStopsBothLinesOnFirstHit(t *testing.T) {
	for name, revoked := range revocationErrors() {
		for _, failing := range []string{quotaWebModel, quotaTextModel} {
			m, id := newQuotaDomainManager(t, "codex", nil)
			quotaMark(m, context.Background(), "codex", id, quotaWebModel, nil, false, 0)
			quotaMark(m, context.Background(), "codex", id, quotaTextModel, nil, false, 0)
			quotaMark(m, context.Background(), "codex", id, failing, revoked, false, 0)
			if !quotaBlocked(t, m, id, quotaWebModel) || !quotaBlocked(t, m, id, quotaTextModel) {
				t.Fatalf("%s on %s: revocation did not stop both lines", name, failing)
			}
		}
	}
	if codexRevocationResult(&Error{Message: "token_revoked", HTTPStatus: http.StatusForbidden}) || codexRevocationResult(&Error{Message: "unauthorized", HTTPStatus: http.StatusUnauthorized}) {
		t.Fatal("non-revocation error classified as revocation")
	}
}

func TestCodexPlainUnauthorizedStaysOnItsLine(t *testing.T) {
	m, id := newQuotaDomainManager(t, "codex", nil)
	quotaMark(m, context.Background(), "codex", id, quotaWebModel, nil, false, 0)
	quotaMark(m, context.Background(), "codex", id, quotaTextModel, nil, false, 0)
	quotaMark(m, context.Background(), "codex", id, quotaWebModel, &Error{Message: "web image credential was rejected (401)", HTTPStatus: http.StatusUnauthorized}, false, 0)
	if !quotaBlocked(t, m, id, quotaWebModel) || quotaBlocked(t, m, id, quotaTextModel) {
		t.Fatal("plain 401 with healthy states on both lines must cool only its own line")
	}
	if quotaAuth(t, m, id).CredentialRevoked {
		t.Fatal("plain 401 set the revoked flag")
	}
}

func TestCodexRevocationHeldUntilTokenChanges(t *testing.T) {
	revoked := &Error{Message: "web image credential was rejected (token_revoked)", HTTPStatus: http.StatusUnauthorized}
	m, id := newQuotaDomainManager(t, "codex", map[string]any{"access_token": "old-token"})
	quotaMark(m, context.Background(), "codex", id, quotaWebModel, nil, false, 0)
	quotaMark(m, context.Background(), "codex", id, quotaTextModel, nil, false, 0)
	quotaMark(m, context.Background(), "codex", id, quotaWebModel, revoked, false, 0)
	assertRevoked := func(step string) {
		t.Helper()
		if !quotaBlocked(t, m, id, quotaWebModel) || !quotaBlocked(t, m, id, quotaTextModel) {
			t.Fatalf("%s released the revoked credential", step)
		}
	}
	// Results of requests that were in flight on the same token change nothing.
	quotaMark(m, context.Background(), "codex", id, quotaTextModel, &Error{Message: "unauthorized", HTTPStatus: http.StatusUnauthorized}, false, 0)
	quotaMark(m, context.Background(), "codex", id, quotaTextModel, nil, false, 0)
	assertRevoked("plain 401 then success")
	quotaMark(m, context.Background(), "codex", id, quotaWebModel, &Error{Message: "bad gateway", HTTPStatus: http.StatusBadGateway}, false, 0)
	assertRevoked("502")
	if auth := quotaAuth(t, m, id); auth.Success != 3 || auth.Failed != 3 {
		t.Fatalf("request counters not kept while revoked: success=%d failed=%d", auth.Success, auth.Failed)
	}

	// Every deadline in the past plus a registry reconcile still leaves it blocked.
	past := time.Now().Add(-25 * time.Hour)
	m.mu.Lock()
	live := m.auths[id]
	live.NextRetryAfter = past
	for _, state := range live.ModelStates {
		state.NextRetryAfter = past
	}
	m.mu.Unlock()
	registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: quotaTextModel}, {ID: quotaWebModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	m.ReconcileRegistryModelStates(context.Background(), id)
	assertRevoked("expired deadlines and reconcile")

	same := quotaAuth(t, m, id)
	if _, err := m.Update(context.Background(), same); err != nil {
		t.Fatal(err)
	}
	assertRevoked("update with unchanged tokens")

	fresh := quotaAuth(t, m, id)
	fresh.Metadata = map[string]any{"access_token": "new-token"}
	if _, err := m.Update(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	if quotaBlocked(t, m, id, quotaWebModel) || quotaBlocked(t, m, id, quotaTextModel) {
		t.Fatalf("new access token did not release the credential: %+v", quotaAuth(t, m, id))
	}
}

func TestCodexLateRevocationOfReplacedTokenIsIgnored(t *testing.T) {
	revoked := &Error{Message: "web image credential was rejected (token_revoked)", HTTPStatus: http.StatusUnauthorized}
	m, id := newQuotaDomainManager(t, "codex", map[string]any{"access_token": "old-token"})
	// Two requests are in flight on the old token.
	inFlight := quotaAuth(t, m, id)
	m.markExecutionResult(context.Background(), Result{AuthID: id, Provider: "codex", Model: quotaWebModel, Error: revoked}, inFlight)
	if !quotaBlocked(t, m, id, quotaTextModel) {
		t.Fatal("first revocation did not stop the credential")
	}
	fresh := quotaAuth(t, m, id)
	fresh.Metadata = map[string]any{"access_token": "new-token"}
	if _, err := m.Update(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	m.markExecutionResult(context.Background(), Result{AuthID: id, Provider: "codex", Model: quotaTextModel, Error: revoked}, inFlight)
	if quotaBlocked(t, m, id, quotaWebModel) || quotaBlocked(t, m, id, quotaTextModel) {
		t.Fatalf("late revocation of the old token blocked the new token: %+v", quotaAuth(t, m, id))
	}
	// The new token is still revocable.
	m.markExecutionResult(context.Background(), Result{AuthID: id, Provider: "codex", Model: quotaTextModel, Error: revoked}, quotaAuth(t, m, id))
	if !quotaBlocked(t, m, id, quotaWebModel) {
		t.Fatal("revocation of the current token was ignored")
	}
}

func TestCodexWebImageUnauthorizedDuringTextQuotaKeepsTextState(t *testing.T) {
	m, id := newQuotaDomainManager(t, "codex", nil)
	quotaTextExhausted(m, "codex", id, time.Hour)
	quotaMark(m, context.Background(), "codex", id, quotaWebModel, &Error{Message: "web image credential was rejected (401)", HTTPStatus: http.StatusUnauthorized}, false, 0)
	auth := quotaAuth(t, m, id)
	if hasUnauthorizedAuthFailure(auth) {
		t.Fatalf("plain web-image 401 marked the whole credential unauthorized: %+v", auth.LastError)
	}
	if auth.Quota.Reason != "credential_quota" {
		t.Fatalf("text quota lost: %+v", auth.Quota)
	}
	state := auth.ModelStates[quotaWebModel]
	if state == nil || !state.NextRetryAfter.After(time.Now().Add(29*time.Minute)) || state.NextRetryAfter.After(time.Now().Add(31*time.Minute)) {
		t.Fatalf("web-image state not cooled for 30 minutes: %+v", state)
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, quotaTextModel, time.Now().Add(2*time.Hour)); blocked {
		t.Fatal("text blocked beyond its quota cooldown")
	}
}

func TestCodexQuotaDomainProjection(t *testing.T) {
	now := time.Now()
	for _, withWebState := range []bool{false, true} {
		m, id := newQuotaDomainManager(t, "codex", nil)
		if withWebState {
			quotaMark(m, context.Background(), "codex", id, quotaWebModel, nil, false, 0)
		}
		quotaTextExhausted(m, "codex", id, time.Hour)
		auth := quotaAuth(t, m, id)
		if m.clientModelProjectionForAuth(auth, quotaWebModel, now).Suspended {
			t.Fatalf("withWebState=%v: web image projected as suspended under text quota", withWebState)
		}
		if !m.clientModelProjectionForAuth(auth, quotaTextModel, now).Suspended {
			t.Fatalf("withWebState=%v: text not projected as suspended", withWebState)
		}
	}
	revoked := &Error{Message: "web image credential was rejected (token_revoked)", HTTPStatus: http.StatusUnauthorized}
	for _, otherHasState := range []bool{false, true} {
		for failing, other := range map[string]string{quotaWebModel: quotaTextModel, quotaTextModel: quotaWebModel} {
			m, id := newQuotaDomainManager(t, "codex", nil)
			if otherHasState {
				quotaMark(m, context.Background(), "codex", id, other, nil, false, 0)
			}
			quotaMark(m, context.Background(), "codex", id, failing, revoked, false, 0)
			if !m.clientModelProjectionForAuth(quotaAuth(t, m, id), other, now).Suspended {
				t.Fatalf("revocation on %s (other state=%v): %s not projected as suspended", failing, otherHasState, other)
			}
		}
	}
}

func TestCodexLegacySchedulerFollowsQuotaDomains(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex"})
	id := "quota-domain-scheduler"
	registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: quotaTextModel}, {ID: quotaWebModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatal(err)
	}
	pick := func(model string) error {
		_, _, _, err := m.pickNextMixed(context.Background(), []string{"codex"}, model, cliproxyexecutor.Options{}, nil)
		return err
	}
	if err := pick(quotaWebModel); err != nil {
		t.Fatalf("web image pick before any failure: %v", err)
	}
	quotaTextExhausted(m, "codex", id, time.Hour)
	if err := pick(quotaTextModel); err == nil {
		t.Fatal("text picked a credential whose text quota is exhausted")
	}
	if err := pick(quotaWebModel); err != nil {
		t.Fatalf("web image not selectable while only the text quota is exhausted: %v", err)
	}
	quotaMark(m, context.Background(), "codex", id, quotaTextModel, &Error{Message: "Encountered invalidated oauth token for user, failing request", HTTPStatus: http.StatusUnauthorized}, false, 0)
	if err := pick(quotaWebModel); err == nil {
		t.Fatal("web image shard kept a credential revoked on the text line")
	}
}
