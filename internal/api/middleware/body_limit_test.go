package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newEchoEngine(t *testing.T, limit int64) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(BodySizeLimit(limit))
	engine.POST("/echo", func(c *gin.Context) {
		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.String(http.StatusRequestEntityTooLarge, "read error: %v", err)
			return
		}
		c.String(http.StatusOK, "ok:%d", len(data))
	})
	return engine
}

func TestBodySizeLimitRejectsOversizedBody(t *testing.T) {
	engine := newEchoEngine(t, 8)

	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader("this body is definitely more than 8 bytes"))
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusRequestEntityTooLarge, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "request body too large") {
		t.Fatalf("expected request-body-too-large error, got %q", rr.Body.String())
	}
}

func TestBodySizeLimitAllowsBodyWithinLimit(t *testing.T) {
	engine := newEchoEngine(t, 1024)

	body := bytes.Repeat([]byte("a"), 100)
	req := httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if rr.Body.String() != "ok:100" {
		t.Fatalf("body = %q, want ok:100", rr.Body.String())
	}
}

func TestBodySizeLimitDisabledWhenNonPositive(t *testing.T) {
	engine := newEchoEngine(t, 0)

	body := bytes.Repeat([]byte("a"), 10000)
	req := httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (limit disabled); body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
}
