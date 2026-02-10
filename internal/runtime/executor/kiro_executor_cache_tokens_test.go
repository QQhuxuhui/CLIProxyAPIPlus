package executor

import "testing"

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
