package helps

import (
	"context"

	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestCodexSpeedTimingUsesCreatedAndRealOutput(t *testing.T) {
	r := &UsageReporter{provider: "codex"}
	r.StartResponseTTFT()
	ObserveResponsesTokenEvent(r, []byte(`{"type":"response.created"}`))
	if r.speedCreatedAt.IsZero() {
		t.Fatal("response.created not recorded")
	}
	// Shift the recorded times instead of sleeping, so the test does not depend on timer resolution.
	r.speedCreatedAt = r.speedCreatedAt.Add(-2 * time.Second)
	r.speedSentAt = r.speedCreatedAt.Add(-time.Second)
	ObserveResponsesTokenEvent(r, []byte(`{"type":"response.output_text.delta","delta":"OK"}`))
	ObserveResponsesTokenEvent(r, []byte(`{"type":"response.output_text.delta","delta":"later"}`))
	gap, pre, _ := r.codexSpeedGap()
	if gap < 2*time.Second || pre != time.Second {
		t.Fatalf("gap=%v pre=%v", gap, pre)
	}
}

func TestCodexSpeedTimingIgnoresTerminalFallback(t *testing.T) {
	r := &UsageReporter{provider: "codex"}
	r.StartResponseTTFT()
	ObserveResponsesTokenEvent(r, []byte(`{"type":"response.created"}`))
	ObserveResponsesTokenEvent(r, []byte(`{"type":"response.completed","response":{}}`))
	if gap, _, _ := r.codexSpeedGap(); gap != 0 {
		t.Fatalf("a completion without output must give no gap, got %v", gap)
	}
	r2 := &UsageReporter{provider: "codex"}
	ObserveResponsesTokenEvent(r2, []byte(`{"type":"response.output_text.delta","delta":"x"}`))
	if gap, _, _ := r2.codexSpeedGap(); gap != 0 {
		t.Fatal("missing response.created must give no gap")
	}
}

func TestCodexSpeedOverloadClassification(t *testing.T) {
	cases := []struct {
		rec  usage.Record
		want bool
	}{
		{usage.Record{Failed: true, Fail: usage.Failure{StatusCode: 503}}, true},
		{usage.Record{Failed: true, Fail: usage.Failure{StatusCode: 200, Body: `{"code":"server_is_overloaded"}`}}, true},
		{usage.Record{Failed: true, Fail: usage.Failure{StatusCode: 429}}, false},
		{usage.Record{Failed: false, Fail: usage.Failure{StatusCode: 503}}, false},
	}
	for i, tc := range cases {
		if got := codexSpeedOverload(tc.rec); got != tc.want {
			t.Errorf("case %d: got %v", i, got)
		}
	}
	if !CodexRequestHasImageInput([]byte(`{"input":[{"type":"input_image"}]}`)) || CodexRequestHasImageInput([]byte(`{"input":"text"}`)) {
		t.Fatal("image detection")
	}
}

func TestCodexSpeedTimingSkipsEmptyDelta(t *testing.T) {
	r := &UsageReporter{provider: "codex"}
	r.StartResponseTTFT()
	ObserveResponsesTokenEvent(r, []byte(`{"type":"response.created"}`))
	r.speedCreatedAt = r.speedCreatedAt.Add(-time.Second)
	for _, empty := range []string{`{"type":"response.output_text.delta","delta":""}`, `{"type":"response.output_text.delta"}`, `{"type":"response.reasoning_text.delta","delta":null}`} {
		ObserveResponsesTimingEvent(r, []byte(empty))
	}
	if !r.speedOutputAt.IsZero() {
		t.Fatal("empty deltas must not mark the first output")
	}
	ObserveResponsesTimingEvent(r, []byte(`{"type":"response.output_text.delta","delta":" "}`))
	if r.speedOutputAt.IsZero() {
		t.Fatal("a whitespace delta is real output")
	}
}

// The reporter freezes the account's speed group when the attempt starts; API key accounts are
// left out of speed statistics.
func TestCodexSpeedReporterFreezesGroupAtStart(t *testing.T) {
	ctx := context.Background()
	oauth := &cliproxyauth.Auth{ID: "oauth-acct", Provider: "codex"}
	if got := NewUsageReporter(ctx, "codex", "m", oauth).speedGroup; got != cliproxyauth.CodexSpeedGroupOff {
		t.Fatalf("disabled ranking must tag the attempt off, got %q", got)
	}
	key := &cliproxyauth.Auth{ID: "key-acct", Provider: "codex", Attributes: map[string]string{cliproxyauth.AttributeAPIKey: "sk"}}
	if got := NewUsageReporter(ctx, "codex", "m", key).speedGroup; got != "" {
		t.Fatalf("API key accounts must be excluded, got %q", got)
	}
	if got := NewUsageReporter(ctx, "openai", "m", oauth).speedGroup; got != "" {
		t.Fatalf("non-Codex providers carry no speed group, got %q", got)
	}
}
