package webimage

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	fileServiceAssetPattern = regexp.MustCompile(`file-service://[A-Za-z0-9_.:-]+`)
	sedimentAssetPattern    = regexp.MustCompile(`sediment://[A-Za-z0-9_.:-]+`)
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
	for name, values := range sentinelHeaders(state) {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}

	response, errDo := session.Client.Do(request)
	if errDo != nil {
		if errContext := preserveContextError(errDo); errContext != nil {
			return errContext
		}
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
	body := e.buildConversationPayload(state)
	now := time.Now()
	messageID := uuid.NewString()
	body["messages"] = []any{
		map[string]any{
			"id":          messageID,
			"author":      map[string]any{"role": "user"},
			"create_time": float64(now.UnixMilli()) / 1000,
			"content":     map[string]any{"content_type": "text", "parts": []any{state.prompt}},
			"metadata": map[string]any{
				"developer_mode_connector_ids": []any{},
				"selected_github_repos":        []any{},
				"selected_all_github_repos":    false,
				"serialization_metadata":       map[string]any{"custom_symbol_offsets": []any{}},
				"system_hints":                 []any{"picture_v2"},
			},
		},
	}
	return json.Marshal(body)
}

func (e *Executor) buildPrepareBody(state *generationState) ([]byte, error) {
	if state.parentMessageID == "" {
		state.parentMessageID = uuid.NewString()
	}
	body := map[string]any{
		"action":                "next",
		"fork_from_shared_post": false,
		"parent_message_id":     state.parentMessageID,
		"model":                 strings.TrimSpace(e.cfg.WebImageBaseModel),
		"client_prepare_state":  "success",
		"timezone_offset_min":   -480,
		"timezone":              "Asia/Shanghai",
		"conversation_mode":     map[string]any{"kind": "primary_assistant"},
		"system_hints":          []any{"picture_v2"},
		"partial_query": map[string]any{
			"id":      uuid.NewString(),
			"author":  map[string]any{"role": "user"},
			"content": map[string]any{"content_type": "text", "parts": []any{state.prompt}},
		},
		"supports_buffering":  true,
		"supported_encodings": []any{"v1"},
		"client_contextual_info": map[string]any{
			"app_name": "chatgpt.com",
		},
	}
	return json.Marshal(body)
}

func (e *Executor) buildConversationPayload(state *generationState) map[string]any {
	if state.parentMessageID == "" {
		state.parentMessageID = uuid.NewString()
	}
	baseModel := strings.TrimSpace(e.cfg.WebImageBaseModel)
	return map[string]any{
		"action":                               "next",
		"parent_message_id":                    state.parentMessageID,
		"model":                                baseModel,
		"client_prepare_state":                 "sent",
		"timezone":                             "Asia/Shanghai",
		"timezone_offset_min":                  -480,
		"conversation_mode":                    map[string]any{"kind": "primary_assistant"},
		"force_parallel_switch":                "auto",
		"enable_message_followups":             true,
		"paragen_cot_summary_display_override": "allow",
		"supported_encodings":                  []any{"v1"},
		"supports_buffering":                   true,
		"system_hints":                         []any{"picture_v2"},
		"local_function_names":                 []any{},
		"client_contextual_info": map[string]any{
			"app_name":          "chatgpt.com",
			"is_dark_mode":      false,
			"time_since_loaded": 1200,
			"page_height":       1072,
			"page_width":        1724,
			"pixel_ratio":       1.2,
			"screen_height":     1440,
			"screen_width":      2560,
		},
	}
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
		mergeGenerationState(state, event)
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
	return assetID(findAssetRef(value))
}

func findAssetRef(value any) string {
	refs := findAssetRefs(value)
	if len(refs) > 0 {
		return refs[0]
	}
	return ""
}

func findAssetRefs(value any) []string {
	refs := make([]string, 0, 4)
	findEmbeddedAssetRefs(value, &refs)
	return deduplicateAssetRefs(refs)
}

func findEmbeddedAssetRefs(value any, refs *[]string) {
	switch current := value.(type) {
	case string:
		for _, match := range fileServiceAssetPattern.FindAllString(current, -1) {
			*refs = append(*refs, match)
		}
		for _, match := range sedimentAssetPattern.FindAllString(current, -1) {
			*refs = append(*refs, match)
		}
		trimmed := strings.TrimSpace(current)
		lower := strings.ToLower(trimmed)
		if (strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")) && (strings.Contains(lower, "image") || strings.Contains(lower, "download") || strings.Contains(lower, ".png") || strings.Contains(lower, ".jpg") || strings.Contains(lower, ".jpeg") || strings.Contains(lower, ".webp")) {
			*refs = append(*refs, trimmed)
		}
		if strings.HasPrefix(lower, "data:image/") {
			*refs = append(*refs, trimmed)
		}
	case map[string]any:
		for key, child := range current {
			if text, ok := child.(string); ok {
				switch strings.ToLower(key) {
				case "asset_pointer", "image_asset_pointer", "file_id", "fileid", "sediment_id":
					if trimmed := strings.TrimSpace(text); trimmed != "" {
						*refs = append(*refs, trimmed)
					}
				}
			}
			findEmbeddedAssetRefs(child, refs)
		}
	case []any:
		for _, child := range current {
			findEmbeddedAssetRefs(child, refs)
		}
	}
}

func mergeGenerationState(state *generationState, value any) {
	if state == nil {
		return
	}
	state.assetRefs = deduplicateAssetRefs(append(state.assetRefs, findAssetRefs(value)...))
	for _, status := range findStrings(value, "status", "type") {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "finished_successfully", "completed", "done", "finished", "message_stream_complete":
			state.done = true
		case "failed", "error", "cancelled", "canceled":
			state.failed = true
		}
	}
}

func deduplicateAssetRefs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func hasDirectAsset(refs []string) bool {
	for _, ref := range refs {
		lower := strings.ToLower(strings.TrimSpace(ref))
		if strings.HasPrefix(lower, "file-service://") || strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "data:image/") {
			return true
		}
	}
	return false
}

func assetID(pointer string) string {
	pointer = strings.TrimSpace(pointer)
	if pointer == "" {
		return ""
	}
	if index := strings.LastIndex(pointer, "/"); index >= 0 && index < len(pointer)-1 {
		pointer = pointer[index+1:]
	}
	return strings.TrimPrefix(pointer, "sediment:")
}
