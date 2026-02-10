package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestExtractKiroCacheTokens(t *testing.T) {
	tests := []struct {
		name          string
		tokenUsage    map[string]interface{}
		wantRead      int64
		wantCreation  int64
		wantHasFields bool
	}{
		{
			name: "read and creation present",
			tokenUsage: map[string]interface{}{
				"cacheReadInputTokens":  float64(42),
				"cacheWriteInputTokens": float64(11),
			},
			wantRead:      42,
			wantCreation:  11,
			wantHasFields: true,
		},
		{
			name: "explicit zero read is still present",
			tokenUsage: map[string]interface{}{
				"cacheReadInputTokens": float64(0),
			},
			wantRead:      0,
			wantCreation:  0,
			wantHasFields: true,
		},
		{
			name: "creation only",
			tokenUsage: map[string]interface{}{
				"cacheWriteInputTokens": float64(99),
			},
			wantRead:      0,
			wantCreation:  99,
			wantHasFields: true,
		},
		{
			name:          "no cache fields",
			tokenUsage:    map[string]interface{}{},
			wantRead:      0,
			wantCreation:  0,
			wantHasFields: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			read, creation, hasFields := extractKiroCacheTokens(tt.tokenUsage)
			if read != tt.wantRead {
				t.Fatalf("read = %d, want %d", read, tt.wantRead)
			}
			if creation != tt.wantCreation {
				t.Fatalf("creation = %d, want %d", creation, tt.wantCreation)
			}
			if hasFields != tt.wantHasFields {
				t.Fatalf("hasFields = %v, want %v", hasFields, tt.wantHasFields)
			}
		})
	}
}

func TestShouldSimulateKiroCacheReadTokens(t *testing.T) {
	tests := []struct {
		name                  string
		inputTokens           int64
		cacheReadTokens       int64
		cacheCreationTokens   int64
		hasUpstreamCacheField bool
		want                  bool
	}{
		{
			name:                  "simulate when no upstream cache fields and enough input",
			inputTokens:           3000,
			cacheReadTokens:       0,
			cacheCreationTokens:   0,
			hasUpstreamCacheField: false,
			want:                  true,
		},
		{
			name:                  "do not simulate when upstream explicitly returns cache fields with zero",
			inputTokens:           3000,
			cacheReadTokens:       0,
			cacheCreationTokens:   0,
			hasUpstreamCacheField: true,
			want:                  false,
		},
		{
			name:                  "do not simulate below threshold",
			inputTokens:           500,
			cacheReadTokens:       0,
			cacheCreationTokens:   0,
			hasUpstreamCacheField: false,
			want:                  false,
		},
		{
			name:                  "do not simulate when cache read already present",
			inputTokens:           3000,
			cacheReadTokens:       200,
			cacheCreationTokens:   0,
			hasUpstreamCacheField: false,
			want:                  false,
		},
		{
			name:                  "do not simulate when cache creation already present",
			inputTokens:           3000,
			cacheReadTokens:       0,
			cacheCreationTokens:   150,
			hasUpstreamCacheField: false,
			want:                  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldSimulateKiroCacheReadTokens(tt.inputTokens, tt.cacheReadTokens, tt.cacheCreationTokens, tt.hasUpstreamCacheField)
			if got != tt.want {
				t.Fatalf("shouldSimulate = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyKiroCacheTokens_DoesNotInflateUncachedInputTokens(t *testing.T) {
	detail := usage.Detail{InputTokens: 2500}
	tokenUsage := map[string]interface{}{
		"cacheReadInputTokens":  float64(7000),
		"cacheWriteInputTokens": float64(500),
	}

	hasRead, hasCreation := applyKiroCacheTokens(&detail, tokenUsage)

	if !hasRead {
		t.Fatalf("hasRead = false, want true")
	}
	if !hasCreation {
		t.Fatalf("hasCreation = false, want true")
	}
	if detail.InputTokens != 2500 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 2500)
	}
	if detail.CachedTokens != 7000 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 7000)
	}
	if detail.CacheCreationTokens != 500 {
		t.Fatalf("cache creation tokens = %d, want %d", detail.CacheCreationTokens, 500)
	}
}

func TestApplyKiroCacheTokens_WithReadOnly_DoesNotSetInputTokensFromCache(t *testing.T) {
	detail := usage.Detail{InputTokens: 0}
	tokenUsage := map[string]interface{}{
		"cacheReadInputTokens": float64(7000),
	}

	hasRead, hasCreation := applyKiroCacheTokens(&detail, tokenUsage)

	if !hasRead {
		t.Fatalf("hasRead = false, want true")
	}
	if hasCreation {
		t.Fatalf("hasCreation = true, want false")
	}
	if detail.InputTokens != 0 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 0)
	}
	if detail.CachedTokens != 7000 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 7000)
	}
}
