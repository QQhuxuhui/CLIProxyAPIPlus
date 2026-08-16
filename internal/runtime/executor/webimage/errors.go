package webimage

import (
	"errors"
	"fmt"
	"net/http"
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
	Status int
	Kind   string
	Stage  string
	Msg    string
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
