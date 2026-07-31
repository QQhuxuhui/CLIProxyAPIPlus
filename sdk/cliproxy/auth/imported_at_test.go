package auth

import (
	"encoding/json"
	"testing"
	"time"
)

func TestResolveImportedAtSeedsAndFreezes(t *testing.T) {
	metadata := map[string]any{"email": "a@example.com"}
	mtime := time.Date(2026, 7, 20, 8, 30, 0, 0, time.UTC)

	first := ResolveImportedAt(metadata, mtime)
	if !first.Equal(mtime) {
		t.Fatalf("first resolve = %v, want the fallback %v", first, mtime)
	}
	raw, ok := metadata[MetadataImportedAt].(string)
	if !ok || raw == "" {
		t.Fatalf("metadata %q not seeded, got %#v", MetadataImportedAt, metadata[MetadataImportedAt])
	}

	// A later load sees a newer mtime (a token refresh rewrote the file) but the
	// import time must not move.
	later := ResolveImportedAt(metadata, mtime.Add(72*time.Hour))
	if !later.Equal(mtime) {
		t.Errorf("second resolve = %v, want the seeded %v", later, mtime)
	}
	if got := metadata[MetadataImportedAt].(string); got != raw {
		t.Errorf("seeded value rewritten: %q -> %q", raw, got)
	}
}

func TestResolveImportedAtSurvivesJSONRoundTrip(t *testing.T) {
	mtime := time.Date(2026, 7, 20, 8, 30, 0, 123456789, time.UTC)
	metadata := map[string]any{}
	ResolveImportedAt(metadata, mtime)

	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	var decoded map[string]any
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}

	got := ResolveImportedAt(decoded, time.Now())
	if !got.Equal(mtime) {
		t.Errorf("after round trip = %v, want %v", got, mtime)
	}
}

func TestResolveImportedAtAcceptsHandEditedValues(t *testing.T) {
	fallback := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		value any
		want  time.Time
	}{
		{"rfc3339", "2026-07-20T08:30:00Z", time.Date(2026, 7, 20, 8, 30, 0, 0, time.UTC)},
		{"no zone", "2026-07-20T08:30:00", time.Date(2026, 7, 20, 8, 30, 0, 0, time.UTC)},
		{"padded", "  2026-07-20T08:30:00Z  ", time.Date(2026, 7, 20, 8, 30, 0, 0, time.UTC)},
		{"time value", time.Date(2026, 7, 20, 8, 30, 0, 0, time.UTC), time.Date(2026, 7, 20, 8, 30, 0, 0, time.UTC)},
		{"unix seconds", float64(1784795400), time.Unix(1784795400, 0).UTC()},
		{"json number", json.Number("1784795400"), time.Unix(1784795400, 0).UTC()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metadata := map[string]any{MetadataImportedAt: tc.value}
			if got := ResolveImportedAt(metadata, fallback); !got.Equal(tc.want) {
				t.Errorf("ResolveImportedAt(%#v) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestResolveImportedAtReseedsUnusableValues(t *testing.T) {
	fallback := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	for _, value := range []any{"", "   ", "not-a-date", float64(0), float64(-5), nil, true} {
		metadata := map[string]any{MetadataImportedAt: value}
		if got := ResolveImportedAt(metadata, fallback); !got.Equal(fallback) {
			t.Errorf("ResolveImportedAt(%#v) = %v, want the fallback %v", value, got, fallback)
		}
		if _, ok := metadata[MetadataImportedAt].(string); !ok {
			t.Errorf("ResolveImportedAt(%#v) did not reseed metadata", value)
		}
	}
}

func TestResolveImportedAtWithoutMetadata(t *testing.T) {
	fallback := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	if got := ResolveImportedAt(nil, fallback); !got.Equal(fallback) {
		t.Errorf("nil metadata = %v, want %v", got, fallback)
	}
	// A zero fallback still yields a usable timestamp rather than the zero value.
	metadata := map[string]any{}
	if got := ResolveImportedAt(metadata, time.Time{}); got.IsZero() {
		t.Error("zero fallback produced a zero import time")
	}
}
