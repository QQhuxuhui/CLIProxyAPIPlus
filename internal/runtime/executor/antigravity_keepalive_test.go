package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// These tests guard the connection-reuse half of the transport-pool work. The
// per-credential transport cache on its own is not enough: as long as any request
// path sets Request.Close, Go emits "Connection: close" and every attempt still
// pays a full TCP + TLS (+ SOCKS5, when a proxy is configured) handshake. On this
// deployment that handshake is the dominant per-attempt cost, so a regression here
// silently reverts most of the benefit while the cache still looks correct.
//
// The upstream commit that introduced the cache (c33a33e1, "reuse native upstream
// connections") removed Request.Close from buildRequest, CountTokens and
// HttpRequest in the same change; an earlier backport took only the cache and was
// caught by review rather than by a test.

func antigravityKeepaliveAuth(t *testing.T, baseURL string) *cliproxyauth.Auth {
	t.Helper()
	return &cliproxyauth.Auth{
		ID: "auth-keepalive",
		Attributes: map[string]string{
			"base_url": baseURL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
}

// remoteAddrCounter records the distinct client connections a test server saw.
// Go reuses the source port for the lifetime of a keep-alive connection, so a
// single distinct RemoteAddr across N sequential requests proves reuse.
type remoteAddrCounter struct {
	mu      sync.Mutex
	remotes map[string]int
}

func newRemoteAddrCounter() *remoteAddrCounter {
	return &remoteAddrCounter{remotes: make(map[string]int)}
}

func (c *remoteAddrCounter) record(addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remotes[addr]++
}

func (c *remoteAddrCounter) distinct() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.remotes)
}

func TestAntigravityExecuteReusesUpstreamConnection(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	counter := newRemoteAddrCounter()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.record(r.RemoteAddr)
		if r.Close {
			t.Errorf("upstream saw Connection: close, want a keep-alive request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`))
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{})
	auth := antigravityKeepaliveAuth(t, server.URL)

	const requests = 3
	for i := 0; i < requests; i++ {
		_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-sonnet-4-6",
			Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatAntigravity,
		})
		if err != nil {
			t.Fatalf("Execute() #%d error = %v", i+1, err)
		}
	}

	if got := counter.distinct(); got != 1 {
		t.Fatalf("expected %d requests to reuse one upstream connection, got %d connections", requests, got)
	}
}

func TestAntigravityCountTokensReusesUpstreamConnection(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	counter := newRemoteAddrCounter()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.record(r.RemoteAddr)
		if r.Close {
			t.Errorf("upstream saw Connection: close, want a keep-alive request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"totalTokens":7}`))
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{})
	auth := antigravityKeepaliveAuth(t, server.URL)

	const requests = 3
	for i := 0; i < requests; i++ {
		_, err := exec.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-sonnet-4-6",
			Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatAntigravity,
		})
		if err != nil {
			t.Fatalf("CountTokens() #%d error = %v", i+1, err)
		}
	}

	if got := counter.distinct(); got != 1 {
		t.Fatalf("expected %d countTokens requests to reuse one upstream connection, got %d connections", requests, got)
	}
}

// TestAntigravityHTTPRequestClearsInboundConnectionClose covers the passthrough
// path. Request.Close is a field, not a header, so the header whitelist in
// HttpRequest cannot strip it: a downstream client sending "Connection: close"
// makes Go's server set Request.Close, WithContext copies it verbatim, and the
// close would then propagate upstream and drain the shared pool for everyone.
func TestAntigravityHTTPRequestClearsInboundConnectionClose(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	counter := newRemoteAddrCounter()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.record(r.RemoteAddr)
		if r.Close {
			t.Errorf("inbound Connection: close leaked to the upstream request")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{})
	auth := antigravityKeepaliveAuth(t, server.URL)

	const requests = 3
	for i := 0; i < requests; i++ {
		req, errReq := http.NewRequest(http.MethodPost, server.URL+"/v1internal:countTokens", http.NoBody)
		if errReq != nil {
			t.Fatalf("build request #%d: %v", i+1, errReq)
		}
		req.Header.Set("Content-Type", "application/json")
		// Mirror what Go's server does for an inbound "Connection: close".
		req.Header.Set("Connection", "close")
		req.Close = true

		resp, errDo := exec.HttpRequest(context.Background(), auth, req)
		if errDo != nil {
			t.Fatalf("HttpRequest() #%d error = %v", i+1, errDo)
		}
		// The body must be drained to EOF before Close, otherwise Go cannot return
		// the connection to the idle pool and every request opens a new one. The
		// real callers of HttpRequest read the response in full; only this test
		// would otherwise skip it and measure a reuse failure that does not exist.
		if _, errDrain := io.Copy(io.Discard, resp.Body); errDrain != nil {
			t.Fatalf("drain response #%d: %v", i+1, errDrain)
		}
		_ = resp.Body.Close()
	}

	if got := counter.distinct(); got != 1 {
		t.Fatalf("expected %d passthrough requests to reuse one upstream connection, got %d connections", requests, got)
	}
}
