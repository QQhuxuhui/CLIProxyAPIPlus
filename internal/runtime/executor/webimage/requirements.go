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
	tokenContext, cancelToken := context.WithTimeout(ctx, e.duration(e.cfg.WebImagePoWTimeout, config.DefaultWebImagePoWTimeout))
	requirementsToken, errToken := BuildLegacyRequirementsToken(tokenContext, session.UserAgent, PoWOptions{})
	cancelToken()
	if errToken != nil {
		if errors.Is(errToken, context.Canceled) {
			return errToken
		}
		if errors.Is(errToken, context.DeadlineExceeded) {
			return &StatusError{Status: http.StatusGatewayTimeout, Kind: ErrorKindTimeout, Stage: "Sentinel prepare", Msg: "web image Sentinel requirements token timed out"}
		}
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "Sentinel prepare", Msg: "web image Sentinel requirements token failed"}
	}
	prepareBody, errBody := json.Marshal(map[string]any{"p": requirementsToken})
	if errBody != nil {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "Sentinel prepare", Msg: "web image Sentinel prepare body failed"}
	}
	prepareResponse, errPrepare := e.doJSON(ctx, session, credentials, http.MethodPost, "/backend-api/sentinel/chat-requirements/prepare", prepareBody, nil, "Sentinel prepare")
	if errPrepare != nil {
		return errPrepare
	}
	state.prepareToken = findString(prepareResponse.Body, "prepare_token", "prepareToken")
	if state.prepareToken == "" {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "Sentinel prepare", Msg: "web image Sentinel prepare token missing"}
	}

	turnstile := findMap(prepareResponse.Body, "turnstile")
	if boolValue(turnstile, "required") {
		errLegacy := e.prepareLegacyRequirements(ctx, session, credentials, state, deadline, prepareBody)
		if errLegacy == nil {
			return nil
		}
		if errors.Is(errLegacy, context.Canceled) || errors.Is(errLegacy, context.DeadlineExceeded) {
			return errLegacy
		}
		var statusError *StatusError
		if errors.As(errLegacy, &statusError) {
			switch statusError.Kind {
			case ErrorKindAuth, ErrorKindRateLimit, ErrorKindTimeout, ErrorKindChallenge:
				return errLegacy
			}
		}
		return &StatusError{Status: http.StatusServiceUnavailable, Kind: ErrorKindChallenge, Stage: "Sentinel prepare", Msg: "web image requires an interactive browser challenge"}
	}

	powMap := findMap(prepareResponse.Body, "proofofwork")
	challenge := PoWChallenge{
		Required:   boolValue(powMap, "required"),
		Seed:       stringValue(powMap, "seed"),
		Difficulty: stringValue(powMap, "difficulty"),
	}
	if challenge.Required {
		powContext, cancelPoW := context.WithTimeout(ctx, e.duration(e.cfg.WebImagePoWTimeout, config.DefaultWebImagePoWTimeout))
		proofToken, errProof := SolveLegacyProof(powContext, challenge, session.UserAgent, PoWOptions{})
		cancelPoW()
		if errProof != nil {
			if errors.Is(errProof, context.Canceled) {
				return errProof
			}
			if errors.Is(errProof, context.DeadlineExceeded) {
				return &StatusError{Status: http.StatusGatewayTimeout, Kind: ErrorKindTimeout, Stage: "Sentinel proof", Msg: "web image Sentinel proof timed out"}
			}
			return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "Sentinel proof", Msg: "web image Sentinel proof failed"}
		}
		state.proofToken = proofToken
	}

	if errBudget := e.checkBudget(ctx, deadline, "Sentinel finalize"); errBudget != nil {
		return errBudget
	}
	finalizePayload := map[string]any{"prepare_token": state.prepareToken}
	if state.proofToken != "" {
		finalizePayload["proofofwork"] = state.proofToken
	}
	if state.turnstileToken != "" {
		finalizePayload["turnstile"] = state.turnstileToken
	}
	finalizeBody, _ := json.Marshal(finalizePayload)
	finalizeResponse, errFinalize := e.doJSON(ctx, session, credentials, http.MethodPost, "/backend-api/sentinel/chat-requirements/finalize", finalizeBody, nil, "Sentinel finalize")
	if errFinalize != nil {
		return errFinalize
	}
	if body, ok := finalizeResponse.Body.(map[string]any); ok {
		state.chatRequirementsToken = stringValue(body, "token")
	}
	if state.chatRequirementsToken != "" {
		return nil
	}
	legacyPrepareToken := findString(finalizeResponse.Body, "prepare_token", "prepareToken")
	legacyProofToken := findString(finalizeResponse.Body, "proof_token", "proofToken")
	legacyTurnstileToken := findString(finalizeResponse.Body, "turnstile_token", "turnstileToken")
	if legacyPrepareToken != "" {
		state.prepareToken = legacyPrepareToken
	}
	if legacyProofToken != "" {
		state.proofToken = legacyProofToken
	}
	if legacyTurnstileToken != "" {
		state.turnstileToken = legacyTurnstileToken
	}
	if legacyPrepareToken != "" && legacyProofToken != "" {
		return nil
	}
	return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "Sentinel finalize", Msg: "web image Sentinel requirements token missing"}
}

func (e *Executor) prepareLegacyRequirements(ctx context.Context, session *Session, credentials Credentials, state *generationState, deadline time.Time, body []byte) error {
	if errBudget := e.checkBudget(ctx, deadline, "Sentinel requirements"); errBudget != nil {
		return errBudget
	}
	response, errRequirements := e.doJSON(ctx, session, credentials, http.MethodPost, "/backend-api/sentinel/chat-requirements", body, nil, "Sentinel requirements")
	if errRequirements != nil {
		return errRequirements
	}
	if boolValue(findMap(response.Body, "arkose"), "required") {
		return &StatusError{Status: http.StatusServiceUnavailable, Kind: ErrorKindChallenge, Stage: "Sentinel requirements", Msg: "web image requires an interactive browser challenge"}
	}

	requirementsToken := findString(response.Body, "token", "chat_token", "chat_requirements_token", "requirements_token")
	if requirementsToken == "" {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "Sentinel requirements", Msg: "web image Sentinel requirements token missing"}
	}
	powMap := findMap(response.Body, "proofofwork")
	if powMap == nil {
		powMap = findMap(response.Body, "proof_of_work")
	}
	challenge := PoWChallenge{
		Required:   boolValue(powMap, "required"),
		Seed:       stringValue(powMap, "seed"),
		Difficulty: stringValue(powMap, "difficulty"),
	}
	if challenge.Required {
		powContext, cancelPoW := context.WithTimeout(ctx, e.duration(e.cfg.WebImagePoWTimeout, config.DefaultWebImagePoWTimeout))
		proofToken, errProof := SolveLegacyProof(powContext, challenge, session.UserAgent, PoWOptions{})
		cancelPoW()
		if errProof != nil {
			if errors.Is(errProof, context.Canceled) {
				return errProof
			}
			if errors.Is(errProof, context.DeadlineExceeded) {
				return &StatusError{Status: http.StatusGatewayTimeout, Kind: ErrorKindTimeout, Stage: "Sentinel proof", Msg: "web image Sentinel proof timed out"}
			}
			return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "Sentinel proof", Msg: "web image Sentinel proof failed"}
		}
		state.proofToken = proofToken
	}
	state.chatRequirementsToken = requirementsToken
	state.legacyRequirements = true
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
