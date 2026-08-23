package webimage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxProtocolResponseBytes = 16 * 1024 * 1024

func (e *Executor) bootstrap(ctx context.Context, session *Session, credentials Credentials, state *generationState, deadline time.Time) error {
	if errBudget := e.checkBudget(ctx, deadline, "bootstrap"); errBudget != nil {
		return errBudget
	}
	request, errRequest := newAPIRequest(ctx, session, credentials, http.MethodGet, e.baseURL, "/", nil)
	if errRequest != nil {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "bootstrap", Msg: "web image bootstrap request failed"}
	}
	response, errDo := session.Client.Do(request)
	if errDo != nil {
		if errContext := preserveContextError(errDo); errContext != nil {
			return errContext
		}
		return &StatusError{Status: http.StatusServiceUnavailable, Kind: ErrorKindChallenge, Stage: "bootstrap", Msg: "web image bootstrap transport failed"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return classifyHTTPError("bootstrap", response)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return nil
}

func (e *Executor) prepareConversation(ctx context.Context, session *Session, credentials Credentials, state *generationState, deadline time.Time) error {
	if errBudget := e.checkBudget(ctx, deadline, "conversation prepare"); errBudget != nil {
		return errBudget
	}
	headers := sentinelHeaders(state)
	headers.Set("X-OAI-Turn-Trace-ID", state.turnTraceID)
	prepareBody, errBody := e.buildPrepareBody(state)
	if errBody != nil {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "conversation prepare", Msg: "web image conversation prepare body failed"}
	}
	prepareResponse, errPrepare := e.doJSON(ctx, session, credentials, http.MethodPost, "/backend-api/f/conversation/prepare", prepareBody, headers, "conversation prepare")
	if errPrepare != nil {
		return errPrepare
	}
	if token := strings.TrimSpace(prepareResponse.Header.Get("X-Conduit-Token")); token != "" {
		state.conduitToken = token
	} else if token := findString(prepareResponse.Body, "conduit_token", "conduitToken"); token != "" {
		state.conduitToken = token
	}
	if state.conduitToken == "" {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "conversation prepare", Msg: "web image conversation prepare token missing"}
	}
	state.clientPrepareState = "success"
	return nil
}

type jsonResponse struct {
	Header http.Header
	Body   any
}

func (e *Executor) doJSON(ctx context.Context, session *Session, credentials Credentials, method, path string, body []byte, extraHeaders http.Header, stage string) (*jsonResponse, error) {
	request, errRequest := newAPIRequest(ctx, session, credentials, method, e.baseURL, path, body)
	if errRequest != nil {
		return nil, &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: stage, Msg: "web image request construction failed"}
	}
	for name, values := range extraHeaders {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, errDo := session.Client.Do(request)
	if errDo != nil {
		if errContext := preserveContextError(errDo); errContext != nil {
			return nil, errContext
		}
		return nil, &StatusError{Status: http.StatusServiceUnavailable, Kind: ErrorKindChallenge, Stage: stage, Msg: "web image upstream transport failed"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, classifyHTTPError(stage, response)
	}
	data, errRead := io.ReadAll(io.LimitReader(response.Body, maxProtocolResponseBytes+1))
	if errRead != nil {
		if errContext := preserveContextError(errRead); errContext != nil {
			return nil, errContext
		}
		return nil, &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: stage, Msg: "web image upstream response read failed"}
	}
	if len(data) > maxProtocolResponseBytes {
		return nil, &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindOversize, Stage: stage, Msg: "web image upstream response exceeded limit"}
	}
	var decoded any = map[string]any{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if errDecode := json.Unmarshal(data, &decoded); errDecode != nil {
			return nil, &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: stage, Msg: "web image upstream JSON schema changed"}
		}
	}
	return &jsonResponse{Header: response.Header.Clone(), Body: decoded}, nil
}

func classifyHTTPError(stage string, response *http.Response) error {
	status := response.StatusCode
	data, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	text := strings.ToLower(string(data))
	server := strings.ToLower(response.Header.Get("Server"))
	if status == http.StatusForbidden && (strings.Contains(server, "cloudflare") || strings.Contains(text, "cf-chl") || strings.Contains(text, "cloudflare")) {
		return &StatusError{Status: http.StatusServiceUnavailable, Kind: ErrorKindChallenge, Stage: stage, Msg: "web image browser challenge rejected"}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// Surface the real auth status so the conductor treats the failure as a
		// credential problem (cooldown + consecutive-failure escalation) instead
		// of a transient upstream error that keeps the account in rotation.
		msg := "web image credential was rejected"
		if code := upstreamErrorCode(text); code != "" {
			msg += " (" + code + ")"
		}
		return &StatusError{Status: status, Kind: ErrorKindAuth, Stage: stage, Msg: msg}
	}
	if status == http.StatusTooManyRequests {
		return &StatusError{Status: http.StatusTooManyRequests, Kind: ErrorKindRateLimit, Stage: stage, Msg: "web image upstream rate limit reached", RetryAfterDelay: retryAfterDuration(response.Header.Get("Retry-After"))}
	}
	if status == http.StatusBadRequest && (strings.Contains(text, "moderation") || strings.Contains(text, "safety") || strings.Contains(text, "policy")) {
		return &StatusError{Status: http.StatusBadRequest, Kind: ErrorKindModeration, Stage: stage, Msg: "web image prompt was rejected"}
	}
	return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindUpstream, Stage: stage, Msg: "web image upstream returned an error"}
}

var upstreamErrorCodePattern = regexp.MustCompile(`"code"\s*:\s*"([a-z0-9_]{1,64})"`)

// upstreamErrorCode extracts the short machine readable error code (for example
// token_invalidated) from an upstream JSON error body without leaking the body.
func upstreamErrorCode(lowerBody string) string {
	match := upstreamErrorCodePattern.FindStringSubmatch(lowerBody)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func retryAfterDuration(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, errParse := strconv.ParseFloat(raw, 64); errParse == nil && seconds > 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	if parsed, errDate := http.ParseTime(raw); errDate == nil {
		if delay := time.Until(parsed); delay > 0 {
			return delay
		}
	}
	return 0
}

func findString(value any, keys ...string) string {
	values := findStrings(value, keys...)
	if len(values) > 0 {
		return values[0]
	}
	return ""
}

func findStrings(value any, keys ...string) []string {
	keySet := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		keySet[strings.ToLower(key)] = struct{}{}
	}
	values := make([]string, 0, 4)
	findStringsRecursive(value, keySet, &values)
	return values
}

func findStringsRecursive(value any, keys map[string]struct{}, values *[]string) {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			if _, ok := keys[strings.ToLower(key)]; ok {
				if text, okText := child.(string); okText && strings.TrimSpace(text) != "" {
					*values = append(*values, strings.TrimSpace(text))
				}
			}
		}
		for _, child := range current {
			findStringsRecursive(child, keys, values)
		}
	case []any:
		for _, child := range current {
			findStringsRecursive(child, keys, values)
		}
	}
}
