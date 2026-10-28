package helps

import "testing"

func TestCodexBootstrapReleaseReason(t *testing.T) {
	for _, tt := range []struct{ payload, reason string }{
		{`{"type":"response.output_item.added","item":{"type":"function_call"}}`, "tool"},
		{`{"type":"response.function_call_arguments.delta","delta":"x"}`, "tool"},
		{`{"type":"response.new_event"}`, "unknown"},
		{`{"type":"response.output_text.delta","delta":"hello"}`, "content"},
	} {
		if got := CodexBootstrapReleaseReason([]byte(tt.payload)); got != tt.reason {
			t.Errorf("%s: %s", tt.payload, got)
		}
	}
}
