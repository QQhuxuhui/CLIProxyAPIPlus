package openai

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeRequestedModelForResponse ensures "auto" model requests echo the resolved model
// (matching server routing behavior), while all other model names are returned as-is.
// This keeps alias-based model names stable in responses.
func normalizeRequestedModelForResponse(requested string) string {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return ""
	}

	parsed := thinking.ParseSuffix(requested)
	if parsed.ModelName != "auto" {
		return requested
	}

	resolved := util.ResolveAutoModel("auto")
	if parsed.HasSuffix && strings.TrimSpace(parsed.RawSuffix) != "" {
		return fmt.Sprintf("%s(%s)", resolved, parsed.RawSuffix)
	}
	return resolved
}

func rewriteOpenAIResponseModel(payload []byte, modelName string) []byte {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" || len(payload) == 0 {
		return payload
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || !gjson.ValidBytes(trimmed) {
		return payload
	}
	// Don't mutate error payloads.
	if gjson.GetBytes(trimmed, "error").Exists() {
		return payload
	}

	updated, err := sjson.SetBytes(trimmed, "model", modelName)
	if err != nil {
		return payload
	}
	return updated
}

func rewriteOpenAIStreamChunkModel(chunk []byte, modelName string) []byte {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" || len(chunk) == 0 {
		return chunk
	}

	trimmed := bytes.TrimSpace(chunk)
	if len(trimmed) == 0 {
		return chunk
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return trimmed
	}
	if !gjson.ValidBytes(trimmed) {
		return chunk
	}
	if gjson.GetBytes(trimmed, "error").Exists() {
		return chunk
	}

	updated, err := sjson.SetBytes(trimmed, "model", modelName)
	if err != nil {
		return chunk
	}
	return updated
}

// rewriteOpenAIResponsesStreamChunkModel rewrites the model name inside OpenAI Responses SSE chunks.
//
// OpenAI Responses streaming uses "event:" + "data:" lines. The "model" can appear at:
// - response.model (common for response.created/response.completed)
// - model (defensive fallback)
func rewriteOpenAIResponsesStreamChunkModel(chunk []byte, modelName string) []byte {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" || len(chunk) == 0 {
		return chunk
	}

	parts := bytes.SplitAfter(chunk, []byte("\n"))
	changed := false

	for i := range parts {
		lineWithNL := parts[i]
		line := bytes.TrimRight(lineWithNL, "\r\n")
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}

		data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if !gjson.ValidBytes(data) {
			continue
		}
		// Don't mutate error payloads.
		if gjson.GetBytes(data, "error").Exists() {
			continue
		}

		path := ""
		if gjson.GetBytes(data, "response.model").Exists() {
			path = "response.model"
		} else if gjson.GetBytes(data, "model").Exists() {
			path = "model"
		} else {
			continue
		}

		updated, err := sjson.SetBytes(data, path, modelName)
		if err != nil {
			continue
		}

		// Preserve original newline suffix.
		suffix := lineWithNL[len(line):]
		outLine := append([]byte("data: "), updated...)
		outLine = append(outLine, suffix...)
		parts[i] = outLine
		changed = true
	}

	if !changed {
		return chunk
	}
	return bytes.Join(parts, nil)
}
