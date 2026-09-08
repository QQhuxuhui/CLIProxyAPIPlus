package signature

import (
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// geminiThoughtSignaturePaths lists every part-relative path that may carry a
// thought signature, in the order the legacy implementation inspected them.
var geminiThoughtSignaturePaths = [...]string{
	"thoughtSignature",
	"thought_signature",
	"functionCall.thoughtSignature",
	"functionCall.thought_signature",
	"functionResponse.thoughtSignature",
	"functionResponse.thought_signature",
	"extra_content.google.thought_signature",
}

// geminiPartEdit records a rewritten part together with its byte range inside
// the original payload, so all rewrites can be spliced in a single pass.
type geminiPartEdit struct {
	start int
	end   int
	path  string
	raw   []byte
}

// GeminiReplaySignatureOrBypass returns a Gemini-replayable thoughtSignature.
// Compatible Gemini signatures are normalized and preserved. Missing, unknown,
// or cross-provider signatures are replaced with Gemini's bypass sentinel.
func GeminiReplaySignatureOrBypass(rawSignature string, blockKind SignatureBlockKind) string {
	if signature, ok := CompatibleSignatureForProviderBlock(SignatureProviderGemini, rawSignature, blockKind); ok {
		return signature
	}
	decision := DecideSignatureCompatibility(SignatureProviderGemini, rawSignature, blockKind)
	if decision.Action == SignatureActionReplaceWithGeminiBypass && decision.ReplacementSignature != "" {
		return decision.ReplacementSignature
	}
	return GeminiSkipThoughtSignatureValidator
}

// SanitizeGeminiRequestThoughtSignatures applies Gemini replay policy to a
// Gemini-shaped request. Model-turn functionCall, thought, and signed parts keep
// compatible Gemini signatures and use the bypass sentinel otherwise. User-turn
// functionResponse parts must not carry thoughtSignature fields.
//
// Every sjson write allocates a full copy of the document it is given, so the
// rewrites are applied to the individual part objects (a few hundred bytes each)
// and spliced into the payload once at the end. The caller's buffer is only read.
func SanitizeGeminiRequestThoughtSignatures(payload []byte, contentsPath string) []byte {
	contentsPath = strings.TrimSpace(contentsPath)
	if contentsPath == "" {
		contentsPath = "contents"
	}

	contents := gjson.GetBytes(payload, contentsPath)
	if !contents.IsArray() {
		return payload
	}

	var edits []geminiPartEdit
	contents.ForEach(func(contentIdx, content gjson.Result) bool {
		isModelTurn := content.Get("role").String() == "model"
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}

		parts.ForEach(func(partIdx, part gjson.Result) bool {
			partRaw, changed := sanitizeGeminiPart(part, isModelTurn, contentsPath, int(contentIdx.Int()), int(partIdx.Int()))
			if !changed {
				return true
			}
			edits = append(edits, geminiPartEdit{
				start: part.Index,
				end:   part.Index + len(part.Raw),
				path:  fmt.Sprintf("%s.%d.parts.%d", contentsPath, contentIdx.Int(), partIdx.Int()),
				raw:   partRaw,
			})
			return true
		})
		return true
	})

	return applyGeminiPartEdits(payload, edits)
}

// sanitizeGeminiPart rewrites a single part object and reports whether anything
// changed. The returned bytes are only valid when changed is true.
func sanitizeGeminiPart(part gjson.Result, isModelTurn bool, contentsPath string, contentIndex, partIndex int) ([]byte, bool) {
	if part.Get("functionResponse").Exists() {
		_, hadSignature := geminiPartThoughtSignature(part)
		if !hadSignature {
			return nil, false
		}
		partRaw := deleteGeminiPartThoughtSignatureFields(part, []byte(part.Raw))
		logGeminiThoughtSignatureSanitize(contentsPath, contentIndex, partIndex, SignatureCompatibilityDecision{
			TargetProvider: SignatureProviderGemini,
			BlockKind:      SignatureBlockKindGeminiModelPart,
			Action:         SignatureActionDropSignature,
			Reason:         "user-turn functionResponse parts cannot replay thought signatures",
		}, "", true)
		return partRaw, true
	}
	if !isModelTurn {
		return nil, false
	}

	hasFunctionCall := part.Get("functionCall").Exists()
	hasThought := part.Get("thought").Exists()
	rawSignature, hasSignature := geminiPartThoughtSignature(part)
	if !hasFunctionCall && !hasThought && !hasSignature {
		return nil, false
	}

	blockKind := SignatureBlockKindGeminiModelPart
	if hasFunctionCall {
		blockKind = SignatureBlockKindGeminiFunctionCall
	}
	partRaw := deleteGeminiPartThoughtSignatureFields(part, []byte(part.Raw))
	decision := DecideSignatureCompatibility(SignatureProviderGemini, rawSignature, blockKind)
	replaySignature := GeminiReplaySignatureOrBypass(rawSignature, blockKind)
	partRaw, _ = sjson.SetBytes(partRaw, "thoughtSignature", replaySignature)
	if decision.Action != SignatureActionPreserve {
		logGeminiThoughtSignatureSanitize(contentsPath, contentIndex, partIndex, decision, rawSignature, hasSignature)
	}
	return partRaw, true
}

// applyGeminiPartEdits merges the rewritten parts back into the payload. The
// parts are visited in document order and never overlap, so a single copy pass
// is enough. When gjson could not report a usable offset the edit falls back to
// an sjson write on the whole document.
func applyGeminiPartEdits(payload []byte, edits []geminiPartEdit) []byte {
	if len(edits) == 0 {
		return payload
	}

	grown := 0
	cursor := 0
	for _, edit := range edits {
		if edit.start <= 0 || edit.start < cursor || edit.end > len(payload) || payload[edit.start] != '{' {
			return applyGeminiPartEditsByPath(payload, edits)
		}
		cursor = edit.end
		grown += len(edit.raw) - (edit.end - edit.start)
	}

	out := make([]byte, 0, len(payload)+grown)
	cursor = 0
	for _, edit := range edits {
		out = append(out, payload[cursor:edit.start]...)
		out = append(out, edit.raw...)
		cursor = edit.end
	}
	out = append(out, payload[cursor:]...)
	return out
}

// applyGeminiPartEditsByPath is the defensive fallback used when part offsets
// are unavailable. It is correct but allocates a full document copy per edit.
func applyGeminiPartEditsByPath(payload []byte, edits []geminiPartEdit) []byte {
	for _, edit := range edits {
		payload, _ = sjson.SetRawBytes(payload, edit.path, edit.raw)
	}
	return payload
}

func logGeminiThoughtSignatureSanitize(contentsPath string, contentIndex, partIndex int, decision SignatureCompatibilityDecision, rawSignature string, hasSignature bool) {
	log.WithFields(log.Fields{
		"component":         "signature_sanitizer",
		"target_provider":   string(SignatureProviderGemini),
		"action":            string(decision.Action),
		"reason":            decision.Reason,
		"contents_path":     contentsPath,
		"content_index":     contentIndex,
		"part_index":        partIndex,
		"block_kind":        string(decision.BlockKind),
		"detected_provider": string(decision.DetectedProvider),
		"has_signature":     hasSignature,
		"signature_length":  len(strings.TrimSpace(rawSignature)),
	}).Debug("gemini request: sanitized thoughtSignature before upstream")
}

func geminiPartThoughtSignature(part gjson.Result) (string, bool) {
	for _, path := range geminiThoughtSignaturePaths {
		result := part.Get(path)
		if result.Exists() {
			return result.String(), true
		}
	}
	return "", false
}

// deleteGeminiPartThoughtSignatureFields removes every known thought signature
// field from a single part object. sjson allocates a fresh buffer even when a
// delete is a no-op, so only the paths that actually exist are deleted. The
// listed paths live in disjoint subtrees, which keeps the existence checks valid
// against the original part while partRaw shrinks.
func deleteGeminiPartThoughtSignatureFields(part gjson.Result, partRaw []byte) []byte {
	for _, path := range geminiThoughtSignaturePaths {
		if !part.Get(path).Exists() {
			continue
		}
		partRaw, _ = sjson.DeleteBytes(partRaw, path)
	}
	return partRaw
}
