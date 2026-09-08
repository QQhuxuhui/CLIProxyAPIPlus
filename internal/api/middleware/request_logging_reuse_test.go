package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

// countTempFiles returns the number of files with the given suffix in dir.
func countFilesWithSuffix(t *testing.T, dir, suffix string) int {
	t.Helper()
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("read dir %s: %v", dir, errRead)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), suffix) {
			count++
		}
	}
	return count
}

func TestCaptureRequestInfoStashesRawBodyInContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	payload := []byte(`{"model":"test-model"}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))

	if _, errCapture := captureRequestInfo(c, true); errCapture != nil {
		t.Fatalf("captureRequestInfo: %v", errCapture)
	}

	value, exists := c.Get(logging.CapturedRequestBodyContextKey)
	if !exists {
		t.Fatal("captured request body was not stashed in the gin context")
	}
	raw, ok := value.([]byte)
	if !ok {
		t.Fatalf("stashed request body type = %T, want []byte", value)
	}
	if !bytes.Equal(raw, payload) {
		t.Fatalf("stashed request body = %q, want %q", string(raw), string(payload))
	}
}

func TestCaptureRequestInfoDoesNotStashWhenBodyNotCaptured(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte("{}")))

	if _, errCapture := captureRequestInfo(c, false); errCapture != nil {
		t.Fatalf("captureRequestInfo: %v", errCapture)
	}

	if _, exists := c.Get(logging.CapturedRequestBodyContextKey); exists {
		t.Fatal("request body should not be stashed when capture is disabled")
	}
}

// TestRequestLoggingMiddlewareFinalizesOnPanic verifies that the deferred Finalize releases
// streaming resources even when the handler panics and an outer middleware recovers.
func TestRequestLoggingMiddlewareFinalizesOnPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(true, logsDir, "", 0)

	engine := gin.New()
	// Recovery is registered before the logging middleware, mirroring the server wiring.
	engine.Use(func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()
		c.Next()
	})
	engine.Use(RequestLoggingMiddleware(logger))
	engine.POST("/v1/messages", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Writer.WriteHeader(http.StatusOK)
		if _, errWrite := c.Writer.Write([]byte("data: chunk\n\n")); errWrite != nil {
			t.Errorf("write chunk: %v", errWrite)
		}
		panic("handler exploded")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"stream":true}`)))
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if got := countFilesWithSuffix(t, logsDir, ".tmp"); got != 0 {
		t.Fatalf("temp files left after panic = %d, want 0", got)
	}
	if got := countFilesWithSuffix(t, logsDir, ".log"); got != 1 {
		t.Fatalf("log files after panic = %d, want 1", got)
	}
}

// TestFinalizeReleasesStreamingResourcesWhenLoggerDisabled covers request-log being toggled
// off while a stream is still in flight: temp files must be removed, the async writer must
// exit and no log file may be written.
func TestFinalizeReleasesStreamingResourcesWhenLoggerDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(true, logsDir, "", 0)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"stream":true}`)))

	wrapper := NewResponseWriterWrapper(c.Writer, logger, &RequestInfo{
		URL:    "/v1/messages",
		Method: http.MethodPost,
		Body:   []byte(`{"stream":true}`),
	})
	c.Writer = wrapper

	wrapper.Header().Set("Content-Type", "text/event-stream")
	wrapper.WriteHeader(http.StatusOK)
	if _, errWrite := wrapper.Write([]byte("data: chunk\n\n")); errWrite != nil {
		t.Fatalf("write chunk: %v", errWrite)
	}
	if wrapper.streamWriter == nil {
		t.Fatal("expected a streaming log writer to be created")
	}
	if got := countFilesWithSuffix(t, logsDir, ".tmp"); got == 0 {
		t.Fatal("expected streaming temp files to exist before Finalize")
	}

	// Request logging is turned off while the stream is still in flight.
	logger.SetEnabled(false)

	if errFinalize := wrapper.Finalize(c); errFinalize != nil {
		t.Fatalf("Finalize: %v", errFinalize)
	}

	if wrapper.streamWriter != nil {
		t.Fatal("stream writer was not released")
	}
	if wrapper.chunkChannel != nil {
		t.Fatal("chunk channel was not released")
	}
	if wrapper.streamDone != nil {
		t.Fatal("streaming goroutine was not awaited")
	}
	if got := countFilesWithSuffix(t, logsDir, ".tmp"); got != 0 {
		t.Fatalf("temp files left after Finalize = %d, want 0", got)
	}
	if got := countFilesWithSuffix(t, logsDir, ".log"); got != 0 {
		t.Fatalf("log files written while logging disabled = %d, want 0", got)
	}
	if _, errStat := os.Stat(filepath.Join(logsDir)); errStat != nil {
		t.Fatalf("logs dir stat: %v", errStat)
	}
}

func TestFinalizeIsIdempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(false, logsDir, "", 0)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte("{}")))

	wrapper := NewResponseWriterWrapper(c.Writer, logger, &RequestInfo{URL: "/v1/messages", Method: http.MethodPost})
	if errFinalize := wrapper.Finalize(c); errFinalize != nil {
		t.Fatalf("first Finalize: %v", errFinalize)
	}
	if errFinalize := wrapper.Finalize(c); errFinalize != nil {
		t.Fatalf("second Finalize: %v", errFinalize)
	}
}
