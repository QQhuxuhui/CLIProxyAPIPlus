package executor

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultMaxTraceRecords is the default maximum number of trace records to keep.
const DefaultMaxTraceRecords = 100

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneTraceRecord(in *MasqueradeTraceRecord) *MasqueradeTraceRecord {
	if in == nil {
		return nil
	}
	out := *in
	out.OriginalHeaders = cloneStringMap(in.OriginalHeaders)
	out.MaskedHeaders = cloneStringMap(in.MaskedHeaders)
	return &out
}

// MasqueradeTraceRecord represents a single masquerade trace entry.
// It captures the original request and the masqueraded version for comparison.
type MasqueradeTraceRecord struct {
	ID        string `json:"id"`
	Timestamp int64  `json:"timestamp"`
	Model     string `json:"model"`
	AuthID    string `json:"auth_id"`
	AuthLabel string `json:"auth_label"`

	// Original request data
	OriginalHeaders map[string]string `json:"original_headers"`
	OriginalBody    string            `json:"original_body,omitempty"`

	// Masqueraded request data
	MaskedHeaders map[string]string `json:"masked_headers"`
	MaskedBody    string            `json:"masked_body,omitempty"`

	// User ID comparison
	OriginalUserID  string `json:"original_user_id"`
	MaskedUserID    string `json:"masked_user_id"`
	OriginalSession string `json:"original_session"`
	MaskedSession   string `json:"masked_session"`

	// Session pool info
	HashSource string `json:"hash_source,omitempty"` // "client" or "channel"
}

// MasqueradeTraceSummary is a lightweight version for list responses.
type MasqueradeTraceSummary struct {
	ID              string `json:"id"`
	Timestamp       int64  `json:"timestamp"`
	Model           string `json:"model"`
	AuthID          string `json:"auth_id"`
	AuthLabel       string `json:"auth_label"`
	OriginalUserID  string `json:"original_user_id"`
	MaskedUserID    string `json:"masked_user_id"`
	UserIDChanged   bool   `json:"user_id_changed"`
	HeadersModified int    `json:"headers_modified"`
}

// ToSummary converts a full record to a summary.
func (r *MasqueradeTraceRecord) ToSummary() MasqueradeTraceSummary {
	headersModified := 0
	for k, v := range r.MaskedHeaders {
		if orig, ok := r.OriginalHeaders[k]; !ok || orig != v {
			headersModified++
		}
	}

	return MasqueradeTraceSummary{
		ID:              r.ID,
		Timestamp:       r.Timestamp,
		Model:           r.Model,
		AuthID:          r.AuthID,
		AuthLabel:       r.AuthLabel,
		OriginalUserID:  r.OriginalUserID,
		MaskedUserID:    r.MaskedUserID,
		UserIDChanged:   r.OriginalUserID != r.MaskedUserID,
		HeadersModified: headersModified,
	}
}

// MasqueradeTraceStore provides thread-safe storage for trace records.
// It uses a ring buffer to limit memory usage.
type MasqueradeTraceStore struct {
	records   []*MasqueradeTraceRecord
	maxSize   int
	index     int  // Next write position
	count     int  // Total records added (may exceed maxSize)
	full      bool // Whether the ring buffer has wrapped
	enabled   bool
	mu        sync.RWMutex
}

var (
	globalTraceStore     *MasqueradeTraceStore
	globalTraceStoreOnce sync.Once
)

// GetGlobalTraceStore returns the singleton trace store.
func GetGlobalTraceStore() *MasqueradeTraceStore {
	globalTraceStoreOnce.Do(func() {
		globalTraceStore = NewMasqueradeTraceStore(DefaultMaxTraceRecords)
	})
	return globalTraceStore
}

// NewMasqueradeTraceStore creates a new trace store with the given capacity.
func NewMasqueradeTraceStore(maxSize int) *MasqueradeTraceStore {
	if maxSize <= 0 {
		maxSize = DefaultMaxTraceRecords
	}
	return &MasqueradeTraceStore{
		records: make([]*MasqueradeTraceRecord, maxSize),
		maxSize: maxSize,
		enabled: false, // Disabled by default (opt-in for debugging)
	}
}

// SetEnabled controls whether tracing is active.
func (s *MasqueradeTraceStore) SetEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = enabled
}

// IsEnabled returns whether tracing is active.
func (s *MasqueradeTraceStore) IsEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

// Add stores a new trace record. Returns the assigned ID.
func (s *MasqueradeTraceStore) Add(record *MasqueradeTraceRecord) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.enabled {
		return ""
	}
	if record == nil {
		return ""
	}

	// Assign ID and timestamp if not set
	if record.ID == "" {
		record.ID = uuid.New().String()
	}
	if record.Timestamp == 0 {
		record.Timestamp = time.Now().UnixMilli()
	}

	// Store in ring buffer
	s.records[s.index] = cloneTraceRecord(record)
	s.index = (s.index + 1) % s.maxSize
	s.count++
	if s.count >= s.maxSize {
		s.full = true
	}

	return record.ID
}

// Get retrieves a single record by ID.
func (s *MasqueradeTraceStore) Get(id string) *MasqueradeTraceRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := range s.records {
		if s.records[i] != nil && s.records[i].ID == id {
			// Return a copy to prevent mutation
			return cloneTraceRecord(s.records[i])
		}
	}
	return nil
}

// List returns all stored records as summaries, newest first.
func (s *MasqueradeTraceStore) List() []MasqueradeTraceSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]MasqueradeTraceSummary, 0, s.activeCount())

	// Iterate in reverse order (newest first)
	if s.full {
		// Start from index-1 (newest) and wrap around
		for i := 0; i < s.maxSize; i++ {
			idx := (s.index - 1 - i + s.maxSize) % s.maxSize
			if s.records[idx] != nil {
				result = append(result, s.records[idx].ToSummary())
			}
		}
	} else {
		// Not full, iterate from index-1 down to 0
		for i := s.index - 1; i >= 0; i-- {
			if s.records[i] != nil {
				result = append(result, s.records[i].ToSummary())
			}
		}
	}

	return result
}

// ListFull returns all stored records with full data, newest first.
func (s *MasqueradeTraceStore) ListFull() []*MasqueradeTraceRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*MasqueradeTraceRecord, 0, s.activeCount())

	// Iterate in reverse order (newest first)
	if s.full {
		for i := 0; i < s.maxSize; i++ {
			idx := (s.index - 1 - i + s.maxSize) % s.maxSize
			if s.records[idx] != nil {
				result = append(result, cloneTraceRecord(s.records[idx]))
			}
		}
	} else {
		for i := s.index - 1; i >= 0; i-- {
			if s.records[i] != nil {
				result = append(result, cloneTraceRecord(s.records[i]))
			}
		}
	}

	return result
}

// Clear removes all stored records.
func (s *MasqueradeTraceStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.records {
		s.records[i] = nil
	}
	s.index = 0
	s.count = 0
	s.full = false
}

// Count returns the number of stored records.
func (s *MasqueradeTraceStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeCount()
}

// activeCount returns the actual number of records (not thread-safe, call with lock).
func (s *MasqueradeTraceStore) activeCount() int {
	if s.full {
		return s.maxSize
	}
	return s.index
}

// SetMaxSize updates the maximum size. Existing records may be lost if shrinking.
func (s *MasqueradeTraceStore) SetMaxSize(maxSize int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if maxSize <= 0 {
		maxSize = DefaultMaxTraceRecords
	}
	if maxSize == s.maxSize {
		return
	}

	// Create new buffer
	newRecords := make([]*MasqueradeTraceRecord, maxSize)
	oldCount := s.activeCount()

	// Copy records, preserving newest
	copyCount := oldCount
	if copyCount > maxSize {
		copyCount = maxSize
	}

	for i := 0; i < copyCount; i++ {
		var srcIdx int
		if s.full {
			srcIdx = (s.index - copyCount + i + s.maxSize) % s.maxSize
		} else {
			srcIdx = s.index - copyCount + i
		}
		newRecords[i] = s.records[srcIdx]
	}

	s.records = newRecords
	s.maxSize = maxSize
	s.index = copyCount % maxSize
	s.full = copyCount >= maxSize
	if !s.full {
		s.count = copyCount
	}
}

// RecordMasquerade creates and stores a trace record from original and masked data.
func RecordMasquerade(
	model, authID, authLabel string,
	originalHeaders, maskedHeaders map[string]string,
	originalBody, maskedBody []byte,
	originalUserID, maskedUserID string,
	hashSource string,
) {
	store := GetGlobalTraceStore()
	if !store.IsEnabled() {
		return
	}

	// Extract session UUIDs from user_ids
	originalSession := extractSessionFromUserID(originalUserID)
	maskedSession := extractSessionFromUserID(maskedUserID)

	record := &MasqueradeTraceRecord{
		Model:           model,
		AuthID:          authID,
		AuthLabel:       authLabel,
		OriginalHeaders: originalHeaders,
		OriginalBody:    truncateBody(originalBody, 4096),
		MaskedHeaders:   maskedHeaders,
		MaskedBody:      truncateBody(maskedBody, 4096),
		OriginalUserID:  originalUserID,
		MaskedUserID:    maskedUserID,
		OriginalSession: originalSession,
		MaskedSession:   maskedSession,
		HashSource:      hashSource,
	}

	store.Add(record)
}

// extractSessionFromUserID extracts the session UUID from a user_id string.
func extractSessionFromUserID(userID string) string {
	const marker = "_account__session_"
	idx := len(userID) - 36 // UUID is 36 chars
	if idx > 0 && len(userID) > len(marker)+36 {
		return userID[idx:]
	}
	return ""
}

// truncateBody limits body size for storage.
func truncateBody(body []byte, maxLen int) string {
	if len(body) <= maxLen {
		return string(body)
	}
	return string(body[:maxLen]) + "...[truncated]"
}
