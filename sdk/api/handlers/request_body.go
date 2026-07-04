package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

// ReadRequestBody reads the incoming request body and decodes supported
// Content-Encoding values before handlers inspect JSON fields.
func ReadRequestBody(c *gin.Context) ([]byte, error) {
	raw, err := c.GetRawData()
	if err != nil {
		return nil, err
	}

	encoding := ""
	if c != nil && c.Request != nil {
		encoding = strings.TrimSpace(c.Request.Header.Get("Content-Encoding"))
	}
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return raw, nil
	}

	decoded, err := decodeRequestBody(raw, encoding)
	if err != nil {
		if json.Valid(raw) {
			return raw, nil
		}
		return nil, err
	}
	return decoded, nil
}

func decodeRequestBody(raw []byte, encoding string) ([]byte, error) {
	parts := strings.Split(encoding, ",")
	body := raw
	for i := len(parts) - 1; i >= 0; i-- {
		enc := strings.ToLower(strings.TrimSpace(parts[i]))
		switch enc {
		case "", "identity":
			continue
		case "zstd":
			decoded, err := decodeZstdRequestBody(body)
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

// maxDecodedRequestBodyBytes caps the decompressed size of a single zstd
// request-body stage. Without a cap a few-KB compressed payload could expand
// to gigabytes and OOM the process (a decompression bomb). 128 MiB is generous
// for legitimate JSON/base64-image payloads yet far below memory exhaustion.
// It is a var (not const) so tests can lower it and restore via defer.
var maxDecodedRequestBodyBytes int64 = 128 << 20

func decodeZstdRequestBody(raw []byte) ([]byte, error) {
	decoder, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd request decoder: %w", err)
	}
	defer decoder.Close()

	// Bound the decompressed output: read one byte past the cap so an
	// over-cap stream is detectable, then reject it instead of returning the
	// oversized data.
	decoded, err := io.ReadAll(io.LimitReader(decoder, maxDecodedRequestBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to decode zstd request body: %w", err)
	}
	if int64(len(decoded)) > maxDecodedRequestBodyBytes {
		return nil, fmt.Errorf("decompressed request body exceeds maximum allowed size of %d bytes", maxDecodedRequestBodyBytes)
	}
	return decoded, nil
}
