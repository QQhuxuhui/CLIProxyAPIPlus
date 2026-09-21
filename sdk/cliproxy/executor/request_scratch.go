package executor

import (
	"strings"
	"sync"
)

// RequestScratchMetadataKey holds host-private, request-local memoised results in
// Options.Metadata. The value is a pointer, so it survives the shallow metadata
// copies made while one request moves through retry attempts. It must never be
// serialised to plugins or other processes; see IsHostPrivateMetadataKey.
const RequestScratchMetadataKey = "_request_scratch"

// maxRequestScratchEntries bounds what one request may pin. Entries can reference
// payload-sized buffers, so the cap keeps the worst case at a few bodies.
const maxRequestScratchEntries = 4

// RequestScratch memoises deterministic, expensive results for the lifetime of a
// single request. Every user must treat a miss as the normal path: the scratch is
// optional, bounded, and may be cleared at any time.
type RequestScratch struct {
	mu     sync.Mutex
	values map[string]any
}

// WithRequestScratch attaches a scratch to metadata when none is present and
// returns the metadata map. A nil map is allocated.
func WithRequestScratch(metadata map[string]any) map[string]any {
	if metadata == nil {
		metadata = make(map[string]any, 1)
	}
	if _, ok := metadata[RequestScratchMetadataKey].(*RequestScratch); !ok {
		metadata[RequestScratchMetadataKey] = &RequestScratch{}
	}
	return metadata
}

// RequestScratchFrom returns the scratch attached to metadata, or nil.
func RequestScratchFrom(metadata map[string]any) *RequestScratch {
	if metadata == nil {
		return nil
	}
	scratch, _ := metadata[RequestScratchMetadataKey].(*RequestScratch)
	return scratch
}

// Load returns a memoised value.
func (s *RequestScratch) Load(key string) (any, bool) {
	if s == nil || key == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[key]
	return value, ok
}

// Store memoises value unless the scratch is full. It reports whether the value
// was kept.
func (s *RequestScratch) Store(key string, value any) bool {
	if s == nil || key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.values[key]; !exists && len(s.values) >= maxRequestScratchEntries {
		return false
	}
	if s.values == nil {
		s.values = make(map[string]any, 1)
	}
	s.values[key] = value
	return true
}

// DeletePrefix drops every entry whose key starts with prefix so the referenced
// buffers can be collected once they are no longer useful.
func (s *RequestScratch) DeletePrefix(prefix string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.values {
		if strings.HasPrefix(key, prefix) {
			delete(s.values, key)
		}
	}
}

// IsHostPrivateMetadataKey reports whether a metadata key carries in-process state
// that must not cross a serialisation boundary.
func IsHostPrivateMetadataKey(key string) bool {
	return key == SessionInfoCacheMetadataKey || key == RequestScratchMetadataKey
}
