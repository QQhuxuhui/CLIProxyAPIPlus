package webimage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

var (
	ErrInvalidChallenge = errors.New("invalid Sentinel proof-of-work challenge")
	ErrPoWExhausted     = errors.New("Sentinel proof-of-work attempts exhausted")
)

const (
	ErrorKindAuth       = "auth"
	ErrorKindChallenge  = "challenge"
	ErrorKindRateLimit  = "rate_limit"
	ErrorKindModeration = "moderation"
	ErrorKindProtocol   = "protocol"
	ErrorKindTimeout    = "timeout"
	ErrorKindOversize   = "oversize"
	ErrorKindUpstream   = "upstream"
)

// StatusError carries a scrubbed failure classification for the Codex bridge.
type StatusError struct {
	Status     int
	Kind       string
	Stage      string
	Msg        string
	RetryAfter time.Duration
	Scoped     bool
}

func (e *StatusError) Error() string {
	if e == nil {
		return "web image request failed"
	}
	if e.Msg != "" {
		return e.Msg
	}
	if e.Stage != "" {
		return fmt.Sprintf("web image %s failed", e.Stage)
	}
	return "web image request failed"
}

func (e *StatusError) StatusCode() int {
	if e == nil || e.Status <= 0 {
		return http.StatusBadGateway
	}
	return e.Status
}

// RequestScoped reports whether this failure is unrelated to credential health.
func (e *StatusError) RequestScoped() bool {
	return e != nil && e.Scoped
}

func preserveContextError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}
