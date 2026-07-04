// Package middleware provides shared Gin middleware for the HTTP API server.
package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// BodySizeLimit returns a Gin middleware that wraps the incoming request body with
// http.MaxBytesReader so reads beyond maxBytes fail fast instead of allowing an
// unbounded amount of client-supplied data to be buffered into memory. maxBytes <= 0
// disables the cap (the request body is left untouched).
func BodySizeLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if maxBytes > 0 && c.Request != nil && c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		}
		c.Next()
	}
}
