package webimage

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (e *Executor) startConversation(ctx context.Context, session *Session, credentials Credentials, state *generationState, deadline time.Time) error {
	if errBudget := e.checkBudget(ctx, deadline, "conversation"); errBudget != nil {
		return errBudget
	}
	body, errBody := e.buildConversationBody(state)
	if errBody != nil {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "conversation", Msg: "web image conversation body failed"}
	}
	request, errRequest := newAPIRequest(ctx, session, credentials, http.MethodPost, e.baseURL, "/backend-api/f/conversation", body)
	if errRequest != nil {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "conversation", Msg: "web image conversation request failed"}
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("X-OAI-Turn-Trace-ID", state.turnTraceID)
	request.Header.Set("OAI-Echo-Logs", "false")
	request.Header.Set("OAI-Telemetry", "{}")
	if state.conduitToken != "" {
		request.Header.Set("X-Conduit-Token", state.conduitToken)
	}
	request.Header.Set("OpenAI-Sentinel-Chat-Requirements-Prepare-Token", state.prepareToken)
	request.Header.Set("OpenAI-Sentinel-Proof-Token", state.proofToken)
	request.Header.Set("OpenAI-Sentinel-Turnstile-Token", state.turnstileToken)

	response, errDo := session.Client.Do(request)
	if errDo != nil {
		return &StatusError{Status: http.StatusServiceUnavailable, Kind: ErrorKindChallenge, Stage: "conversation", Msg: "web image conversation transport failed"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return classifyHTTPError("conversation", response)
	}
	if token := strings.TrimSpace(response.Header.Get("X-Conduit-Token")); token != "" {
		state.conduitToken = token
	}
	return parseConversationSSE(response.Body, state)
}

func (e *Executor) buildConversationBody(state *generationState) ([]byte, error) {
	now := time.Now()
	messageID := uuid.NewString()
	parentMessageID := uuid.NewString()
	baseModel := strings.TrimSpace(e.cfg.WebImageBaseModel)
	body := map[string]any{
		"action": "next",
		"messages": []any{
			map[string]any{
				"id":          messageID,
				"author":      map[string]any{"role": "user"},
				"create_time": float64(now.UnixMilli()) / 1000,
				"content":     map[string]any{"content_type": "text", "parts": []any{state.prompt}},
				"metadata": map[string]any{
					"serialization_metadata": map[string]any{"custom_symbol_offsets": []any{}},
					"system_hints":           []any{},
				},
			},
		},
		"parent_message_id":                    parentMessageID,
		"model":                                baseModel,
		"timezone":                             "Asia/Shanghai",
		"timezone_offset_min":                  -480,
		"conversation_mode":                    map[string]any{"kind": "primary_assistant"},
		"client_prepare_state":                 state.clientPrepareState,
		"force_parallel_switch":                "auto",
		"enable_message_followups":             true,
		"paragen_cot_summary_display_override": "allow",
		"supported_encodings":                  []any{"v1"},
		"supports_buffering":                   true,
		"system_hints":                         []any{"image_generation"},
		"local_function_names":                 []any{},
		"client_contextual_info": map[string]any{
			"app_name":                         "chatgpt.com",
			"has_web_push_capabilities":        false,
			"is_dark_mode":                     false,
			"page_height":                      900,
			"page_width":                       1440,
			"pixel_ratio":                      1,
			"screen_height":                    1080,
			"screen_width":                     1920,
			"time_since_loaded":                1,
			"web_push_notification_permission": "default",
		},
	}
	return json.Marshal(body)
}

func parseConversationSSE(reader io.Reader, state *generationState) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxProtocolResponseBytes)
	sawData := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		sawData = true
		var event any
		if errDecode := json.Unmarshal([]byte(data), &event); errDecode != nil {
			return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "conversation", Msg: "web image conversation SSE schema changed"}
		}
		if state.conversationID == "" {
			state.conversationID = findString(event, "conversation_id", "conversationId")
		}
		if state.fileID == "" {
			state.fileID = findFileID(event)
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "conversation", Msg: "web image conversation stream was interrupted"}
	}
	if !sawData || state.conversationID == "" {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "conversation", Msg: "web image conversation did not start"}
	}
	return nil
}

func findFileID(value any) string {
	pointer := findString(value, "asset_pointer", "image_asset_pointer", "file_id", "fileId")
	pointer = strings.TrimSpace(pointer)
	if pointer == "" {
		return ""
	}
	if index := strings.LastIndex(pointer, "/"); index >= 0 && index < len(pointer)-1 {
		pointer = pointer[index+1:]
	}
	return strings.TrimPrefix(pointer, "sediment:")
}
