package auth

import (
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

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
