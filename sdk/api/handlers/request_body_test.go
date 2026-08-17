package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

func TestReadRequestBodyLimitedRejectsRawBodyOverLimit(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader("12345"))
	ctx.Request.Body = http.MaxBytesReader(recorder, ctx.Request.Body, 4)

	if _, errRead := ReadRequestBodyLimited(ctx, 4); errRead == nil {
		t.Fatal("ReadRequestBodyLimited() error = nil")
	}
}

func TestReadRequestBodyLimitedRejectsDecodedZstdBodyOverLimit(t *testing.T) {
	var compressed bytes.Buffer
	encoder, errEncoder := zstd.NewWriter(&compressed)
	if errEncoder != nil {
		t.Fatalf("zstd.NewWriter() error = %v", errEncoder)
	}
	if _, errWrite := encoder.Write([]byte("12345")); errWrite != nil {
		t.Fatalf("encoder.Write() error = %v", errWrite)
	}
	if errClose := encoder.Close(); errClose != nil {
		t.Fatalf("encoder.Close() error = %v", errClose)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(compressed.Bytes()))
	ctx.Request.Header.Set("Content-Encoding", "zstd")

	if _, errRead := ReadRequestBodyLimited(ctx, 4); errRead == nil || !strings.Contains(errRead.Error(), "limit") {
		t.Fatalf("ReadRequestBodyLimited() error = %v", errRead)
	}
}
