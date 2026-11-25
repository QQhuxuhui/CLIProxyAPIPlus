package auth

import (
	"errors"
	"testing"
)

type displayRewrittenErr struct {
	display  string
	original string
}

func (e displayRewrittenErr) Error() string   { return e.display }
func (e displayRewrittenErr) StatusCode() int { return 500 }
func (e displayRewrittenErr) ClassificationError() error {
	return errors.New(e.original)
}

func TestCodexFailureClassUsesClassificationText(t *testing.T) {
	err := displayRewrittenErr{
		display:  `{"error":{"type":"server_error","message":"you can 7f3c your request"}}`,
		original: `{"error":{"type":"server_error","message":"you can retry your request"}}`,
	}
	if class := codexFailureClass(err); class != "overload" {
		t.Fatalf("class = %q, want overload", class)
	}
}
