package helps

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChromeFingerprintHTTPClientSupportsPlainHTTPFixtures(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	client, errClient := NewChromeFingerprintHTTPClient("")
	if errClient != nil {
		t.Fatalf("NewChromeFingerprintHTTPClient() error = %v", errClient)
	}
	response, errGet := client.Get(server.URL)
	if errGet != nil {
		t.Fatalf("client.Get() error = %v", errGet)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "ok" {
		t.Fatalf("body = %q, want ok", body)
	}
}

func TestChromeFingerprintHTTPClientRejectsUntrustedTLS(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "unexpected")
	}))
	defer server.Close()

	client, errClient := NewChromeFingerprintHTTPClient("")
	if errClient != nil {
		t.Fatalf("NewChromeFingerprintHTTPClient() error = %v", errClient)
	}
	_, errGet := client.Get(server.URL)
	if errGet == nil || !strings.Contains(strings.ToLower(errGet.Error()), "certificate") {
		t.Fatalf("client.Get() error = %v, want certificate rejection", errGet)
	}
}

func TestChromeFingerprintHTTPClientFallsBackToHTTP11ForNonChatGPTHost(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Errorf("request protocol = %s, want HTTP/1.1", r.Proto)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	server.EnableHTTP2 = false
	server.StartTLS()
	defer server.Close()

	client, errClient := NewChromeFingerprintHTTPClient("")
	if errClient != nil {
		t.Fatalf("NewChromeFingerprintHTTPClient() error = %v", errClient)
	}
	transport, ok := client.Transport.(*chromeFingerprintRoundTripper)
	if !ok {
		t.Fatalf("client transport type = %T", client.Transport)
	}
	trustedTransport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("server client transport type = %T", server.Client().Transport)
	}
	transport.http = trustedTransport.Clone()

	response, errGet := client.Get(server.URL)
	if errGet != nil {
		t.Fatalf("client.Get() error = %v", errGet)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "ok" {
		t.Fatalf("body = %q, want ok", body)
	}
}

func TestChromeFingerprintHTTPClientRejectsInvalidProxy(t *testing.T) {
	t.Parallel()

	if _, errClient := NewChromeFingerprintHTTPClient("invalid-proxy"); errClient == nil {
		t.Fatal("NewChromeFingerprintHTTPClient() error = nil, want invalid proxy error")
	}
}
