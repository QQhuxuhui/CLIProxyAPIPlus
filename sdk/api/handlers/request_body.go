package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

// RawRequestBody returns the raw request body without any Content-Encoding decoding.
// When the request logging middleware already read the body, the captured bytes are
// reused so the payload is not held twice in memory. Otherwise it falls back to the
// standard gin body read.
//
// The returned slice may be shared with the request logger; callers must not modify
// it in place.
func RawRequestBody(c *gin.Context) ([]byte, error) {
	if raw, ok := capturedRequestBody(c); ok {
		return raw, nil
	}
	return c.GetRawData()
}

// capturedRequestBody returns the request body bytes stashed by the request logging
// middleware, if that middleware is installed and captured the body for this request.
func capturedRequestBody(c *gin.Context) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	value, exists := c.Get(logging.CapturedRequestBodyContextKey)
	if !exists {
		return nil, false
	}
	raw, ok := value.([]byte)
	if !ok {
		return nil, false
	}
	return raw, true
}

// ReadRequestBody reads the incoming request body and decodes supported
// Content-Encoding values before handlers inspect JSON fields.
func ReadRequestBody(c *gin.Context) ([]byte, error) {
	return ReadRequestBodyLimited(c, 0)
}

// ReadRequestBodyLimited reads and decodes a request while bounding the decoded body.
// The returned slice may be shared with the request logger; callers must not modify it in place.
func ReadRequestBodyLimited(c *gin.Context, maxBytes int64) ([]byte, error) {
	raw, err := RawRequestBody(c)
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 && int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("request body exceeds the configured byte limit")
	}

	encoding := ""
	if c != nil && c.Request != nil {
		encoding = strings.TrimSpace(c.Request.Header.Get("Content-Encoding"))
	}
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return raw, nil
	}

	decoded, err := decodeRequestBodyLimited(raw, encoding, maxBytes)
	if err != nil {
		if json.Valid(raw) {
			return raw, nil
		}
		return nil, err
	}
	return decoded, nil
}

func decodeRequestBody(raw []byte, encoding string) ([]byte, error) {
	return decodeRequestBodyLimited(raw, encoding, 0)
}

func decodeRequestBodyLimited(raw []byte, encoding string, maxBytes int64) ([]byte, error) {
	parts := strings.Split(encoding, ",")
	body := raw
	for i := len(parts) - 1; i >= 0; i-- {
		enc := strings.ToLower(strings.TrimSpace(parts[i]))
		switch enc {
		case "", "identity":
			continue
		case "zstd":
			decoded, err := decodeZstdRequestBodyLimited(body, maxBytes)
			if err != nil {
				return nil, err
			}
			body = decoded
		default:
			return nil, fmt.Errorf("unsupported request content encoding: %s", enc)
		}
	}
	return body, nil
}

func decodeZstdRequestBody(raw []byte) ([]byte, error) {
	return decodeZstdRequestBodyLimited(raw, 0)
}

func decodeZstdRequestBodyLimited(raw []byte, maxBytes int64) ([]byte, error) {
	decoder, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd request decoder: %w", err)
	}
	defer decoder.Close()

	reader := io.Reader(decoder)
	if maxBytes > 0 {
		reader = io.LimitReader(decoder, maxBytes+1)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to decode zstd request body: %w", err)
	}
	if maxBytes > 0 && int64(len(decoded)) > maxBytes {
		return nil, fmt.Errorf("decoded request body exceeds the configured byte limit")
	}
	return decoded, nil
}
