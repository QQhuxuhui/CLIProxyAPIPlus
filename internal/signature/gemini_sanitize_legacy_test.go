package signature

// Reference copies of the pre-optimization Gemini sanitizer. They are kept here
// verbatim so the optimized implementation can be proven to produce the exact
// same output. They are test-only and must not be used by production code.

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func legacySanitizeGeminiRequestThoughtSignatures(payload []byte, contentsPath string) []byte {
	contentsPath = strings.TrimSpace(contentsPath)
	if contentsPath == "" {
		contentsPath = "contents"
	}

	contents := gjson.GetBytes(payload, contentsPath)
	if !contents.IsArray() {
		return payload
	}

	contents.ForEach(func(contentIdx, content gjson.Result) bool {
		isModelTurn := content.Get("role").String() == "model"
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}

		parts.ForEach(func(partIdx, part gjson.Result) bool {
			partPath := fmt.Sprintf("%s.%d.parts.%d", contentsPath, contentIdx.Int(), partIdx.Int())
			if part.Get("functionResponse").Exists() {
				_, hadSignature := geminiPartThoughtSignature(part)
				payload = legacyDeleteGeminiPartThoughtSignatureFields(payload, partPath)
				if hadSignature {
					logGeminiThoughtSignatureSanitize(contentsPath, int(contentIdx.Int()), int(partIdx.Int()), SignatureCompatibilityDecision{
						TargetProvider: SignatureProviderGemini,
						BlockKind:      SignatureBlockKindGeminiModelPart,
						Action:         SignatureActionDropSignature,
						Reason:         "user-turn functionResponse parts cannot replay thought signatures",
					}, "", true)
				}
				return true
			}
			if !isModelTurn {
				return true
			}

			hasFunctionCall := part.Get("functionCall").Exists()
			hasThought := part.Get("thought").Exists()
			rawSignature, hasSignature := geminiPartThoughtSignature(part)
			if !hasFunctionCall && !hasThought && !hasSignature {
				return true
			}

			blockKind := SignatureBlockKindGeminiModelPart
			if hasFunctionCall {
				blockKind = SignatureBlockKindGeminiFunctionCall
			}
			payload = legacyDeleteGeminiPartThoughtSignatureFields(payload, partPath)
			decision := DecideSignatureCompatibility(SignatureProviderGemini, rawSignature, blockKind)
			replaySignature := GeminiReplaySignatureOrBypass(rawSignature, blockKind)
			payload, _ = sjson.SetBytes(payload, partPath+".thoughtSignature", replaySignature)
			if decision.Action != SignatureActionPreserve {
				logGeminiThoughtSignatureSanitize(contentsPath, int(contentIdx.Int()), int(partIdx.Int()), decision, rawSignature, hasSignature)
			}
			return true
		})
		return true
	})

	return payload
}

func legacyDeleteGeminiPartThoughtSignatureFields(payload []byte, partPath string) []byte {
	for _, path := range []string{
		"thoughtSignature",
		"thought_signature",
		"functionCall.thoughtSignature",
		"functionCall.thought_signature",
		"functionResponse.thoughtSignature",
		"functionResponse.thought_signature",
		"extra_content.google.thought_signature",
	} {
		payload, _ = sjson.DeleteBytes(payload, partPath+"."+path)
	}
	return payload
}
