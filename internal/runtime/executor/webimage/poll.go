package webimage

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func (e *Executor) pollConversation(ctx context.Context, session *Session, credentials Credentials, state *generationState, totalDeadline time.Time) error {
	pollDeadline := time.Now().Add(e.duration(e.cfg.WebImagePollTimeout, config.DefaultWebImagePollTimeout))
	if totalDeadline.Before(pollDeadline) {
		pollDeadline = totalDeadline
	}
	interval := e.duration(e.cfg.WebImagePollInterval, config.DefaultWebImagePollInterval)
	conversationPath := "/backend-api/conversation/" + url.PathEscape(state.conversationID)
	statusPath := conversationPath + "/stream_status"

	for {
		if errBudget := e.checkBudget(ctx, pollDeadline, "poll"); errBudget != nil {
			return errBudget
		}
		statusResponse, errStatus := e.doJSON(ctx, session, credentials, http.MethodGet, statusPath, nil, nil, "stream status")
		if errStatus != nil {
			return errStatus
		}
		status := strings.ToLower(findString(statusResponse.Body, "status", "stream_status"))
		if strings.Contains(status, "fail") || strings.Contains(status, "error") {
			return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindUpstream, Stage: "poll", Msg: "web image generation failed upstream"}
		}

		conversationResponse, errConversation := e.doJSON(ctx, session, credentials, http.MethodGet, conversationPath, nil, http.Header{"X-OAI-Turn-Trace-ID": []string{state.turnTraceID}}, "conversation poll")
		if errConversation != nil {
			return errConversation
		}
		if fileID := findFileID(conversationResponse.Body); fileID != "" {
			state.fileID = fileID
		}
		finished := strings.Contains(status, "finished") || strings.Contains(status, "success") || strings.Contains(status, "complete")
		if finished && state.fileID != "" {
			return nil
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
