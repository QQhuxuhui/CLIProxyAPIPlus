package helps

import (
	"strings"

	"github.com/tidwall/gjson"
)

// CodexBootstrapReleaseReason returns a bounded label for the first visible event.
func CodexBootstrapReleaseReason(payload []byte) string {
	kind := gjson.GetBytes(payload, "type").String()
	item := gjson.GetBytes(payload, "item.type").String()
	if strings.HasPrefix(kind, "response.function_call_arguments.") || strings.HasSuffix(item, "_call") {
		return "tool"
	}
	switch kind {
	case "response.completed", "response.incomplete", "response.done":
		return "terminal"
	}
	if HasMeaningfulCodexOutputDelta(payload) {
		return "content"
	}
	return "unknown"
}
