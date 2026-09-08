package handlers

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

// countingReader records how many times the underlying request body was read.
type countingReader struct {
	reader io.Reader
	reads  int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.reads++
	}
	return n, err
}

func (r *countingReader) Close() error { return nil }

func newTestContext(t *testing.T, body []byte) (*gin.Context, *countingReader) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	counter := &countingReader{reader: bytes.NewReader(body)}
	c.Request.Body = counter
	return c, counter
}

func zstdCompress(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	encoder, errNewWriter := zstd.NewWriter(&buf)
	if errNewWriter != nil {
		t.Fatalf("zstd.NewWriter: %v", errNewWriter)
	}
	if _, errWrite := encoder.Write(payload); errWrite != nil {
		t.Fatalf("zstd write: %v", errWrite)
	}
	if errClose := encoder.Close(); errClose != nil {
		t.Fatalf("zstd close: %v", errClose)
	}
	return buf.Bytes()
}

func TestRawRequestBodyReusesCapturedBytes(t *testing.T) {
	payload := []byte(`{"model":"test-model"}`)
	c, counter := newTestContext(t, payload)
	c.Set(logging.CapturedRequestBodyContextKey, payload)

	raw, errRead := RawRequestBody(c)
	if errRead != nil {
		t.Fatalf("RawRequestBody: %v", errRead)
	}
	if !bytes.Equal(raw, payload) {
		t.Fatalf("raw body = %q, want %q", string(raw), string(payload))
	}
	if counter.reads != 0 {
		t.Fatalf("request body reads = %d, want 0", counter.reads)
	}
}

func TestRawRequestBodyFallsBackWithoutMiddleware(t *testing.T) {
	payload := []byte(`{"model":"test-model"}`)
	c, counter := newTestContext(t, payload)

	raw, errRead := RawRequestBody(c)
	if errRead != nil {
		t.Fatalf("RawRequestBody: %v", errRead)
	}
	if !bytes.Equal(raw, payload) {
		t.Fatalf("raw body = %q, want %q", string(raw), string(payload))
	}
	if counter.reads == 0 {
		t.Fatal("expected the request body to be read when no captured bytes exist")
	}
}

func TestReadRequestBodyReusesCapturedBytes(t *testing.T) {
	payload := []byte(`{"model":"test-model"}`)
	c, counter := newTestContext(t, payload)
	c.Set(logging.CapturedRequestBodyContextKey, payload)

	raw, errRead := ReadRequestBody(c)
	if errRead != nil {
		t.Fatalf("ReadRequestBody: %v", errRead)
	}
	if !bytes.Equal(raw, payload) {
		t.Fatalf("body = %q, want %q", string(raw), string(payload))
	}
	if counter.reads != 0 {
		t.Fatalf("request body reads = %d, want 0", counter.reads)
	}
}

func TestReadRequestBodyDecodesCapturedZstdBytes(t *testing.T) {
	payload := []byte(`{"model":"test-model","stream":true}`)
	compressed := zstdCompress(t, payload)

	c, counter := newTestContext(t, compressed)
	c.Request.Header.Set("Content-Encoding", "zstd")
	c.Set(logging.CapturedRequestBodyContextKey, compressed)

	decoded, errRead := ReadRequestBody(c)
	if errRead != nil {
		t.Fatalf("ReadRequestBody: %v", errRead)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("decoded body = %q, want %q", string(decoded), string(payload))
	}
	if counter.reads != 0 {
		t.Fatalf("request body reads = %d, want 0", counter.reads)
	}
}

func TestReadRequestBodyLimitedEnforcesLimitOnCapturedBytes(t *testing.T) {
	payload := []byte(`{"model":"test-model"}`)
	c, _ := newTestContext(t, payload)
	c.Set(logging.CapturedRequestBodyContextKey, payload)

	if _, errRead := ReadRequestBodyLimited(c, 4); errRead == nil {
		t.Fatal("ReadRequestBodyLimited() error = nil, want limit error")
	}
}

func TestReadRequestBodyLimitedEnforcesLimitOnCapturedZstdBytes(t *testing.T) {
	payload := []byte(strings.Repeat("a", 4096))
	compressed := zstdCompress(t, payload)

	c, _ := newTestContext(t, compressed)
	c.Request.Header.Set("Content-Encoding", "zstd")
	c.Set(logging.CapturedRequestBodyContextKey, compressed)

	if _, errRead := ReadRequestBodyLimited(c, 8); errRead == nil || !strings.Contains(errRead.Error(), "limit") {
		t.Fatalf("ReadRequestBodyLimited() error = %v, want limit error", errRead)
	}
}

// TestRequestLoggingMiddlewareSharesBodyWithHandler wires the real middleware in front of a
// handler and asserts the handler receives the captured bytes without reading the body again.
func TestRequestLoggingMiddlewareSharesBodyWithHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	payload := []byte(`{"model":"test-model"}`)
	logger := logging.NewFileRequestLogger(false, t.TempDir(), "", 0)

	var (
		handlerBody []byte
		counter     *countingReader
	)

	engine := gin.New()
	engine.Use(middleware.RequestLoggingMiddleware(logger))
	engine.POST("/v1/messages", func(c *gin.Context) {
		// Wrap the restored body so any second read is observable.
		counter = &countingReader{reader: c.Request.Body}
		c.Request.Body = counter

		raw, errRead := RawRequestBody(c)
		if errRead != nil {
			t.Errorf("RawRequestBody: %v", errRead)
		}
		handlerBody = raw
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if !bytes.Equal(handlerBody, payload) {
		t.Fatalf("handler body = %q, want %q", string(handlerBody), string(payload))
	}
	if counter == nil {
		t.Fatal("handler was not invoked")
	}
	if counter.reads != 0 {
		t.Fatalf("handler re-read the request body %d time(s), want 0", counter.reads)
	}
}

// TestHandlerFallsBackWhenMiddlewareNotInstalled keeps the commercial/no-middleware path honest.
func TestHandlerFallsBackWhenMiddlewareNotInstalled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	payload := []byte(`{"model":"test-model"}`)

	var handlerBody []byte
	engine := gin.New()
	engine.POST("/v1/messages", func(c *gin.Context) {
		raw, errRead := RawRequestBody(c)
		if errRead != nil {
			t.Errorf("RawRequestBody: %v", errRead)
		}
		handlerBody = raw
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if !bytes.Equal(handlerBody, payload) {
		t.Fatalf("handler body = %q, want %q", string(handlerBody), string(payload))
	}
}
