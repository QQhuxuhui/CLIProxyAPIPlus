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
				if errContext := ctx.Err(); errContext != nil {
					return errContext
				}
				retryDelay := rateLimitDelay
				if statusError.RetryAfterDelay > retryDelay {
					retryDelay = statusError.RetryAfterDelay
				}
				if remaining := time.Until(pollDeadline); retryDelay >= remaining {
					contextEndsFirst := false
					if contextDeadline, ok := ctx.Deadline(); ok {
						contextEndsFirst = contextDeadline.Before(pollDeadline)
					}
					if !contextEndsFirst {
						return statusError
					}
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
		if state.done && containsPollText(conversationResponse.Body, "free plan limit", "image generation limit", "limit for image generations") {
			// The account's image quota is exhausted for a long window; report it
			// as a 429 with an explicit cooldown so the conductor parks the
			// account instead of re-probing it every transient-error cooldown.
			return &StatusError{Status: http.StatusTooManyRequests, Kind: ErrorKindRateLimit, Stage: "poll", Msg: "web image free plan image limit reached", RetryAfterDelay: e.freeLimitCooldown()}
		}
		if state.done && containsPollText(conversationResponse.Body, "rate limit") {
			return &StatusError{Status: http.StatusTooManyRequests, Kind: ErrorKindRateLimit, Stage: "poll", Msg: "web image upstream rate limit reached"}
		}
		if state.done && len(state.assetRefs) > 0 {
			return nil
		}
		if state.done {
			// The upstream finished the turn without producing an image (content
			// policy refusal or a text-only answer). That outcome is decided by
			// the prompt, so rotating to another credential cannot change it — a
			// client-side (400) request problem, not an upstream (502) fault.
			return &StatusError{Status: http.StatusBadRequest, Kind: ErrorKindModeration, Stage: "poll", Msg: "图片被内容风控拒绝：提示词可能违反内容政策，未生成图片，请调整提示词后重试", Scoped: true}
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
