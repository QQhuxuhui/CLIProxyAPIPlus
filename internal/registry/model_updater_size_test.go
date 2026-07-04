package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchModelsFromRemote_RejectsOversizeBody (S11) asserts that a response body
// larger than maxModelsResponseBytes is rejected (not parsed) even though its content
// is otherwise valid JSON, guarding against memory exhaustion from a hostile host.
func TestFetchModelsFromRemote_RejectsOversizeBody(t *testing.T) {
	// A valid, empty models catalog ("{}") padded with whitespace past the size cap.
	// json.Unmarshal would accept this if it were ever read, so a non-nil result would
	// indicate the size cap was not enforced.
	oversized := "{" + strings.Repeat(" ", int(maxModelsResponseBytes)+100) + "}"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(oversized))
	}))
	defer srv.Close()

	restore := swapModelsURLs([]string{srv.URL})
	defer restore()

	parsed, url := fetchModelsFromRemote(context.Background())
	if parsed != nil {
		t.Fatalf("expected oversize response to be rejected (nil), got parsed=%v from %s", parsed, url)
	}
}

// TestFetchModelsFromRemote_AcceptsSmallBody is a control ensuring the size cap does
// not break a normal, small, valid response.
func TestFetchModelsFromRemote_AcceptsSmallBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	restore := swapModelsURLs([]string{srv.URL})
	defer restore()

	parsed, url := fetchModelsFromRemote(context.Background())
	if parsed == nil {
		t.Fatalf("expected a small valid response to be accepted, got nil")
	}
	if url != srv.URL {
		t.Fatalf("expected fetch URL %s, got %s", srv.URL, url)
	}
}

func swapModelsURLs(urls []string) func() {
	prev := modelsURLs
	modelsURLs = urls
	return func() { modelsURLs = prev }
}
