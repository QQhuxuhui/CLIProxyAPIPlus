package helps

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// ReadResponsesBodyWithTTFT retains the original SSE bytes for non-stream translation
// while measuring token arrival with the same event classifier used by streaming callers.
func ReadResponsesBodyWithTTFT(body io.Reader, reporter *UsageReporter) ([]byte, error) {
	reader := bufio.NewReader(body)
	var payload []byte
	for {
		line, err := reader.ReadBytes('\n')
		payload = append(payload, line...)
		if bytes.HasPrefix(line, []byte("data:")) {
			ObserveResponsesTokenEvent(reporter, bytes.TrimSpace(line[len("data:"):]))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return payload, nil
			}
			return payload, err
		}
	}
}
