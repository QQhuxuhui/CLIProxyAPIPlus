package executor

import "sync/atomic"

var codexBootstrapReleaseCounts [7]atomic.Uint64
var codexBootstrapReleaseReasons = [...]string{"content", "event", "terminal", "frame budget", "byte budget", "time budget", "overload"}

// ObserveCodexBootstrapRelease counts a fixed set of bootstrap outcomes. Unknown
// events share one bucket so upstream event names cannot grow label cardinality.
func ObserveCodexBootstrapRelease(reason string) {
	for i, name := range codexBootstrapReleaseReasons {
		if name == reason {
			codexBootstrapReleaseCounts[i].Add(1)
			return
		}
	}
	codexBootstrapReleaseCounts[1].Add(1)
}

// CodexBootstrapMetrics returns process-lifetime counters for management telemetry.
func CodexBootstrapMetrics() map[string]uint64 {
	out := make(map[string]uint64, len(codexBootstrapReleaseReasons))
	for i, reason := range codexBootstrapReleaseReasons {
		out[reason] = codexBootstrapReleaseCounts[i].Load()
	}
	return out
}
