package webimage

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func (e *Executor) pollConversation(ctx context.Context, session *Session, credentials Credentials, state *generationState, totalDeadline time.Time) error {
	if hasDirectAsset(state.assetRefs) {
		return nil
	}
	pollDeadline := time.Now().Add(e.duration(e.cfg.WebImagePollTimeout, config.DefaultWebImagePollTimeout))
	if totalDeadline.Before(pollDeadline) {
		pollDeadline = totalDeadline
	}
	interval := e.duration(e.cfg.WebImagePollInterval, config.DefaultWebImagePollInterval)
	rateLimitDelay := interval
	if rateLimitDelay < 3*time.Second {
		rateLimitDelay = 3 * time.Second
	}
	lastAssetChange := time.Now()
	previousAssetCount := len(state.assetRefs)
	conversationPath := "/backend-api/conversation/" + url.PathEscape(state.conversationID)

	for {
		if errBudget := e.checkBudget(ctx, pollDeadline, "poll"); errBudget != nil {
			return errBudget
		}
		if hasDirectAsset(state.assetRefs) {
			return nil
		}
		if len(state.assetRefs) > 0 && (state.done || time.Since(lastAssetChange) >= 18*time.Second) {
			return nil
		}
		if state.failed && len(state.assetRefs) == 0 {
			return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindUpstream, Stage: "poll", Msg: "web image generation failed upstream"}
		}
		headers := sentinelHeaders(state)
		headers.Set("X-OAI-Turn-Trace-ID", state.turnTraceID)
		conversationResponse, errConversation := e.doJSON(ctx, session, credentials, http.MethodGet, conversationPath, nil, headers, "conversation poll")
		if errConversation != nil {
			var statusError *StatusError
			if errors.As(errConversation, &statusError) && statusError.Kind == ErrorKindRateLimit {
				retryDelay := rateLimitDelay
				if statusError.RetryAfter > retryDelay {
					retryDelay = statusError.RetryAfter
				}
				if remaining := time.Until(pollDeadline); retryDelay > remaining {
					retryDelay = remaining
				}
				if errWait := waitPoll(ctx, retryDelay); errWait != nil {
					return errWait
				}
				rateLimitDelay *= 2
				if rateLimitDelay > 30*time.Second {
					rateLimitDelay = 30 * time.Second
				}
				continue
			}
			return errConversation
		}
		rateLimitDelay = interval
		if rateLimitDelay < 3*time.Second {
			rateLimitDelay = 3 * time.Second
		}
		mergeGenerationState(state, conversationResponse.Body)
		if len(state.assetRefs) != previousAssetCount {
			previousAssetCount = len(state.assetRefs)
			lastAssetChange = time.Now()
		}
		if hasDirectAsset(state.assetRefs) {
			return nil
		}
		if state.done && containsPollText(conversationResponse.Body, "free plan limit", "image generation limit", "limit for image generations", "rate limit") {
			return &StatusError{Status: http.StatusServiceUnavailable, Kind: ErrorKindRateLimit, Stage: "poll", Msg: "web image upstream rate limit reached"}
		}
		if state.done && len(state.assetRefs) > 0 {
			return nil
		}
		if state.done {
			return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "poll", Msg: "web image generation completed without an asset"}
		}
		if state.failed {
			return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindUpstream, Stage: "poll", Msg: "web image generation failed upstream"}
		}

		if errWait := waitPoll(ctx, interval); errWait != nil {
			return errWait
		}
	}
}

func containsPollText(value any, markers ...string) bool {
	switch current := value.(type) {
	case string:
		lower := strings.ToLower(current)
		for _, marker := range markers {
			if strings.Contains(lower, marker) {
				return true
			}
		}
	case map[string]any:
		for _, child := range current {
			if containsPollText(child, markers...) {
				return true
			}
		}
	case []any:
		for _, child := range current {
			if containsPollText(child, markers...) {
				return true
			}
		}
	}
	return false
}

func waitPoll(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
