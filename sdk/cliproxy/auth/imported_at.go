package auth

import (
	"encoding/json"
	"strings"
	"time"
)

// MetadataImportedAt is the auth metadata key holding the moment an account was
// first imported into this platform.
const MetadataImportedAt = "imported_at"

// ResolveImportedAt returns the account's import timestamp, seeding metadata
// from fallback the first time an account is seen.
//
// Storage backends derive CreatedAt from the auth file's mtime, which moves
// every time a token refresh rewrites the file, so a restart would otherwise
// report the last refresh as the creation time. Seeding the value into metadata
// makes the next Save persist it, after which the timestamp stays put.
//
// The metadata map is mutated in place; callers pass the same map they hand to
// Auth.Metadata so the seeded value rides along to storage.
func ResolveImportedAt(metadata map[string]any, fallback time.Time) time.Time {
	if metadata == nil {
		return fallback
	}
	if parsed, ok := parseImportedAt(metadata[MetadataImportedAt]); ok {
		return parsed
	}
	if fallback.IsZero() {
		fallback = time.Now()
	}
	fallback = fallback.UTC()
	metadata[MetadataImportedAt] = fallback.Format(time.RFC3339Nano)
	return fallback
}

// parseImportedAt accepts the shapes the value can take across a JSON round
// trip or a hand-edited auth file.
func parseImportedAt(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case time.Time:
		if !typed.IsZero() {
			return typed, true
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return time.Time{}, false
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
			if parsed, err := time.Parse(layout, trimmed); err == nil && !parsed.IsZero() {
				return parsed, true
			}
		}
	case float64:
		if typed > 0 {
			return time.Unix(int64(typed), 0).UTC(), true
		}
	case json.Number:
		if seconds, err := typed.Int64(); err == nil && seconds > 0 {
			return time.Unix(seconds, 0).UTC(), true
		}
	}
	return time.Time{}, false
}
