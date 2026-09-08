package gemini

// Reference copies of the pre-optimization Antigravity request translator. They
// are kept verbatim so the optimized implementation can be proven to produce the
// same output. They are test-only and must not be used by production code.

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/translator/gemini/common"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func legacyConvertGeminiRequestToAntigravity(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	template := `{"project":"","request":{},"model":""}`
	templateBytes, _ := sjson.SetRawBytes([]byte(template), "request", rawJSON)
	templateBytes, _ = sjson.SetBytes(templateBytes, "model", modelName)
	template = string(templateBytes)
	template, _ = sjson.Delete(template, "request.model")

	template, errFixCLIToolResponse := legacyFixCLIToolResponse(template)
	if errFixCLIToolResponse != nil {
		return []byte{}
	}

	systemInstructionResult := gjson.Get(template, "request.system_instruction")
	if systemInstructionResult.Exists() {
		templateBytes, _ = sjson.SetRawBytes([]byte(template), "request.systemInstruction", []byte(systemInstructionResult.Raw))
		template = string(templateBytes)
		template, _ = sjson.Delete(template, "request.system_instruction")
	}
	rawJSON = []byte(template)

	// Normalize roles in request.contents: default to valid values if missing/invalid
	contents := gjson.GetBytes(rawJSON, "request.contents")
	if contents.Exists() {
		prevRole := ""
		idx := 0
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
				path := fmt.Sprintf("request.contents.%d.role", idx)
				rawJSON, _ = sjson.SetBytes(rawJSON, path, newRole)
				role = newRole
			}
			prevRole = role
			idx++
			return true
		})
	}

	toolsResult := gjson.GetBytes(rawJSON, "request.tools")
	if toolsResult.Exists() && toolsResult.IsArray() {
		toolResults := toolsResult.Array()
		for i := 0; i < len(toolResults); i++ {
			functionDeclarationsResult := gjson.GetBytes(rawJSON, fmt.Sprintf("request.tools.%d.function_declarations", i))
			if functionDeclarationsResult.Exists() && functionDeclarationsResult.IsArray() {
				functionDeclarationsResults := functionDeclarationsResult.Array()
				for j := 0; j < len(functionDeclarationsResults); j++ {
					parametersResult := gjson.GetBytes(rawJSON, fmt.Sprintf("request.tools.%d.function_declarations.%d.parameters", i, j))
					if parametersResult.Exists() {
						strJson, _ := legacyRenameKey(string(rawJSON), fmt.Sprintf("request.tools.%d.function_declarations.%d.parameters", i, j), fmt.Sprintf("request.tools.%d.function_declarations.%d.parametersJsonSchema", i, j))
						rawJSON = []byte(strJson)
					}
				}
			}
		}
	}

	if strings.Contains(strings.ToLower(modelName), "claude") {
		rawJSON = sanitizeAntigravityClaudeGeminiRequestSignatures(modelName, rawJSON)
	} else {
		rawJSON = signature.SanitizeGeminiRequestThoughtSignatures(rawJSON, "request.contents")
	}

	return common.AttachDefaultSafetySettings(rawJSON, "request.safetySettings")
}

// legacyRenameKey is the pre-optimization util.RenameKey implementation.
func legacyRenameKey(jsonStr, oldKeyPath, newKeyPath string) (string, error) {
	value := gjson.Get(jsonStr, oldKeyPath)

	if !value.Exists() {
		return "", fmt.Errorf("old key '%s' does not exist", oldKeyPath)
	}

	interimJSON, errSet := sjson.SetRawBytes([]byte(jsonStr), newKeyPath, []byte(value.Raw))
	if errSet != nil {
		return "", fmt.Errorf("failed to set new key '%s': %w", newKeyPath, errSet)
	}

	finalJSON, errDelete := sjson.DeleteBytes(interimJSON, oldKeyPath)
	if errDelete != nil {
		return "", fmt.Errorf("failed to delete old key '%s': %w", oldKeyPath, errDelete)
	}

	return string(finalJSON), nil
}

func legacyFixCLIToolResponse(input string) (string, error) {
	// Parse the input JSON to extract the conversation structure
	parsed := gjson.Parse(input)

	// Extract the contents array which contains the conversation messages
	contents := parsed.Get("request.contents")
	if !contents.Exists() {
		return input, fmt.Errorf("contents not found in input")
	}

	// Initialize data structures for processing and grouping
	contentsWrapper := []byte(`{"contents":[]}`)
	var pendingGroups []*FunctionCallGroup // Groups awaiting completion with responses
	var collectedResponses []gjson.Result  // Standalone responses to be matched

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

				groupResponses := collectedResponses[:group.ResponsesNeeded]
				collectedResponses = collectedResponses[group.ResponsesNeeded:]

				functionResponseContent := []byte(`{"parts":[],"role":"function"}`)
				for ri, response := range groupResponses {
					partRaw := parseFunctionResponseRaw(response, group.CallNames[ri])
					if partRaw != "" {
						functionResponseContent, _ = sjson.SetRawBytes(functionResponseContent, "parts.-1", []byte(partRaw))
					}
				}

				if gjson.GetBytes(functionResponseContent, "parts.#").Int() > 0 {
					contentsWrapper, _ = sjson.SetRawBytes(contentsWrapper, "contents.-1", functionResponseContent)
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
				if !value.IsObject() {
					log.Warnf("failed to parse model content")
					return true
				}
				contentsWrapper, _ = sjson.SetRawBytes(contentsWrapper, "contents.-1", []byte(value.Raw))

				group := &FunctionCallGroup{
					ResponsesNeeded: len(callNames),
					CallNames:       callNames,
				}
				pendingGroups = append(pendingGroups, group)
			} else {
				if !value.IsObject() {
					log.Warnf("failed to parse content")
					return true
				}
				contentsWrapper, _ = sjson.SetRawBytes(contentsWrapper, "contents.-1", []byte(value.Raw))
			}
		} else {
			if !value.IsObject() {
				log.Warnf("failed to parse content")
				return true
			}
			contentsWrapper, _ = sjson.SetRawBytes(contentsWrapper, "contents.-1", []byte(value.Raw))
		}

		return true
	})

	// Handle any remaining pending groups with remaining responses
	for _, group := range pendingGroups {
		if len(collectedResponses) >= group.ResponsesNeeded {
			groupResponses := collectedResponses[:group.ResponsesNeeded]
			collectedResponses = collectedResponses[group.ResponsesNeeded:]

			functionResponseContent := []byte(`{"parts":[],"role":"function"}`)
			for ri, response := range groupResponses {
				partRaw := parseFunctionResponseRaw(response, group.CallNames[ri])
				if partRaw != "" {
					functionResponseContent, _ = sjson.SetRawBytes(functionResponseContent, "parts.-1", []byte(partRaw))
				}
			}

			if gjson.GetBytes(functionResponseContent, "parts.#").Int() > 0 {
				contentsWrapper, _ = sjson.SetRawBytes(contentsWrapper, "contents.-1", functionResponseContent)
			}
		}
	}

	// Update the original JSON with the new contents
	result, _ := sjson.SetRawBytes([]byte(input), "request.contents", []byte(gjson.GetBytes(contentsWrapper, "contents").Raw))

	return string(result), nil
}
