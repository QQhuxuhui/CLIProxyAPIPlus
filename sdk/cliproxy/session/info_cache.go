package session

import (
	"net/http"
	"slices"
	"sync"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type infoCacheFunc func(http.Header, []byte, map[string]any) (SessionInfo, bool)

type infoCache struct {
	mu       sync.Mutex
	set      bool
	payload  []byte
	headers  http.Header
	metadata [4]string
	info     SessionInfo
	found    bool
}

// WithInfoCache attaches request-local session extraction state without changing
// the supplied map. Request bodies must remain immutable; rewrites replace the
// slice. Header and session metadata changes are checked on every lookup.
func WithInfoCache(metadata map[string]any) map[string]any {
	if _, ok := metadata[cliproxyexecutor.SessionInfoCacheMetadataKey].(infoCacheFunc); ok {
		return metadata
	}
	out := make(map[string]any, len(metadata)+1)
	for key, value := range metadata {
		out[key] = value
	}
	cache := &infoCache{}
	// A function value survives shallow metadata copies and is omitted by the
	// plugin JSON sanitizer, unlike a struct containing internal cache state.
	out[cliproxyexecutor.SessionInfoCacheMetadataKey] = infoCacheFunc(cache.extract)
	return out
}

func (c *infoCache) extract(headers http.Header, payload []byte, metadata map[string]any) (SessionInfo, bool) {
	var inputs [4]string
	for i, key := range [...]string{
		cliproxyexecutor.CallerScopeMetadataKey,
		cliproxyexecutor.ExecutionSessionMetadataKey,
		cliproxyexecutor.LCPAffinitySessionIDMetadataKey,
		cliproxyexecutor.ParentSessionIDMetadataKey,
	} {
		inputs[i], _ = metadata[key].(string)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	samePayload := len(c.payload) == len(payload) && (len(payload) == 0 || &c.payload[0] == &payload[0])
	if c.set && samePayload && c.metadata == inputs && sameSessionHeaders(c.headers, headers) {
		return c.info, c.found
	}
	c.info, c.found = extractSessionInfo(headers, payload, metadata)
	c.payload = payload
	c.headers = headers.Clone()
	c.metadata = inputs
	c.set = true
	return c.info, c.found
}

func sameSessionHeaders(left, right http.Header) bool {
	if len(left) != len(right) {
		return false
	}
	for key, values := range left {
		other, ok := right[key]
		if !ok || !slices.Equal(values, other) {
			return false
		}
	}
	return true
}
