package auth

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type codexProbeRateError struct{ reset time.Time }

func (e codexProbeRateError) Error() string            { return "rate limited" }
func (e codexProbeRateError) StatusCode() int          { return 429 }
func (e codexProbeRateError) IsCredentialScoped() bool { return true }
func (e codexProbeRateError) QuotaDetail() (cliproxyexecutor.QuotaDetail, bool) {
	return cliproxyexecutor.QuotaDetail{ReasonCode: "RATE_LIMIT_EXCEEDED", ResetAt: e.reset}, true
}

func TestCodexProbeFeedbackCoolsAccountWithoutBusinessMetrics(t *testing.T) {
	hook := &recordingHook{}
	m := NewManager(nil, nil, hook)
	now := time.Now()
	m.rateCap.nowFunc = func() time.Time { return now }
	m.auths["a"] = &Auth{ID: "a", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "t"}}
	probe, reason := m.AcquireCodexProbeAuth("gpt-6-luna")
	if probe == nil {
		t.Fatalf("acquire: %s", reason)
	}
	probeToken := m.rateCap.sequence
	// A concurrent business request must not be marked rejected by the probe result.
	businessToken := m.rateCap.start("a")
	beforeAttempts := m.rateCap.metrics.bucket(now).Attempts
	beforeLimit := m.rateCap.accounts["a"].limit
	reset := now.Add(2 * time.Minute)
	m.MarkCodexProbeResult(context.Background(), probe, "gpt-6-luna", codexProbeRateError{reset: reset})
	if next, _ := m.AcquireCodexProbeAuth("another-model"); next != nil {
		t.Fatal("rate-limited credential still selected for a different model")
	}
	a := m.auths["a"]
	if !a.Quota.NextRecoverAt.Equal(reset) {
		t.Fatalf("cooldown = %v, want %v", a.Quota.NextRecoverAt, reset)
	}
	if a.Success != 0 || a.Failed != 0 || a.recentRequests != (recentRequestRing{}) || hook.lastResult.Load() != nil {
		t.Fatalf("probe polluted business history: success=%d failed=%d hook=%v", a.Success, a.Failed, hook.lastResult.Load())
	}
	if got := m.rateCap.metrics.bucket(now).Attempts; got != beforeAttempts {
		t.Fatalf("attempt metrics = %d, want %d", got, beforeAttempts)
	}
	account := m.rateCap.accounts["a"]
	if account.limit >= beforeLimit {
		t.Fatalf("learned cap did not decrease: %d -> %d", beforeLimit, account.limit)
	}
	for _, attempt := range account.attempts {
		if attempt.id == probeToken && !attempt.rejected {
			t.Fatal("probe attempt was not rejected")
		}
		if attempt.id == businessToken && attempt.rejected {
			t.Fatal("business attempt was rejected")
		}
	}
	// A repeated callback must be harmless.
	generation := a.Generation
	m.MarkCodexProbeResult(context.Background(), probe, "gpt-6-luna", codexProbeRateError{reset: reset})
	if a.Generation != generation {
		t.Fatal("duplicate feedback mutated auth")
	}
}

func TestCodexProbeFeedbackIgnoresSuccessAndCancellation(t *testing.T) {
	for _, probeErr := range []error{nil, context.Canceled} {
		m := NewManager(nil, nil, nil)
		m.auths["a"] = &Auth{ID: "a", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "t"}}
		probe, _ := m.AcquireCodexProbeAuth("gpt-6-luna")
		m.MarkCodexProbeResult(context.Background(), probe, "gpt-6-luna", probeErr)
		if a := m.auths["a"]; a.Generation != 0 || a.Success != 0 || a.Failed != 0 || a.Status != StatusActive {
			t.Fatalf("probe %v changed auth: %+v", probeErr, a)
		}
	}
}

func TestAcquireCodexProbeAuthRespectsRateWindow(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Unix(1800000000, 0)
	m.rateCap.nowFunc = func() time.Time { return now }

	if auth, reason := m.AcquireCodexProbeAuth("gpt-6-luna"); auth != nil || reason != "no codex oauth credential" {
		t.Fatalf("empty manager: %v %q", auth, reason)
	}

	m.auths["key"] = &Auth{ID: "key", Provider: "codex", Status: StatusActive, Attributes: map[string]string{"api_key": "sk"}}
	m.auths["off"] = &Auth{ID: "off", Provider: "codex", Status: StatusActive, Disabled: true, Metadata: map[string]any{"access_token": "t"}}
	m.auths["a"] = &Auth{ID: "a", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "t"}}

	// Probes may use the account's whole learned per-minute limit, never more.
	_, limit, _ := m.rateCap.account("a")
	for i := 0; i < limit; i++ {
		auth, reason := m.AcquireCodexProbeAuth("gpt-6-luna")
		if auth == nil || auth.ID != "a" {
			t.Fatalf("acquire %d: %v %q", i, auth, reason)
		}
	}
	if auth, reason := m.AcquireCodexProbeAuth("gpt-6-luna"); auth != nil || reason != "all codex oauth credentials at rate window limit" {
		t.Fatalf("full window: %v %q", auth, reason)
	}
	if count, _, _ := m.rateCap.account("a"); count != limit {
		t.Fatalf("rate window count = %d, want %d", count, limit)
	}
	if attempts := m.rateCap.metrics.bucket(now).Attempts; attempts != 0 {
		t.Fatalf("probe attempts leaked into pool metrics: %d", attempts)
	}

	now = now.Add(time.Minute + time.Second)
	if auth, _ := m.AcquireCodexProbeAuth("gpt-6-luna"); auth == nil {
		t.Fatal("window did not slide")
	}

	homeCfg := &internalconfig.Config{}
	homeCfg.Home.Enabled = true
	m.runtimeConfig.Store(homeCfg)
	if auth, reason := m.AcquireCodexProbeAuth("gpt-6-luna"); auth != nil || reason != "home mode" {
		t.Fatalf("home mode: %v %q", auth, reason)
	}
}
