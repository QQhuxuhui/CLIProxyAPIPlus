package helps

import (
	"testing"
	"time"

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
