package webimage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func (e *Executor) prepareRequirements(ctx context.Context, session *Session, credentials Credentials, state *generationState, deadline time.Time) error {
	if errBudget := e.checkBudget(ctx, deadline, "Sentinel prepare"); errBudget != nil {
		return errBudget
	}
	prepareResponse, errPrepare := e.doJSON(ctx, session, credentials, http.MethodPost, "/backend-api/sentinel/chat-requirements/prepare", []byte(`{}`), nil, "Sentinel prepare")
	if errPrepare != nil {
		return errPrepare
	}
	state.prepareToken = findString(prepareResponse.Body, "prepare_token", "prepareToken")
	if state.prepareToken == "" {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "Sentinel prepare", Msg: "web image Sentinel prepare token missing"}
	}

	turnstile := findMap(prepareResponse.Body, "turnstile")
	if boolValue(turnstile, "required") {
		return &StatusError{Status: http.StatusServiceUnavailable, Kind: ErrorKindChallenge, Stage: "Sentinel prepare", Msg: "web image requires an interactive browser challenge"}
	}

	powMap := findMap(prepareResponse.Body, "proofofwork")
	challenge := PoWChallenge{
		Required:   boolValue(powMap, "required"),
		Seed:       stringValue(powMap, "seed"),
		Difficulty: stringValue(powMap, "difficulty"),
	}
	vector := e.browserVector(session)
	powContext, cancelPoW := context.WithTimeout(ctx, e.duration(e.cfg.WebImagePoWTimeout, config.DefaultWebImagePoWTimeout))
	proofToken, errProof := SolveProof(powContext, challenge, vector, PoWOptions{})
	cancelPoW()
	if errProof != nil {
		if errors.Is(errProof, context.DeadlineExceeded) {
			return &StatusError{Status: http.StatusGatewayTimeout, Kind: ErrorKindTimeout, Stage: "Sentinel proof", Msg: "web image Sentinel proof timed out"}
		}
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "Sentinel proof", Msg: "web image Sentinel proof failed"}
	}
	state.proofToken = proofToken

	if errBudget := e.checkBudget(ctx, deadline, "Sentinel finalize"); errBudget != nil {
		return errBudget
	}
	finalizeBody, _ := json.Marshal(map[string]any{
		"prepare_token":   state.prepareToken,
		"proof_token":     state.proofToken,
		"turnstile_token": state.turnstileToken,
	})
	finalizeResponse, errFinalize := e.doJSON(ctx, session, credentials, http.MethodPost, "/backend-api/sentinel/chat-requirements/finalize", finalizeBody, nil, "Sentinel finalize")
	if errFinalize != nil {
		return errFinalize
	}
	if value := findString(finalizeResponse.Body, "prepare_token", "prepareToken"); value != "" {
		state.prepareToken = value
	}
	if value := findString(finalizeResponse.Body, "proof_token", "proofToken"); value != "" {
		state.proofToken = value
	}
	if value := findString(finalizeResponse.Body, "turnstile_token", "turnstileToken"); value != "" {
		state.turnstileToken = value
	}
	return nil
}

func findMap(value any, key string) map[string]any {
	key = strings.ToLower(strings.TrimSpace(key))
	switch current := value.(type) {
	case map[string]any:
		for currentKey, child := range current {
			if strings.ToLower(currentKey) == key {
				if mapped, ok := child.(map[string]any); ok {
					return mapped
				}
			}
		}
		for _, child := range current {
			if found := findMap(child, key); found != nil {
				return found
			}
		}
	case []any:
		for _, child := range current {
			if found := findMap(child, key); found != nil {
				return found
			}
		}
	}
	return nil
}

func boolValue(values map[string]any, key string) bool {
	if values == nil {
		return false
	}
	value, ok := values[key]
	if !ok {
		return false
	}
	boolean, _ := value.(bool)
	return boolean
}

func stringValue(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}
