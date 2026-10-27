package executor

import "testing"

func TestCodexBootstrapMetricsReasons(t *testing.T) {
	before := CodexBootstrapMetrics()
	ObserveCodexBootstrapRelease("frame budget")
	ObserveCodexBootstrapRelease("byte budget")
	after := CodexBootstrapMetrics()
	if after["frame budget"] != before["frame budget"]+1 || after["byte budget"] != before["byte budget"]+1 {
		t.Fatalf("counters before=%v after=%v", before, after)
	}
}
