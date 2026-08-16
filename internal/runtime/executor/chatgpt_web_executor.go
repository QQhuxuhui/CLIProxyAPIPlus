package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

const chatGPTWebProvider = "chatgpt-web"

// ChatGPTWebExecutor adapts the private ChatGPT Web protocol to host-native execution.
type ChatGPTWebExecutor struct {
	cfg *config.Config
}

// NewChatGPTWebExecutor creates the isolated ChatGPT Web executor.
func NewChatGPTWebExecutor(cfg *config.Config) *ChatGPTWebExecutor {
	return &ChatGPTWebExecutor{cfg: cfg}
}

func (e *ChatGPTWebExecutor) Identifier() string { return chatGPTWebProvider }

func (e *ChatGPTWebExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	client, errClient := helps.NewChatGPTWebClient(ctx, e.cfg, auth)
	if errClient != nil {
		return resp, errClient
	}
	if chatGPTWebIsImageRequest(opts) {
		return e.executeImage(ctx, client, req)
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	defer reporter.TrackFailure(ctx, &err)

	prepared, errPrepare := chatGPTWebPrepareChat(req, opts, false)
	if errPrepare != nil {
		return resp, errPrepare
	}
	result, errChat := client.Chat(ctx, prepared.prompt)
	if errChat != nil {
		return resp, errChat
	}
	upstream, errBuild := chatGPTWebCompletionPayload(req.Model, result.Text)
	if errBuild != nil {
		return resp, errBuild
	}
	reporter.Publish(ctx, helps.ParseOpenAIUsage(upstream))
	var param any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatOpenAI, prepared.responseFormat, req.Model, opts.OriginalRequest, prepared.body, upstream, &param)
	return cliproxyexecutor.Response{
		Payload: out,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func (e *ChatGPTWebExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if chatGPTWebIsImageRequest(opts) {
		return nil, &helps.ChatGPTWebError{Status: http.StatusBadRequest, Code: "image_stream_unsupported", Message: "ChatGPT Web image streaming is not supported"}
	}
	client, errClient := helps.NewChatGPTWebClient(ctx, e.cfg, auth)
	if errClient != nil {
		return nil, errClient
	}
	prepared, errPrepare := chatGPTWebPrepareChat(req, opts, true)
	if errPrepare != nil {
		return nil, errPrepare
	}
	result, errChat := client.Chat(ctx, prepared.prompt)
	if errChat != nil {
		return nil, errChat
	}
	chunks, errChunks := chatGPTWebCompletionStream(req.Model, result.Text)
	if errChunks != nil {
		return nil, errChunks
	}

	out := make(chan cliproxyexecutor.StreamChunk, len(chunks)+2)
	go func() {
		defer close(out)
		var param any
		for _, chunk := range chunks {
			translated := sdktranslator.TranslateStream(ctx, sdktranslator.FormatOpenAI, prepared.responseFormat, req.Model, opts.OriginalRequest, prepared.body, chunk, &param)
			for _, item := range translated {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: item}:
				case <-ctx.Done():
					return
				}
			}
		}
		done := sdktranslator.TranslateStream(ctx, sdktranslator.FormatOpenAI, prepared.responseFormat, req.Model, opts.OriginalRequest, prepared.body, []byte("[DONE]"), &param)
		for _, item := range done {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: item}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks:  out,
	}, nil
}

func (e *ChatGPTWebExecutor) executeImage(ctx context.Context, client *helps.ChatGPTWebClient, req cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
	prompt := strings.TrimSpace(gjson.GetBytes(req.Payload, "prompt").String())
	if prompt == "" {
		return cliproxyexecutor.Response{}, &helps.ChatGPTWebError{Status: http.StatusBadRequest, Code: "empty_prompt", Message: "image prompt is required"}
	}
	n := gjson.GetBytes(req.Payload, "n")
	if n.Exists() && n.Int() != 1 {
		return cliproxyexecutor.Response{}, &helps.ChatGPTWebError{Status: http.StatusBadRequest, Code: "image_count_unsupported", Message: "ChatGPT Web supports n=1 only"}
	}
	size := strings.TrimSpace(gjson.GetBytes(req.Payload, "size").String())
	if size == "" {
		size = "1024x1024"
	}
	switch size {
	case "1024x1024", "1536x1024", "1024x1536":
	default:
		return cliproxyexecutor.Response{}, &helps.ChatGPTWebError{Status: http.StatusBadRequest, Code: "image_size_unsupported", Message: "supported sizes are 1024x1024, 1536x1024, and 1024x1536"}
	}
	quality := strings.TrimSpace(gjson.GetBytes(req.Payload, "quality").String())
	result, errGenerate := client.GenerateImage(ctx, prompt, size, quality)
	if errGenerate != nil {
		return cliproxyexecutor.Response{}, errGenerate
	}
	item := map[string]any{
		"b64_json":       base64.StdEncoding.EncodeToString(result.Image),
		"revised_prompt": result.RevisedPrompt,
		"mime_type":      result.MIMEType,
	}
	payload, errMarshal := json.Marshal(map[string]any{
		"created": time.Now().Unix(),
		"data":    []any{item},
	})
	if errMarshal != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("chatgpt web executor: marshal image response: %w", errMarshal)
	}
	return cliproxyexecutor.Response{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func (e *ChatGPTWebExecutor) Refresh(_ context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, &helps.ChatGPTWebError{Status: http.StatusServiceUnavailable, Code: "credentials_missing", Message: "ChatGPT Web auth is missing"}
	}
	return auth, nil
}

func (e *ChatGPTWebExecutor) CountTokens(_ context.Context, _ *cliproxyauth.Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	count := len([]rune(string(req.Payload))) / 4
	if count == 0 && len(req.Payload) > 0 {
		count = 1
	}
	payload, _ := json.Marshal(map[string]any{"total_tokens": count})
	return cliproxyexecutor.Response{Payload: payload}, nil
}

func (e *ChatGPTWebExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, request *http.Request) (*http.Response, error) {
	client, errClient := helps.NewChatGPTWebClient(ctx, e.cfg, auth)
	if errClient != nil {
		return nil, errClient
	}
	return client.DoRequest(ctx, request)
}

type chatGPTWebPreparedChat struct {
	body           []byte
	prompt         string
	responseFormat sdktranslator.Format
}

func chatGPTWebPrepareChat(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (chatGPTWebPreparedChat, error) {
	from := opts.SourceFormat
	if from == "" {
		from = sdktranslator.FormatOpenAI
	}
	body := bytes.Clone(req.Payload)
	if from != sdktranslator.FormatOpenAI {
		body = sdktranslator.TranslateRequest(from, sdktranslator.FormatOpenAI, req.Model, body, stream)
	}
	prompt, errPrompt := chatGPTWebFlattenMessages(body)
	if errPrompt != nil {
		return chatGPTWebPreparedChat{}, errPrompt
	}
	return chatGPTWebPreparedChat{
		body:           body,
		prompt:         prompt,
		responseFormat: cliproxyexecutor.ResponseFormatOrSource(opts),
	}, nil
}

func chatGPTWebFlattenMessages(payload []byte) (string, error) {
	var request map[string]any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&request); errDecode != nil {
		return "", &helps.ChatGPTWebError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "request body is not valid JSON", Cause: errDecode}
	}
	messages, _ := request["messages"].([]any)
	parts := make([]string, 0, len(messages))
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		role, _ := message["role"].(string)
		role = strings.TrimSpace(role)
		text := chatGPTWebMessageText(message["content"])
		if text == "" {
			continue
		}
		if role == "" {
			role = "user"
		}
		parts = append(parts, "["+role+"]\n"+text)
	}
	if len(parts) == 0 {
		return "", &helps.ChatGPTWebError{Status: http.StatusBadRequest, Code: "empty_messages", Message: "messages contain no text"}
	}
	return strings.Join(parts, "\n\n"), nil
}

func chatGPTWebMessageText(content any) string {
	switch current := content.(type) {
	case string:
		return strings.TrimSpace(current)
	case []any:
		parts := make([]string, 0, len(current))
		for _, rawPart := range current {
			part, _ := rawPart.(map[string]any)
			typeName, _ := part["type"].(string)
			if typeName != "text" && typeName != "input_text" {
				continue
			}
			text, _ := part["text"].(string)
			if strings.TrimSpace(text) != "" {
				parts = append(parts, strings.TrimSpace(text))
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func chatGPTWebCompletionPayload(model, text string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"id":      "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	})
}

func chatGPTWebCompletionStream(model, text string) ([][]byte, error) {
	id := "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	created := time.Now().Unix()
	first, errFirst := json.Marshal(map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": text}, "finish_reason": nil}},
	})
	if errFirst != nil {
		return nil, errFirst
	}
	final, errFinal := json.Marshal(map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	})
	if errFinal != nil {
		return nil, errFinal
	}
	return [][]byte{
		[]byte("data: " + string(first)),
		[]byte("data: " + string(final)),
	}, nil
}

func chatGPTWebIsImageRequest(opts cliproxyexecutor.Options) bool {
	if strings.EqualFold(opts.SourceFormat.String(), "openai-image") {
		return true
	}
	if opts.Metadata == nil {
		return false
	}
	path, _ := opts.Metadata[cliproxyexecutor.RequestPathMetadataKey].(string)
	return strings.Contains(strings.ToLower(path), "/images/")
}
