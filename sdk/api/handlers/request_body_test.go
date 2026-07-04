package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

// zstdCompress returns a zstd-compressed copy of data.
func zstdCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("new zstd writer: %v", err)
	}
	if _, err = w.Write(data); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return buf.Bytes()
}

func zstdRequestContext(t *testing.T, body []byte) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Encoding", "zstd")
	return c
}

// TestReadRequestBody_ZstdNormalBody confirms a small, legitimate zstd body
// still decodes back to its original bytes.
func TestReadRequestBody_ZstdNormalBody(t *testing.T) {
	original := []byte(`{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`)
	c := zstdRequestContext(t, zstdCompress(t, original))

	got, err := ReadRequestBody(c)
	if err != nil {
		t.Fatalf("ReadRequestBody: unexpected error %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("decoded body = %q, want %q", got, original)
	}
}

// TestReadRequestBody_ZstdBombRejected asserts a zstd stream that decompresses
// past the cap is rejected with an error and the oversized output is NOT
// returned. It lowers the package cap so the test buffer stays modest, and
// restores it via defer. If the io.LimitReader bound is reverted (unbounded
// io.ReadAll), ReadRequestBody would return the full oversized body with no
// error, and both assertions below would fail — pinning the fix.
func TestReadRequestBody_ZstdBombRejected(t *testing.T) {
	const testCap = 1 << 20 // 1 MiB
	original := maxDecodedRequestBodyBytes
	maxDecodedRequestBodyBytes = testCap
	defer func() { maxDecodedRequestBodyBytes = original }()

	// 4 MiB of zeros compresses to a few hundred bytes but expands far past
	// the lowered cap — a decompression bomb in miniature.
	bomb := zstdCompress(t, make([]byte, 4<<20))
	if len(bomb) >= testCap {
		t.Fatalf("compressed bomb %d bytes is not smaller than cap %d", len(bomb), testCap)
	}
	c := zstdRequestContext(t, bomb)

	got, err := ReadRequestBody(c)
	if err == nil {
		t.Fatalf("expected error for over-cap zstd body, got nil (returned %d bytes)", len(got))
	}
	if int64(len(got)) > testCap {
		t.Fatalf("oversized decompressed body leaked: returned %d bytes, cap %d", len(got), testCap)
	}
}
