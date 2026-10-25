package helps

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type responsesBodyTestReader struct {
	chunks   []string
	before   func(int)
	index    int
	terminal error
}

func (r *responsesBodyTestReader) Read(p []byte) (int, error) {
	if r.index == len(r.chunks) {
		return 0, r.terminal
	}
	if r.before != nil {
		r.before(r.index)
	}
	n := copy(p, r.chunks[r.index])
	r.chunks[r.index] = r.chunks[r.index][n:]
	if r.chunks[r.index] == "" {
		r.index++
	}
	return n, nil
}

func TestReadResponsesBodyTTFTTracksTokenBeforeCompletion(t *testing.T) {
	reporter := &UsageReporter{}
	reporter.StartResponseTTFT()
	chunks := []string{
		"data: {\"type\":\"response.created\"}\r\n\r\n",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n",
		"data: {\"type\":\"response.completed\"}",
	}
	want := strings.Join(chunks, "")
	var measured time.Duration
	reader := &responsesBodyTestReader{chunks: chunks, terminal: io.EOF, before: func(i int) {
		if i == 1 {
			if reporter.IsTTFTSet() {
				t.Fatal("created event was counted as a token")
			}
			reporter.ttftStart = time.Now().Add(-5 * time.Second)
		}
		if i == 2 {
			if !reporter.IsTTFTSet() {
				t.Fatal("first token was not recorded before reading completion")
			}
			measured = reporter.ttftDuration()
			if measured < 5*time.Second {
				t.Fatalf("TTFT measured first packet: %v", measured)
			}
		}
	}}
	got, err := ReadResponsesBodyWithTTFT(reader, reporter)
	if err != nil || string(got) != want {
		t.Fatalf("body=%q err=%v", got, err)
	}
	if measured != reporter.ttftDuration() {
		t.Fatal("completion changed first-token timing")
	}
}

func TestReadResponsesBodyPreservesPartialReadError(t *testing.T) {
	broken := errors.New("connection interrupted")
	reader := &responsesBodyTestReader{chunks: []string{"data: partial"}, terminal: broken}
	got, err := ReadResponsesBodyWithTTFT(reader, nil)
	if string(got) != "data: partial" || !errors.Is(err, broken) {
		t.Fatalf("body=%q err=%v", got, err)
	}
}
