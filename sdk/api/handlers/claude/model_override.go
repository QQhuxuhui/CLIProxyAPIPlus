package claude

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
// This keeps alias-based model names stable in Claude responses.
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

func rewriteClaudeResponseModel(payload []byte, modelName string) []byte {
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

	// Claude non-stream responses include a top-level "model" field.
	if !gjson.GetBytes(trimmed, "model").Exists() {
		return payload
	}
	updated, err := sjson.SetBytes(trimmed, "model", modelName)
	if err != nil {
		return payload
	}
	return updated
}

// rewriteClaudeStreamChunkModel rewrites the model name inside Claude SSE chunks.
//
// The Claude executor can forward streams as:
// - one line per chunk (when upstream is already Claude)
// - multi-line "event: ...\\ndata: {...}\\n\\n" chunks (when translated from other providers)
//
// We rewrite only JSON payloads in "data:" lines that already contain a model field.
func rewriteClaudeStreamChunkModel(chunk []byte, modelName string) []byte {
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
		if len(data) == 0 {
			continue
		}
		if !gjson.ValidBytes(data) {
			continue
		}
		// Don't mutate error payloads.
		if gjson.GetBytes(data, "error").Exists() {
			continue
		}

		// Streaming "message_start" event carries model under message.model.
		path := ""
		if gjson.GetBytes(data, "message.model").Exists() {
			path = "message.model"
		} else if gjson.GetBytes(data, "model").Exists() {
			// Defensive: some translated streams may put model at the root.
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

