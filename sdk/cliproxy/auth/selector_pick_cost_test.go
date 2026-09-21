package auth

import (
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestLCPPreparedFromMetadataReusesOnlyForSamePayload(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	id := lcpPayloadIdentity("gemini", body)
	if id == "" {
		t.Fatal("payload identity must not be empty for a non-empty body")
	}
	meta := map[string]any{
		cliproxyexecutor.LCPFingerprintMetadataKey:     []string{"a", "b"},
		cliproxyexecutor.LCPMinPrefixLengthMetadataKey: 1,
		lcpPreparedPayloadMetadataKey:                  id,
	}
	fingerprints, minPrefix, ok := lcpPreparedFromMetadata(meta, id)
	if !ok || len(fingerprints) != 2 || minPrefix != 1 {
		t.Fatalf("same payload must reuse prepared fingerprints, got %v %d %v", fingerprints, minPrefix, ok)
	}

	rewritten := append([]byte(nil), body...)
	if _, _, okRewritten := lcpPreparedFromMetadata(meta, lcpPayloadIdentity("gemini", rewritten)); okRewritten {
		t.Fatal("a replaced body slice must force a fresh extraction")
	}
	if _, _, okFormat := lcpPreparedFromMetadata(meta, lcpPayloadIdentity("openai", body)); okFormat {
		t.Fatal("a different source format must force a fresh extraction")
	}
	if _, _, okEmpty := lcpPreparedFromMetadata(meta, lcpPayloadIdentity("gemini", nil)); okEmpty {
		t.Fatal("an empty body must never reuse fingerprints")
	}
	delete(meta, cliproxyexecutor.LCPFingerprintMetadataKey)
	if _, _, okMissing := lcpPreparedFromMetadata(meta, id); okMissing {
		t.Fatal("missing fingerprints must force a fresh extraction")
	}
}

func TestCanonicalModelKeyCacheMatchesComputation(t *testing.T) {
	for _, model := range []string{"", "  ", "gemini-3.1-pro", " gemini-3.1-pro ", "gemini-3.1-pro(high)", "gpt-5.5(8192)", "claude-opus-4-6-thinking"} {
		want := computeCanonicalModelKey(model)
		for i := 0; i < 3; i++ {
			if got := canonicalModelKey(model); got != want {
				t.Fatalf("canonicalModelKey(%q) call %d = %q, want %q", model, i, got, want)
			}
		}
	}
}

func BenchmarkCanonicalModelKey(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = canonicalModelKey("gemini-3.1-pro-low(high)")
	}
}
