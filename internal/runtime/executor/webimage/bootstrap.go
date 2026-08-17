package webimage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxProtocolResponseBytes = 16 * 1024 * 1024

func (e *Executor) bootstrap(ctx context.Context, session *Session, credentials Credentials, state *generationState, deadline time.Time) error {
	if errBudget := e.checkBudget(ctx, deadline, "bootstrap"); errBudget != nil {
		return errBudget
	}
	initResponse, errInit := e.doJSON(ctx, session, credentials, http.MethodPost, "/backend-api/conversation/init", []byte(`{}`), nil, "conversation init")
	if errInit != nil {
		return errInit
	}
	state.conduitToken = strings.TrimSpace(initResponse.Header.Get("X-Conduit-Token"))
	if state.conduitToken == "" {
		state.conduitToken = findString(initResponse.Body, "conduit_token", "conduitToken")
	}

	if errBudget := e.checkBudget(ctx, deadline, "prepare"); errBudget != nil {
		return errBudget
	}
	headers := http.Header{}
	if state.conduitToken != "" {
		headers.Set("X-Conduit-Token", state.conduitToken)
	}
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
		return nil, &StatusError{Status: http.StatusServiceUnavailable, Kind: ErrorKindChallenge, Stage: stage, Msg: "web image upstream transport failed"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, classifyHTTPError(stage, response)
	}
	data, errRead := io.ReadAll(io.LimitReader(response.Body, maxProtocolResponseBytes+1))
	if errRead != nil {
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
		return &StatusError{Status: http.StatusServiceUnavailable, Kind: ErrorKindAuth, Stage: stage, Msg: "web image credential was rejected"}
	}
	if status == http.StatusTooManyRequests {
		return &StatusError{Status: http.StatusServiceUnavailable, Kind: ErrorKindRateLimit, Stage: stage, Msg: "web image upstream rate limit reached"}
	}
	if status == http.StatusBadRequest && (strings.Contains(text, "moderation") || strings.Contains(text, "safety") || strings.Contains(text, "policy")) {
		return &StatusError{Status: http.StatusBadRequest, Kind: ErrorKindModeration, Stage: stage, Msg: "web image prompt was rejected"}
	}
	return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindUpstream, Stage: stage, Msg: "web image upstream returned an error"}
}

func findString(value any, keys ...string) string {
	keySet := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		keySet[strings.ToLower(key)] = struct{}{}
	}
	return findStringRecursive(value, keySet)
}

func findStringRecursive(value any, keys map[string]struct{}) string {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			if _, ok := keys[strings.ToLower(key)]; ok {
				if text, okText := child.(string); okText && strings.TrimSpace(text) != "" {
					return strings.TrimSpace(text)
				}
			}
		}
		for _, child := range current {
			if text := findStringRecursive(child, keys); text != "" {
				return text
			}
		}
	case []any:
		for _, child := range current {
			if text := findStringRecursive(child, keys); text != "" {
				return text
			}
		}
	}
	return ""
}
