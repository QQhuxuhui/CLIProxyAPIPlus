package executor

import (
	"context"
	"strconv"
	"sync/atomic"
)

type codexGenerationObserverKey struct{}

var codexLogicalCallBuckets [17]atomic.Int64

// WithCodexGenerationObserver shares the logical request's counter across transports.
func WithCodexGenerationObserver(ctx context.Context, observe func()) context.Context {
	return context.WithValue(ctx, codexGenerationObserverKey{}, observe)
}

// ObserveCodexGeneration runs immediately before a generation transport attempt.
// Dial-only websocket handshakes and admission failures are not generation calls.
func ObserveCodexGeneration(ctx context.Context) {
	if ctx != nil {
		if observe, ok := ctx.Value(codexGenerationObserverKey{}).(func()); ok && observe != nil {
			observe()
		}
	}
}

// ObserveCodexLogicalCall moves one request between bounded call-count buckets.
func ObserveCodexLogicalCall(count uint64) {
	if count == 0 {
		return
	}
	if count > 1 {
		codexLogicalCallBuckets[min(count-1, 16)].Add(-1)
	}
	codexLogicalCallBuckets[min(count, 16)].Add(1)
}
func CodexLogicalCallMetrics() map[string]int64 {
	out := make(map[string]int64, 16)
	for i := 1; i <= 16; i++ {
		key := strconv.Itoa(i)
		if i == 16 {
			key = "16+"
		}
		out[key] = codexLogicalCallBuckets[i].Load()
	}
	return out
}
