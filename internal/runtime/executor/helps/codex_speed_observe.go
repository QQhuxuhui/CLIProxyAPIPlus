package helps

import (
	"bytes"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

// codexSpeedOutputEvents are the events that carry real generated output. Terminal events are
// deliberately absent: a completion without output says nothing about generation speed.
var codexSpeedOutputEvents = map[string]struct{}{
	"response.output_text.delta":             {},
	"response.text.delta":                    {},
	"response.refusal.delta":                 {},
	"response.function_call_arguments.delta": {},
	"response.custom_tool_call_input.delta":  {},
	"response.reasoning_summary_text.delta":  {},
	"response.reasoning_text.delta":          {},
	"response.reasoning.delta":               {},
}

// ObserveResponsesTimingEvent records when response.created and the first real output event
// arrive. It is shared by the HTTP, SSE and websocket Responses paths.
func ObserveResponsesTimingEvent(reporter *UsageReporter, payload []byte) {
	if reporter == nil || len(payload) == 0 {
		return
	}
	reporter.ttftMu.RLock()
	done := !reporter.speedCreatedAt.IsZero() && !reporter.speedOutputAt.IsZero()
	reporter.ttftMu.RUnlock()
	if done {
		return
	}
	eventType := gjson.GetBytes(payload, "type").String()
	_, isOutput := codexSpeedOutputEvents[eventType]
	if isOutput {
		// An empty delta carries no output; only a non-empty string marks the first output.
		delta := gjson.GetBytes(payload, "delta")
		isOutput = delta.Type == gjson.String && delta.Str != ""
	}
	if eventType != "response.created" && !isOutput {
		return
	}
	now := time.Now()
	reporter.ttftMu.Lock()
	defer reporter.ttftMu.Unlock()
	if eventType == "response.created" && reporter.speedCreatedAt.IsZero() {
		reporter.speedCreatedAt = now
	}
	if isOutput && reporter.speedOutputAt.IsZero() {
		reporter.speedOutputAt = now
	}
}

// SetHasImageInput marks the upstream request as carrying image input, which excludes it from
// speed samples.
func (r *UsageReporter) SetHasImageInput(hasImage bool) {
	if r == nil {
		return
	}
	r.ttftMu.Lock()
	r.speedHasImage = hasImage
	r.ttftMu.Unlock()
}

// CodexRequestHasImageInput reports whether a Codex request body contains image input.
func CodexRequestHasImageInput(body []byte) bool {
	return bytes.Contains(body, []byte(`"input_image"`))
}

// codexSpeedGap returns created-to-first-output and send-to-created, or zero when either
// event is missing.
func (r *UsageReporter) codexSpeedGap() (gap, preCreated time.Duration, hasImage bool) {
	r.ttftMu.RLock()
	defer r.ttftMu.RUnlock()
	hasImage = r.speedHasImage
	if r.speedCreatedAt.IsZero() || r.speedOutputAt.IsZero() || !r.speedOutputAt.After(r.speedCreatedAt) {
		return 0, 0, hasImage
	}
	gap = r.speedOutputAt.Sub(r.speedCreatedAt)
	if !r.speedSentAt.IsZero() && r.speedCreatedAt.After(r.speedSentAt) {
		preCreated = r.speedCreatedAt.Sub(r.speedSentAt)
	}
	return gap, preCreated, hasImage
}

// codexSpeedOverload reports an upstream overload failure.
func codexSpeedOverload(record usage.Record) bool {
	if !record.Failed {
		return false
	}
	switch record.Fail.StatusCode {
	case 502, 503, 529:
		return true
	}
	return strings.Contains(record.Fail.Body, "server_is_overloaded")
}

// observeCodexSpeed feeds a published Codex attempt into the speed tracker.
func (r *UsageReporter) observeCodexSpeed(record usage.Record) {
	if r == nil || !strings.EqualFold(r.provider, "codex") || record.AdditionalModel || !usage.GenerateEnabled(record.Generate) || record.AuthID == "" {
		return
	}
	gap, preCreated, hasImage := r.codexSpeedGap()
	cliproxyauth.CodexSpeed().ObserveAttempt(cliproxyauth.CodexSpeedAttempt{
		AuthID:          record.AuthID,
		Success:         !record.Failed,
		Overload:        codexSpeedOverload(record),
		Gap:             gap,
		PreCreated:      preCreated,
		InputTokens:     record.Detail.InputTokens,
		ReasoningTokens: record.Detail.ReasoningTokens,
		HasImage:        hasImage,
		TTFT:            record.TTFT,
		Group:           r.speedGroup,
		Generation:      r.speedGeneration,
	})
}
