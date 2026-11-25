package helps

import (
	"bytes"
	"encoding/json"
	"strings"
)

// codexIdentityProtectedKeys are the JSON fields whose string values carry protocol meaning
// (event type, error code, status). An identity value that happens to equal or contain one of
// these strings must not rewrite them, or the frame would be taken for something else.
var codexIdentityProtectedKeys = map[string]struct{}{
	"type":   {},
	"event":  {},
	"object": {},
	"status": {},
	"code":   {},
}

// ReplaceCodexIdentityInPayload replaces from with to inside the JSON string values of an
// upstream payload: a JSON document, an SSE data line, or a full SSE body. Object keys and
// protocol fields (see codexIdentityProtectedKeys) are left untouched, and replaced values are
// re-encoded, so a value containing quotes or backslashes keeps the JSON valid. Any other payload
// (plain text, even with quotes in it) is replaced byte-wise, except an SSE event line.
func ReplaceCodexIdentityInPayload(payload []byte, from, to string) []byte {
	if len(payload) == 0 || from == "" || from == to {
		return payload
	}
	if !codexIdentityNeedsEscape(from) && !bytes.Contains(payload, []byte(from)) {
		return payload
	}
	trimmed := bytes.TrimSpace(payload)
	if !codexIdentityStructuredPayload(trimmed) {
		if bytes.HasPrefix(trimmed, []byte("event:")) {
			return payload
		}
		return bytes.ReplaceAll(payload, []byte(from), []byte(to))
	}

	var out []byte
	last := 0
	pendingKey := ""
	expectValue := false
	for i := 0; i < len(payload); i++ {
		c := payload[i]
		if c != '"' {
			switch c {
			case ':':
				expectValue = true
			case ' ', '\t', '\r', '\n':
			default:
				expectValue = false
			}
			continue
		}
		end := codexIdentityStringEnd(payload, i)
		if end < 0 {
			break
		}
		token := payload[i : end+1]
		next := end + 1
		for next < len(payload) && (payload[next] == ' ' || payload[next] == '\t' || payload[next] == '\r' || payload[next] == '\n') {
			next++
		}
		isKey := next < len(payload) && payload[next] == ':'
		var decoded string
		decodedOK := json.Unmarshal(token, &decoded) == nil
		if isKey {
			pendingKey = ""
			if decodedOK {
				pendingKey = decoded
			}
			expectValue = false
		} else {
			_, protected := codexIdentityProtectedKeys[pendingKey]
			protected = protected && expectValue
			expectValue = false
			if decodedOK && !protected && strings.Contains(decoded, from) {
				if encoded, ok := codexIdentityEncodeString(strings.ReplaceAll(decoded, from, to)); ok {
					if out == nil {
						out = make([]byte, 0, len(payload)+len(to))
					}
					out = append(out, payload[last:i]...)
					out = append(out, encoded...)
					last = end + 1
				}
			}
		}
		i = end
	}
	if out == nil {
		return payload
	}
	return append(out, payload[last:]...)
}

// codexIdentityStructuredPayload reports whether payload is JSON or carries SSE data lines, the
// shapes whose string values are replaced one by one.
func codexIdentityStructuredPayload(trimmed []byte) bool {
	return bytes.HasPrefix(trimmed, []byte("data:")) || bytes.Contains(trimmed, []byte("\ndata:")) || json.Valid(trimmed)
}

// codexIdentityStringEnd returns the index of the quote closing the JSON string that starts at
// start, or -1 when the string is not terminated.
func codexIdentityStringEnd(payload []byte, start int) int {
	for j := start + 1; j < len(payload); j++ {
		switch payload[j] {
		case '\\':
			j++
		case '"':
			return j
		}
	}
	return -1
}

func codexIdentityEncodeString(value string) ([]byte, bool) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if errEncode := encoder.Encode(value); errEncode != nil {
		return nil, false
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), true
}

// codexIdentityNeedsEscape reports whether value may be written differently inside a JSON string
// (escaped, or written as \u by encoders that escape HTML or non-ASCII characters), so a raw byte
// search for it is not a reliable fast path.
func codexIdentityNeedsEscape(value string) bool {
	for i := 0; i < len(value); i++ {
		if c := value[i]; c == '"' || c == '\\' || c < 0x20 || c >= 0x80 || c == '/' || c == '<' || c == '>' || c == '&' || c == '\'' {
			return true
		}
	}
	return false
}
