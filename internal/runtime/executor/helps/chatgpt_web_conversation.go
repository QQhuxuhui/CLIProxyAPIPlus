package helps

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	chatGPTWebFileServicePattern = regexp.MustCompile(`file-service://([A-Za-z0-9_-]+)`)
	chatGPTWebSedimentPattern    = regexp.MustCompile(`sediment://([A-Za-z0-9_-]+)`)
)

type chatGPTWebConversationState struct {
	ConversationID string
	Text           string
	AssetRefs      []string
	Done           bool
	Failed         bool
	TextIsDelta    bool
}

// Chat runs one stateless ChatGPT Web conversation.
func (c *ChatGPTWebClient) Chat(ctx context.Context, prompt string) (ChatGPTWebResult, error) {
	if strings.TrimSpace(prompt) == "" {
		return ChatGPTWebResult{}, newChatGPTWebError(http.StatusBadRequest, "empty_messages", "messages contain no text", nil)
	}
	if errToken := c.ensureAccessToken(ctx); errToken != nil {
		return ChatGPTWebResult{}, errToken
	}
	if c.textBootstrap {
		if errBootstrap := c.bootstrap(ctx); errBootstrap != nil {
			return ChatGPTWebResult{}, errBootstrap
		}
	}
	requirements, errRequirements := c.requirements(ctx)
	if errRequirements != nil {
		return ChatGPTWebResult{}, errRequirements
	}
	payload := c.textConversationPayload(prompt)
	conduit := ""
	if c.textPrepare {
		var errPrepare error
		conduit, errPrepare = c.prepareText(ctx, requirements, payload)
		if errPrepare != nil {
			return ChatGPTWebResult{}, errPrepare
		}
	}
	state, errStart := c.startConversation(ctx, "/backend-api/conversation", requirements, conduit, payload, false)
	if c.isAuthError(errStart) {
		requirements, errRequirements = c.requirements(ctx)
		if errRequirements != nil {
			return ChatGPTWebResult{}, errRequirements
		}
		if c.textPrepare {
			conduit, errStart = c.prepareText(ctx, requirements, payload)
			if errStart != nil {
				return ChatGPTWebResult{}, errStart
			}
		}
		state, errStart = c.startConversation(ctx, "/backend-api/conversation", requirements, conduit, payload, false)
	}
	if errStart != nil {
		return ChatGPTWebResult{}, errStart
	}
	if state.Failed {
		return ChatGPTWebResult{}, newChatGPTWebError(http.StatusBadGateway, "conversation_failed", "ChatGPT Web conversation failed", nil)
	}
	if strings.TrimSpace(state.Text) == "" {
		return ChatGPTWebResult{}, newChatGPTWebError(http.StatusBadGateway, "empty_response", "ChatGPT Web returned no text", nil)
	}
	return ChatGPTWebResult{Text: state.Text, ConversationID: state.ConversationID}, nil
}

// GenerateImage runs the picture_v2 flow and downloads the final image bytes.
func (c *ChatGPTWebClient) GenerateImage(ctx context.Context, prompt, size, quality string) (ChatGPTWebResult, error) {
	if strings.TrimSpace(prompt) == "" {
		return ChatGPTWebResult{}, newChatGPTWebError(http.StatusBadRequest, "empty_prompt", "image prompt is required", nil)
	}
	if errToken := c.ensureAccessToken(ctx); errToken != nil {
		return ChatGPTWebResult{}, errToken
	}
	if c.imageBootstrap {
		if errBootstrap := c.bootstrap(ctx); errBootstrap != nil {
			return ChatGPTWebResult{}, errBootstrap
		}
	}
	requirements, errRequirements := c.requirements(ctx)
	if errRequirements != nil {
		return ChatGPTWebResult{}, errRequirements
	}
	imagePrompt := chatGPTWebImagePrompt(prompt, size, quality)
	messageID := uuid.NewString()
	parentMessageID := uuid.NewString()
	payload := c.imageConversationPayload(imagePrompt, messageID, parentMessageID)
	conduit := ""
	if c.imagePrepare {
		var errPrepare error
		conduit, errPrepare = c.prepareImage(ctx, requirements, imagePrompt, parentMessageID)
		if errPrepare != nil {
			return ChatGPTWebResult{}, errPrepare
		}
	}
	state, errStart := c.startConversation(ctx, "/backend-api/f/conversation", requirements, conduit, payload, true)
	if c.isAuthError(errStart) {
		requirements, errRequirements = c.requirements(ctx)
		if errRequirements != nil {
			return ChatGPTWebResult{}, errRequirements
		}
		if c.imagePrepare {
			conduit, errStart = c.prepareImage(ctx, requirements, imagePrompt, parentMessageID)
			if errStart != nil {
				return ChatGPTWebResult{}, errStart
			}
		}
		state, errStart = c.startConversation(ctx, "/backend-api/f/conversation", requirements, conduit, payload, true)
	}
	if errStart != nil {
		return ChatGPTWebResult{}, errStart
	}
	if state.Failed && len(state.AssetRefs) == 0 {
		return ChatGPTWebResult{}, newChatGPTWebError(http.StatusBadGateway, "image_generation_failed", "ChatGPT Web image generation failed", nil)
	}
	if state.ConversationID != "" {
		var errPoll error
		state, errPoll = c.pollForImage(ctx, requirements, state)
		if errPoll != nil {
			return ChatGPTWebResult{}, errPoll
		}
	}
	if len(state.AssetRefs) == 0 {
		return ChatGPTWebResult{}, newChatGPTWebError(http.StatusBadGateway, "image_asset_missing", "image generation completed without an asset", nil)
	}

	var lastError error
	refs := chatGPTWebDeduplicate(state.AssetRefs)
	for index := len(refs) - 1; index >= 0; index-- {
		image, mimeType, errDownload := c.downloadAsset(ctx, state.ConversationID, refs[index])
		if errDownload == nil {
			return ChatGPTWebResult{
				Text:           state.Text,
				ConversationID: state.ConversationID,
				Image:          image,
				MIMEType:       mimeType,
				RevisedPrompt:  state.Text,
			}, nil
		}
		lastError = errDownload
	}
	if lastError != nil {
		return ChatGPTWebResult{}, lastError
	}
	return ChatGPTWebResult{}, newChatGPTWebError(http.StatusBadGateway, "image_download_failed", "all image assets failed to download", nil)
}

func (c *ChatGPTWebClient) prepareText(ctx context.Context, requirements chatGPTWebRequirements, payload map[string]any) (string, error) {
	result, errPrepare := c.postJSON(ctx, "/backend-api/conversation/prepare", payload, c.sentinelHeaders(requirements))
	if errPrepare != nil {
		return "", errPrepare
	}
	conduit := chatGPTWebFirstString(result, "token", "conduit_token", "conduitToken")
	if conduit == "" {
		conduit = chatGPTWebFirstString(chatGPTWebMap(result["prepare_data"]), "token", "conduit_token", "conduitToken")
	}
	if conduit == "" {
		return "", newChatGPTWebError(http.StatusBadGateway, "conduit_missing", "conversation prepare returned no conduit token", nil)
	}
	return conduit, nil
}

func (c *ChatGPTWebClient) prepareImage(ctx context.Context, requirements chatGPTWebRequirements, prompt, parentMessageID string) (string, error) {
	payload := map[string]any{
		"action":                "next",
		"fork_from_shared_post": false,
		"parent_message_id":     parentMessageID,
		"model":                 c.imageModel,
		"client_prepare_state":  "success",
		"timezone_offset_min":   -480,
		"timezone":              "Asia/Shanghai",
		"conversation_mode":     map[string]any{"kind": "primary_assistant"},
		"system_hints":          []string{"picture_v2"},
		"partial_query": map[string]any{
			"id":      uuid.NewString(),
			"author":  map[string]any{"role": "user"},
			"content": map[string]any{"content_type": "text", "parts": []string{prompt}},
		},
		"supports_buffering":  true,
		"supported_encodings": []string{"v1"},
		"client_contextual_info": map[string]any{
			"app_name": "chatgpt.com",
		},
	}
	result, errPrepare := c.postJSON(ctx, "/backend-api/f/conversation/prepare", payload, c.sentinelHeaders(requirements))
	if errPrepare != nil {
		return "", errPrepare
	}
	conduit := chatGPTWebFirstString(result, "conduit_token", "token", "conduitToken")
	if conduit == "" {
		return "", newChatGPTWebError(http.StatusBadGateway, "conduit_missing", "f/conversation prepare returned no conduit token", nil)
	}
	return conduit, nil
}

func (c *ChatGPTWebClient) startConversation(ctx context.Context, path string, requirements chatGPTWebRequirements, conduit string, payload map[string]any, image bool) (chatGPTWebConversationState, error) {
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return chatGPTWebConversationState{}, fmt.Errorf("chatgpt web: marshal conversation: %w", errMarshal)
	}
	headers := c.sentinelHeaders(requirements)
	headers.Set("Accept", "text/event-stream")
	if image {
		headers.Set("X-Oai-Turn-Trace-Id", uuid.NewString())
		if conduit != "" {
			headers.Set("X-Conduit-Token", conduit)
		}
	} else if conduit != "" {
		headers.Set("OpenAI-Conduit-Token", conduit)
	}
	resp, errDo := c.do(ctx, http.MethodPost, path, body, headers, true)
	if errDo != nil {
		return chatGPTWebConversationState{}, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return chatGPTWebConversationState{}, c.httpError(path, resp.StatusCode)
	}
	events, errSSE := chatGPTWebReadSSE(resp.Body)
	if errSSE != nil {
		return chatGPTWebConversationState{}, newChatGPTWebError(http.StatusBadGateway, "conversation_sse_invalid", "ChatGPT Web returned invalid SSE", errSSE)
	}
	return chatGPTWebParseEvents(events), nil
}

func (c *ChatGPTWebClient) pollForImage(ctx context.Context, requirements chatGPTWebRequirements, initial chatGPTWebConversationState) (chatGPTWebConversationState, error) {
	state := initial
	lastChange := time.Now()
	previousCount := len(state.AssetRefs)
	rateLimitDelay := c.pollInterval
	if rateLimitDelay < 3*time.Second {
		rateLimitDelay = 3 * time.Second
	}
	for {
		if chatGPTWebHasDirectAsset(state.AssetRefs) {
			return state, nil
		}
		if len(state.AssetRefs) > 0 && (state.Done || time.Since(lastChange) >= 18*time.Second) {
			return state, nil
		}
		if state.Done && len(state.AssetRefs) == 0 {
			return state, newChatGPTWebError(http.StatusBadGateway, "image_asset_missing", "image generation completed without an asset", nil)
		}
		if errWait := chatGPTWebWait(ctx, c.pollInterval); errWait != nil {
			return state, errWait
		}
		path := "/backend-api/conversation/" + url.PathEscape(state.ConversationID)
		resp, errDo := c.do(ctx, http.MethodGet, path, nil, c.sentinelHeaders(requirements), true)
		if errDo != nil {
			return state, errDo
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter := chatGPTWebRetryAfter(resp.Header.Get("Retry-After"))
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if retryAfter < rateLimitDelay {
				retryAfter = rateLimitDelay
			}
			if errWait := chatGPTWebWait(ctx, retryAfter); errWait != nil {
				return state, errWait
			}
			rateLimitDelay *= 2
			if rateLimitDelay > 30*time.Second {
				rateLimitDelay = 30 * time.Second
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_ = resp.Body.Close()
			return state, c.httpError("conversation poll", resp.StatusCode)
		}
		payload, errJSON := chatGPTWebReadJSON(resp.Body)
		_ = resp.Body.Close()
		if errJSON != nil {
			return state, newChatGPTWebError(http.StatusBadGateway, "conversation_poll_invalid", "conversation poll returned invalid JSON", errJSON)
		}
		rateLimitDelay = c.pollInterval
		if rateLimitDelay < 3*time.Second {
			rateLimitDelay = 3 * time.Second
		}
		current := chatGPTWebParseConversation(payload)
		if current.ConversationID == "" {
			current.ConversationID = state.ConversationID
		}
		current.AssetRefs = chatGPTWebDeduplicate(append(state.AssetRefs, current.AssetRefs...))
		if current.Text == "" {
			current.Text = state.Text
		}
		if len(current.AssetRefs) != previousCount {
			previousCount = len(current.AssetRefs)
			lastChange = time.Now()
		}
		state = current
		if state.Failed && len(state.AssetRefs) == 0 {
			return state, newChatGPTWebError(http.StatusBadGateway, "image_generation_failed", "ChatGPT Web image generation failed", nil)
		}
	}
}

func (c *ChatGPTWebClient) textConversationPayload(prompt string) map[string]any {
	return map[string]any{
		"action": "next",
		"messages": []any{map[string]any{
			"id":       uuid.NewString(),
			"author":   map[string]any{"role": "user"},
			"content":  map[string]any{"content_type": "text", "parts": []string{prompt}},
			"metadata": map[string]any{},
		}},
		"parent_message_id":             uuid.NewString(),
		"model":                         c.textModel,
		"timezone_offset_min":           0,
		"timezone":                      "UTC",
		"conversation_mode":             map[string]any{"kind": "primary_assistant"},
		"history_and_training_disabled": true,
		"supported_encodings":           []string{"v1"},
	}
}

func (c *ChatGPTWebClient) imageConversationPayload(prompt, messageID, parentMessageID string) map[string]any {
	return map[string]any{
		"action": "next",
		"messages": []any{map[string]any{
			"id":          messageID,
			"author":      map[string]any{"role": "user"},
			"create_time": float64(time.Now().UnixMilli()) / 1000,
			"content":     map[string]any{"content_type": "text", "parts": []string{prompt}},
			"metadata": map[string]any{
				"developer_mode_connector_ids": []string{},
				"selected_github_repos":        []string{},
				"selected_all_github_repos":    false,
				"system_hints":                 []string{"picture_v2"},
				"serialization_metadata":       map[string]any{"custom_symbol_offsets": []any{}},
			},
		}},
		"parent_message_id":                    parentMessageID,
		"model":                                c.imageModel,
		"client_prepare_state":                 "sent",
		"timezone_offset_min":                  -480,
		"timezone":                             "Asia/Shanghai",
		"conversation_mode":                    map[string]any{"kind": "primary_assistant"},
		"enable_message_followups":             true,
		"system_hints":                         []string{"picture_v2"},
		"supports_buffering":                   true,
		"supported_encodings":                  []string{"v1"},
		"paragen_cot_summary_display_override": "allow",
		"force_parallel_switch":                "auto",
		"client_contextual_info": map[string]any{
			"is_dark_mode":      false,
			"time_since_loaded": 1200,
			"page_height":       1072,
			"page_width":        1724,
			"pixel_ratio":       1.2,
			"screen_height":     1440,
			"screen_width":      2560,
			"app_name":          "chatgpt.com",
		},
	}
}

func chatGPTWebImagePrompt(prompt, size, quality string) string {
	notes := make([]string, 0, 2)
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "1536x1024":
		notes = append(notes, "Please use a wide landscape composition.")
	case "1024x1536":
		notes = append(notes, "Please use a tall portrait composition.")
	}
	quality = strings.TrimSpace(quality)
	if quality != "" && !strings.EqualFold(quality, "auto") && !strings.EqualFold(quality, "low") {
		notes = append(notes, "Requested output quality: "+quality+".")
	}
	if len(notes) == 0 {
		return prompt
	}
	return prompt + "\n\n" + strings.Join(notes, " ")
}

func chatGPTWebReadSSE(reader io.Reader) ([]map[string]any, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), chatGPTWebMaxJSONBytes)
	dataLines := make([]string, 0, 4)
	events := make([]map[string]any, 0, 8)
	flush := func() {
		if len(dataLines) == 0 {
			return
		}
		raw := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if raw == "[DONE]" {
			return
		}
		value := make(map[string]any)
		if json.Unmarshal([]byte(raw), &value) == nil {
			events = append(events, value)
		}
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	if errScan := scanner.Err(); errScan != nil {
		return nil, errScan
	}
	return events, nil
}

func chatGPTWebParseEvents(events []map[string]any) chatGPTWebConversationState {
	merged := chatGPTWebConversationState{}
	for _, event := range events {
		current := chatGPTWebParseConversation(event)
		if current.ConversationID != "" {
			merged.ConversationID = current.ConversationID
		}
		if current.Text != "" {
			if current.TextIsDelta {
				merged.Text += current.Text
			} else {
				merged.Text = current.Text
			}
		}
		merged.AssetRefs = append(merged.AssetRefs, current.AssetRefs...)
		merged.Done = merged.Done || current.Done
		merged.Failed = merged.Failed || current.Failed
	}
	merged.AssetRefs = chatGPTWebDeduplicate(merged.AssetRefs)
	return merged
}

func chatGPTWebParseConversation(payload map[string]any) chatGPTWebConversationState {
	state := chatGPTWebConversationState{}
	if value := chatGPTWebFindFirst(payload, map[string]bool{"conversation_id": true, "conversationId": true}); value != nil {
		state.ConversationID = chatGPTWebString(value)
	}
	for _, value := range chatGPTWebFindValues(payload, map[string]bool{"status": true}) {
		switch strings.ToLower(chatGPTWebString(value)) {
		case "finished_successfully", "completed", "done", "finished":
			state.Done = true
		case "failed", "error", "cancelled", "canceled":
			state.Failed = true
		}
	}
	for _, value := range chatGPTWebFindValues(payload, map[string]bool{"type": true}) {
		if strings.EqualFold(chatGPTWebString(value), "message_stream_complete") {
			state.Done = true
		}
	}

	deltas := make([]string, 0)
	chatGPTWebWalk(payload, func(value any) {
		switch current := value.(type) {
		case string:
			for _, match := range chatGPTWebFileServicePattern.FindAllStringSubmatch(current, -1) {
				state.AssetRefs = append(state.AssetRefs, "file-service://"+match[1])
			}
			for _, match := range chatGPTWebSedimentPattern.FindAllStringSubmatch(current, -1) {
				state.AssetRefs = append(state.AssetRefs, "sediment://"+match[1])
			}
		case map[string]any:
			if current["o"] == "append" && current["p"] == "/message/content/parts/0" {
				if delta := chatGPTWebString(current["v"]); delta != "" {
					deltas = append(deltas, delta)
				}
			}
			message := current
			if nested := chatGPTWebMap(current["message"]); nested != nil {
				message = nested
			}
			author := chatGPTWebMap(message["author"])
			if strings.EqualFold(chatGPTWebString(author["role"]), "assistant") {
				content := chatGPTWebMap(message["content"])
				if parts, ok := content["parts"].([]any); ok {
					texts := make([]string, 0, len(parts))
					for _, part := range parts {
						if text := chatGPTWebString(part); text != "" {
							texts = append(texts, text)
						}
					}
					if len(texts) > 0 {
						state.Text = strings.Join(texts, "\n")
					}
				}
			}
			for key, raw := range current {
				valueString := chatGPTWebString(raw)
				if valueString == "" {
					continue
				}
				switch strings.ToLower(key) {
				case "asset_pointer", "file_id", "fileid", "sediment_id":
					state.AssetRefs = append(state.AssetRefs, valueString)
				case "url", "download_url", "image_url":
					if chatGPTWebLooksLikeAsset(valueString) {
						state.AssetRefs = append(state.AssetRefs, valueString)
					}
				default:
					if strings.HasPrefix(valueString, "file-service://") || strings.HasPrefix(valueString, "sediment://") || strings.HasPrefix(valueString, "data:image/") {
						state.AssetRefs = append(state.AssetRefs, valueString)
					}
				}
			}
		}
	})
	if len(deltas) > 0 {
		state.Text = strings.Join(deltas, "")
		state.TextIsDelta = true
	}
	state.AssetRefs = chatGPTWebDeduplicate(state.AssetRefs)
	return state
}

func chatGPTWebWalk(value any, visit func(any)) {
	visit(value)
	switch current := value.(type) {
	case map[string]any:
		for _, child := range current {
			chatGPTWebWalk(child, visit)
		}
	case []any:
		for _, child := range current {
			chatGPTWebWalk(child, visit)
		}
	}
}

func chatGPTWebFindValues(value any, keys map[string]bool) []any {
	values := make([]any, 0)
	chatGPTWebWalk(value, func(current any) {
		object, ok := current.(map[string]any)
		if !ok {
			return
		}
		for key, nested := range object {
			if keys[key] {
				values = append(values, nested)
			}
		}
	})
	return values
}

func chatGPTWebFindFirst(value any, keys map[string]bool) any {
	values := chatGPTWebFindValues(value, keys)
	if len(values) == 0 {
		return nil
	}
	return values[0]
}

func chatGPTWebDeduplicate(values []string) []string {
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

func chatGPTWebLooksLikeAsset(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "file-service://") && !strings.HasPrefix(lower, "sediment://") && !strings.HasPrefix(lower, "data:image/") {
		return false
	}
	for _, marker := range []string{"image", "download", "file", ".png", ".jpg", ".jpeg", ".webp"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func chatGPTWebHasDirectAsset(refs []string) bool {
	for _, ref := range refs {
		if strings.HasPrefix(ref, "file-service://") || strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "data:image/") {
			return true
		}
	}
	return false
}

func chatGPTWebWait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func chatGPTWebRetryAfter(raw string) time.Duration {
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

func (c *ChatGPTWebClient) isAuthError(err error) bool {
	var webError *ChatGPTWebError
	return errors.As(err, &webError) && webError.Code == "upstream_auth_error"
}
