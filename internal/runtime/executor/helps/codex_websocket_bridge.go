package helps

import (
	"bytes"
	"hash/fnv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CodexWebsocketBridgeBucketHit reports whether a request falls inside the bridge canary share.
// The bucket is a stable hash of the request ID, so a logical request keeps the same transport
// across credential retries. A request without an ID is only bridged at 100 percent, because
// there is nothing stable to hash and a fresh draw per attempt could flip the transport.
func CodexWebsocketBridgeBucketHit(requestID string, percent int) bool {
	if percent <= 0 {
		return false
	}
	if percent >= 100 {
		return true
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return false
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(requestID))
	return int(h.Sum32()%100) < percent
}

// IsCodexWebsocketBridgeDroppedEvent reports whether an upstream websocket frame is specific to
// the websocket transport and must not reach an HTTP downstream: codex.* frames carry the plan
// and quota of the subscription account plus backend headers, and responsesapi.* frames carry
// websocket timing. HTTP SSE streams do not deliver them.
func IsCodexWebsocketBridgeDroppedEvent(payload []byte) bool {
	eventType := gjson.GetBytes(payload, "type").String()
	return strings.HasPrefix(eventType, "codex.") || strings.HasPrefix(eventType, "responsesapi.")
}

// FrameCodexWebsocketBridgeChunks adds the SSE event line that the HTTP upstream sends in front
// of every data line, so a bridged stream has the same framing as an HTTP one. Chunks that are
// not a data line, or whose JSON has no type, are returned unchanged.
func FrameCodexWebsocketBridgeChunks(chunks [][]byte) [][]byte {
	for i, chunk := range chunks {
		if !bytes.HasPrefix(chunk, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(chunk[len("data:"):])
		eventType := gjson.GetBytes(data, "type").String()
		if eventType == "" {
			continue
		}
		framed := make([]byte, 0, len("event: \n")+len(eventType)+len(chunk))
		framed = append(framed, "event: "...)
		framed = append(framed, eventType...)
		framed = append(framed, '\n')
		framed = append(framed, chunk...)
		chunks[i] = framed
	}
	return chunks
}

// CleanCodexBridgeResponsesBody removes the request fields the HTTP/SSE Codex path removes
// before sending, so a bridged request asks the upstream for the same thing an HTTP one does.
// It keeps only stream_options.reasoning_summary_delivery.
func CleanCodexBridgeResponsesBody(body []byte) []byte {
	for _, field := range []string{"previous_response_id", "generate", "prompt_cache_retention", "safety_identifier"} {
		if gjson.GetBytes(body, field).Exists() {
			body, _ = sjson.DeleteBytes(body, field)
		}
	}
	streamOptions := gjson.GetBytes(body, "stream_options")
	if !streamOptions.Exists() {
		return body
	}
	reasoningSummaryDelivery := streamOptions.Get("reasoning_summary_delivery")
	body, _ = sjson.DeleteBytes(body, "stream_options")
	if reasoningSummaryDelivery.Exists() {
		body, _ = sjson.SetBytes(body, "stream_options.reasoning_summary_delivery", reasoningSummaryDelivery.Value())
	}
	return body
}
