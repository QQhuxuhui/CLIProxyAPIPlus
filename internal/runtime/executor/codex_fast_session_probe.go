package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var codexFastSessionProbeBody = []byte(`{"instructions":"","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Reply with OK."}]}],"stream":true,"store":false,"reasoning":{"effort":"low"}}`)

// ProbeFastSession sends one tiny streaming /responses request with sessionID as the upstream
// Session-Id (and body prompt_cache_key) and returns the times from sending the request to the
// first response.created event and to the first response.output_text.delta. It uses the regular Codex headers and transport but
// bypasses the fast-session mapping, usage reporting, and request logging. No timeout is set:
// the request ends at the first output delta (the probe context is then canceled), at the end
// of the stream, or when ctx is canceled.
func (e *CodexExecutor) ProbeFastSession(ctx context.Context, auth *cliproxyauth.Auth, model, sessionID string) (helps.CodexFastSessionProbeTiming, error) {
	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}
	body, _ := sjson.SetBytes(bytes.Clone(codexFastSessionProbeBody), "model", model)
	body, _ = sjson.SetBytes(body, "prompt_cache_key", sessionID)

	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(probeCtx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+"/responses", bytes.NewReader(body))
	var timing helps.CodexFastSessionProbeTiming
	if err != nil {
		return timing, err
	}
	applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg, http.Header{})
	applyModelHeaderOverrides(httpReq.Header, model, e.cfg)
	setCodexSessionHeaderCasePreserved(httpReq.Header, "Session-Id", sessionID)

	start := time.Now()
	httpResp, err := helps.NewUtlsHTTPClient(probeCtx, e.cfg, auth, 0).Do(httpReq)
	if err != nil {
		return timing, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Debugf("codex fast session probe: close response body error: %v", errClose)
		}
	}()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(httpResp.Body, 2048))
		return timing, fmt.Errorf("status %d: %w", httpResp.StatusCode, newCodexStatusErrWithHeaders(httpResp.StatusCode, data, httpResp.Header, e.modelLevelCooling()))
	}
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}
		eventData := bytes.TrimSpace(line[len(dataTag):])
		switch eventType := gjson.GetBytes(eventData, "type").String(); eventType {
		case "response.created":
			if timing.Created == 0 {
				timing.Created = time.Since(start)
			}
		case "response.output_text.delta":
			timing.First = time.Since(start)
			return timing, nil
		case "response.failed", "response.incomplete", "error":
			if streamErr, _, ok := codexTerminalFailureErrWithCooling(eventData, e.modelLevelCooling()); ok {
				return timing, streamErr
			}
			return timing, fmt.Errorf("upstream %s", eventType)
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return timing, errScan
	}
	return timing, errors.New("stream ended without output text")
}
