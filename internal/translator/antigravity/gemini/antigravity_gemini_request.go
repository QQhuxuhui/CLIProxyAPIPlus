// Package gemini provides request translation functionality for Antigravity to Gemini API compatibility.
// It handles parsing and transforming Antigravity API requests into Gemini API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Antigravity API format and Gemini API's expected format.
package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/translator/gemini/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertGeminiRequestToAntigravity parses and transforms a Antigravity API request into Gemini API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Gemini API.
// The function performs the following transformations:
// 1. Extracts the model information from the request
// 2. Restructures the JSON to match Gemini API format
// 3. Converts system instructions to the expected format
// 4. Fixes CLI tool response format and grouping
//
// Parameters:
//   - modelName: The name of the model to use for the request (unused in current implementation)
//   - rawJSON: The raw JSON request data from the Antigravity API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini API format
func ConvertGeminiRequestToAntigravity(modelName string, inputRawJSON []byte, _ bool) []byte {
	// rawJSON is built here and never aliases the caller's buffer, so sjson is
	// allowed to reuse it in place instead of copying the whole document again.
	inPlace := &sjson.Options{Optimistic: true, ReplaceInPlace: true}

	rawJSON, _ := sjson.SetRawBytesOptions([]byte(`{"project":"","request":{},"model":""}`), "request", inputRawJSON, inPlace)
	rawJSON, _ = sjson.SetBytesOptions(rawJSON, "model", modelName, inPlace)
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "request.model")

	rawJSON, errFixCLIToolResponse := fixCLIToolResponseBytes(rawJSON)
	if errFixCLIToolResponse != nil {
		return []byte{}
	}

	systemInstructionResult := gjson.GetBytes(rawJSON, "request.system_instruction")
	if systemInstructionResult.Exists() {
		rawJSON, _ = sjson.SetRawBytes(rawJSON, "request.systemInstruction", []byte(systemInstructionResult.Raw))
		rawJSON, _ = sjson.DeleteBytes(rawJSON, "request.system_instruction")
	}

	// Normalize roles in request.contents: default to valid values if missing/invalid.
	// The role is rewritten on the content object itself and all rewrites are
	// spliced back at once, instead of copying the document per content.
	contents := gjson.GetBytes(rawJSON, "request.contents")
	if contents.Exists() {
		prevRole := ""
		idx := 0
		var roleEdits []rawJSONEdit
		contents.ForEach(func(_ gjson.Result, value gjson.Result) bool {
			role := value.Get("role").String()
			valid := role == "user" || role == "model"
			if role == "" || !valid {
				var newRole string
				if prevRole == "" {
					newRole = "user"
				} else if prevRole == "user" {
					newRole = "model"
				} else {
					newRole = "user"
				}
				contentRaw, errSetRole := sjson.SetBytes([]byte(value.Raw), "role", newRole)
				if errSetRole == nil {
					roleEdits = append(roleEdits, rawJSONEdit{
						start: value.Index,
						end:   value.Index + len(value.Raw),
						path:  fmt.Sprintf("request.contents.%d", idx),
						raw:   contentRaw,
					})
				}
				role = newRole
			}
			prevRole = role
			idx++
			return true
		})
		rawJSON = applyRawJSONEdits(rawJSON, roleEdits)
	}

	// Tool declarations are inspected from a single snapshot: renaming
	// "parameters" in one declaration never affects the other declarations, so
	// each declaration is rewritten on its own and spliced back at once.
	toolsResult := gjson.GetBytes(rawJSON, "request.tools")
	if toolsResult.Exists() && toolsResult.IsArray() {
		var declarationEdits []rawJSONEdit
		toolResults := toolsResult.Array()
		for i := 0; i < len(toolResults); i++ {
			functionDeclarationsResult := toolResults[i].Get("function_declarations")
			if functionDeclarationsResult.Exists() && functionDeclarationsResult.IsArray() {
				functionDeclarationsResults := functionDeclarationsResult.Array()
				for j := 0; j < len(functionDeclarationsResults); j++ {
					declaration := functionDeclarationsResults[j]
					if !declaration.Get("parameters").Exists() {
						continue
					}
					renamed, errRename := util.RenameKeyBytes([]byte(declaration.Raw), "parameters", "parametersJsonSchema")
					if errRename != nil {
						log.WithError(errRename).Debug("antigravity gemini translator: failed to rename function declaration parameters")
						continue
					}
					declarationEdits = append(declarationEdits, rawJSONEdit{
						start: declaration.Index,
						end:   declaration.Index + len(declaration.Raw),
						path:  fmt.Sprintf("request.tools.%d.function_declarations.%d", i, j),
						raw:   renamed,
					})
				}
			}
		}
		rawJSON = applyRawJSONEdits(rawJSON, declarationEdits)
	}

	if strings.Contains(strings.ToLower(modelName), "claude") {
		rawJSON = sanitizeAntigravityClaudeGeminiRequestSignatures(modelName, rawJSON)
	} else {
		rawJSON = signature.SanitizeGeminiRequestThoughtSignatures(rawJSON, "request.contents")
	}

	return common.AttachDefaultSafetySettings(rawJSON, "request.safetySettings")
}

// rawJSONEdit records a rewritten JSON value together with its byte range and
// its path inside the document it came from.
type rawJSONEdit struct {
	start int
	end   int
	path  string
	raw   []byte
}

// applyRawJSONEdits splices non-overlapping rewrites, ordered by offset, into
// the document with a single copy. Every sjson write allocates a full copy of
// the document, so rewriting sub-objects and merging them here keeps the cost
// linear in the document size. When gjson did not report usable offsets the
// edits are applied by path instead, which is slower but equivalent.
func applyRawJSONEdits(document []byte, edits []rawJSONEdit) []byte {
	if len(edits) == 0 {
		return document
	}

	grown := 0
	cursor := 0
	for _, edit := range edits {
		if edit.start <= 0 || edit.start < cursor || edit.end > len(document) || document[edit.start] != '{' {
			for _, fallback := range edits {
				document, _ = sjson.SetRawBytes(document, fallback.path, fallback.raw)
			}
			return document
		}
		cursor = edit.end
		grown += len(edit.raw) - (edit.end - edit.start)
	}

	out := make([]byte, 0, len(document)+grown)
	cursor = 0
	for _, edit := range edits {
		out = append(out, document[cursor:edit.start]...)
		out = append(out, edit.raw...)
		cursor = edit.end
	}
	return append(out, document[cursor:]...)
}

func sanitizeAntigravityClaudeGeminiRequestSignatures(modelName string, rawJSON []byte) []byte {
	var root map[string]any
	if err := json.Unmarshal(rawJSON, &root); err != nil {
		log.WithError(err).Debug("antigravity gemini translator: failed to parse request for Claude signature sanitize")
		return rawJSON
	}

	request, ok := root["request"].(map[string]any)
	if !ok {
		return rawJSON
	}
	contents, ok := request["contents"].([]any)
	if !ok {
		return rawJSON
	}

	changed := false
	rewrittenContents := make([]any, 0, len(contents))
	for contentIndex, contentValue := range contents {
		content, ok := contentValue.(map[string]any)
		if !ok {
			rewrittenContents = append(rewrittenContents, contentValue)
			continue
		}

		parts, ok := content["parts"].([]any)
		if !ok {
			rewrittenContents = append(rewrittenContents, content)
			continue
		}

		isModelTurn := content["role"] == "model"
		rewrittenParts := make([]any, 0, len(parts))
		for partIndex, partValue := range parts {
			part, ok := partValue.(map[string]any)
			if !ok {
				rewrittenParts = append(rewrittenParts, partValue)
				continue
			}

			rawSignature, hasSignature := antigravityClaudeGeminiPartThoughtSignature(part)
			if hasFunctionResponsePart(part) {
				if hasSignature {
					changed = true
					deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_signature", "functionResponse parts cannot replay Claude thinking signatures", contentIndex, partIndex, rawSignature)
				}
				rewrittenParts = append(rewrittenParts, part)
				continue
			}
			if !isModelTurn {
				if hasSignature {
					changed = true
					deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_signature", "non-model parts cannot replay Claude thinking signatures", contentIndex, partIndex, rawSignature)
				}
				rewrittenParts = append(rewrittenParts, part)
				continue
			}

			if part["thought"] == true {
				normalized, compatible := signature.CompatibleAntigravityClaudeThinkingSignature(rawSignature)
				if !compatible {
					changed = true
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_thinking_block", "missing_or_incompatible_signature", contentIndex, partIndex, rawSignature)
					continue
				}
				if text, _ := part["text"].(string); strings.TrimSpace(text) == "" {
					changed = true
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_thinking_block", "empty_thinking_text", contentIndex, partIndex, rawSignature)
					continue
				}
				if normalized != rawSignature {
					changed = true
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "normalize_signature", "compatible_claude_signature", contentIndex, partIndex, rawSignature)
				}
				deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
				part["thoughtSignature"] = normalized
				rewrittenParts = append(rewrittenParts, part)
				continue
			}

			if hasSignature {
				changed = true
				deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
				logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_signature", "non-thinking parts should not carry Claude thinking signatures", contentIndex, partIndex, rawSignature)
			}
			rewrittenParts = append(rewrittenParts, part)
		}

		if len(rewrittenParts) == 0 {
			changed = true
			continue
		}
		content["parts"] = rewrittenParts
		rewrittenContents = append(rewrittenContents, content)
	}

	if !changed {
		return rawJSON
	}
	request["contents"] = rewrittenContents
	out, err := json.Marshal(root)
	if err != nil {
		log.WithError(err).Debug("antigravity gemini translator: failed to marshal Claude signature sanitize")
		return rawJSON
	}
	return out
}

func antigravityClaudeGeminiPartThoughtSignature(part map[string]any) (string, bool) {
	for _, path := range [][]string{
		{"thoughtSignature"},
		{"thought_signature"},
		{"functionCall", "thoughtSignature"},
		{"functionCall", "thought_signature"},
		{"functionResponse", "thoughtSignature"},
		{"functionResponse", "thought_signature"},
		{"extra_content", "google", "thought_signature"},
	} {
		if value, ok := stringAtPath(part, path...); ok {
			return value, true
		}
	}
	return "", false
}

func deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part map[string]any) {
	for _, path := range [][]string{
		{"thoughtSignature"},
		{"thought_signature"},
		{"functionCall", "thoughtSignature"},
		{"functionCall", "thought_signature"},
		{"functionResponse", "thoughtSignature"},
		{"functionResponse", "thought_signature"},
		{"extra_content", "google", "thought_signature"},
	} {
		deleteAtPath(part, path...)
	}
}

func hasFunctionResponsePart(part map[string]any) bool {
	_, ok := part["functionResponse"]
	if ok {
		return true
	}
	_, ok = part["function_response"]
	return ok
}

func stringAtPath(value map[string]any, path ...string) (string, bool) {
	var current any = value
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = m[key]
		if !ok {
			return "", false
		}
	}
	s, ok := current.(string)
	return s, ok
}

func deleteAtPath(value map[string]any, path ...string) {
	if len(path) == 0 {
		return
	}
	current := value
	for _, key := range path[:len(path)-1] {
		next, ok := current[key].(map[string]any)
		if !ok {
			return
		}
		current = next
	}
	delete(current, path[len(path)-1])
}

func logAntigravityClaudeGeminiSignatureSanitize(modelName, action, reason string, contentIndex, partIndex int, rawSignature string) {
	fields := log.Fields{
		"component":         "signature_sanitizer",
		"translator":        "antigravity_gemini",
		"target_provider":   string(signature.SignatureProviderClaude),
		"action":            action,
		"reason":            reason,
		"model":             modelName,
		"content_index":     contentIndex,
		"part_index":        partIndex,
		"has_signature":     strings.TrimSpace(rawSignature) != "",
		"signature_length":  len(strings.TrimSpace(rawSignature)),
		"detected_provider": string(signature.DetectSignatureProviderForBlock(rawSignature, signature.SignatureBlockKindClaudeThinking)),
	}
	log.WithFields(fields).Debug("antigravity gemini translator: sanitized Claude target thoughtSignature before upstream")
}

// FunctionCallGroup represents a group of function calls and their responses
type FunctionCallGroup struct {
	ResponsesNeeded int
	CallNames       []string // ordered function call names for backfilling empty response names
}

// parseFunctionResponseRaw attempts to normalize a function response part into a JSON object string.
// Falls back to a minimal "functionResponse" object when parsing fails.
// fallbackName is used when the response's own name is empty.
func parseFunctionResponseRaw(response gjson.Result, fallbackName string) string {
	if response.IsObject() && gjson.Valid(response.Raw) {
		raw := response.Raw
		name := response.Get("functionResponse.name").String()
		if strings.TrimSpace(name) == "" && fallbackName != "" {
			updated, _ := sjson.SetBytes([]byte(raw), "functionResponse.name", fallbackName)
			raw = string(updated)
		}
		return raw
	}

	log.Debugf("parse function response failed, using fallback")
	funcResp := response.Get("functionResponse")
	if funcResp.Exists() {
		fr := []byte(`{"functionResponse":{"name":"","response":{"result":""}}}`)
		name := funcResp.Get("name").String()
		if strings.TrimSpace(name) == "" {
			name = fallbackName
		}
		fr, _ = sjson.SetBytes(fr, "functionResponse.name", name)
		fr, _ = sjson.SetBytes(fr, "functionResponse.response.result", funcResp.Get("response").String())
		if id := funcResp.Get("id").String(); id != "" {
			fr, _ = sjson.SetBytes(fr, "functionResponse.id", id)
		}
		return string(fr)
	}

	useName := fallbackName
	if useName == "" {
		useName = "unknown"
	}
	fr := []byte(`{"functionResponse":{"name":"","response":{"result":""}}}`)
	fr, _ = sjson.SetBytes(fr, "functionResponse.name", useName)
	fr, _ = sjson.SetBytes(fr, "functionResponse.response.result", response.String())
	return string(fr)
}

// fixCLIToolResponse performs sophisticated tool response format conversion and grouping.
// This function transforms the CLI tool response format by intelligently grouping function calls
// with their corresponding responses, ensuring proper conversation flow and API compatibility.
// It converts from a linear format (1.json) to a grouped format (2.json) where function calls
// and their responses are properly associated and structured.
//
// Parameters:
//   - input: The input JSON string to be processed
//
// Returns:
//   - string: The processed JSON string with grouped function calls and responses
//   - error: An error if the processing fails
func fixCLIToolResponse(input string) (string, error) {
	result, err := fixCLIToolResponseBytes([]byte(input))
	if err != nil {
		return input, err
	}
	return string(result), nil
}

// fixCLIToolResponseBytes is the []byte implementation of fixCLIToolResponse.
// The rewritten contents are collected as raw JSON slices and serialized once,
// because appending them one by one with sjson copies the whole document per
// element. The input buffer is owned by this function and may be reused.
func fixCLIToolResponseBytes(input []byte) ([]byte, error) {
	// Extract the contents array which contains the conversation messages
	contents := gjson.GetBytes(input, "request.contents")
	if !contents.Exists() {
		// log.Debugf(input)
		return input, fmt.Errorf("contents not found in input")
	}

	// Initialize data structures for processing and grouping
	var rewrittenContents []string         // Raw JSON of the rebuilt contents array
	var pendingGroups []*FunctionCallGroup // Groups awaiting completion with responses
	var collectedResponses []gjson.Result  // Standalone responses to be matched

	// Process each content object in the conversation
	// This iterates through messages and groups function calls with their responses
	contents.ForEach(func(key, value gjson.Result) bool {
		role := value.Get("role").String()
		parts := value.Get("parts")

		// Check if this content has function responses
		var responsePartsInThisContent []gjson.Result
		parts.ForEach(func(_, part gjson.Result) bool {
			if part.Get("functionResponse").Exists() {
				responsePartsInThisContent = append(responsePartsInThisContent, part)
			}
			return true
		})

		// If this content has function responses, collect them
		if len(responsePartsInThisContent) > 0 {
			collectedResponses = append(collectedResponses, responsePartsInThisContent...)

			// Check if pending groups can be satisfied (FIFO: oldest group first)
			for len(pendingGroups) > 0 && len(collectedResponses) >= pendingGroups[0].ResponsesNeeded {
				group := pendingGroups[0]
				pendingGroups = pendingGroups[1:]

				// Take the needed responses for this group
				groupResponses := collectedResponses[:group.ResponsesNeeded]
				collectedResponses = collectedResponses[group.ResponsesNeeded:]

				// Create merged function response content
				if functionResponseContent := buildFunctionResponseContent(groupResponses, group.CallNames); functionResponseContent != "" {
					rewrittenContents = append(rewrittenContents, functionResponseContent)
				}
			}

			return true // Skip adding this content, responses are merged
		}

		// If this is a model with function calls, create a new group
		if role == "model" {
			var callNames []string
			parts.ForEach(func(_, part gjson.Result) bool {
				if part.Get("functionCall").Exists() {
					callNames = append(callNames, part.Get("functionCall.name").String())
				}
				return true
			})

			if len(callNames) > 0 {
				// Add the model content
				if !value.IsObject() {
					log.Warnf("failed to parse model content")
					return true
				}
				rewrittenContents = append(rewrittenContents, value.Raw)

				// Create a new group for tracking responses
				group := &FunctionCallGroup{
					ResponsesNeeded: len(callNames),
					CallNames:       callNames,
				}
				pendingGroups = append(pendingGroups, group)
			} else {
				// Regular model content without function calls
				if !value.IsObject() {
					log.Warnf("failed to parse content")
					return true
				}
				rewrittenContents = append(rewrittenContents, value.Raw)
			}
		} else {
			// Non-model content (user, etc.)
			if !value.IsObject() {
				log.Warnf("failed to parse content")
				return true
			}
			rewrittenContents = append(rewrittenContents, value.Raw)
		}

		return true
	})

	// Handle any remaining pending groups with remaining responses
	for _, group := range pendingGroups {
		if len(collectedResponses) >= group.ResponsesNeeded {
			groupResponses := collectedResponses[:group.ResponsesNeeded]
			collectedResponses = collectedResponses[group.ResponsesNeeded:]

			if functionResponseContent := buildFunctionResponseContent(groupResponses, group.CallNames); functionResponseContent != "" {
				rewrittenContents = append(rewrittenContents, functionResponseContent)
			}
		}
	}

	// Serialize the rebuilt contents array once and update the original JSON
	var contentsBuffer bytes.Buffer
	contentsBuffer.Grow(len(contents.Raw) + 64)
	contentsBuffer.WriteByte('[')
	for i, contentRaw := range rewrittenContents {
		if i > 0 {
			contentsBuffer.WriteByte(',')
		}
		contentsBuffer.WriteString(contentRaw)
	}
	contentsBuffer.WriteByte(']')

	result, _ := sjson.SetRawBytesOptions(input, "request.contents", contentsBuffer.Bytes(), &sjson.Options{Optimistic: true, ReplaceInPlace: true})

	return result, nil
}

// buildFunctionResponseContent merges a group of function responses into a
// single "function" role content. It returns an empty string when the group
// produced no usable part.
func buildFunctionResponseContent(groupResponses []gjson.Result, callNames []string) string {
	var builder strings.Builder
	builder.WriteString(`{"parts":[`)
	count := 0
	for ri, response := range groupResponses {
		partRaw := parseFunctionResponseRaw(response, callNames[ri])
		if partRaw == "" {
			continue
		}
		if count > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(partRaw)
		count++
	}
	if count == 0 {
		return ""
	}
	builder.WriteString(`],"role":"function"}`)
	return builder.String()
}
