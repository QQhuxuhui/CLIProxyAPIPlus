package webimage

import (
	"context"
	"net/http"
	"testing"
)

func TestNewAPIRequestBuildsConsistentBrowserHeaders(t *testing.T) {
	session := &Session{
		UserAgent: "Mozilla/5.0 Chrome/152.0.0.0 Safari/537.36",
		Identity:  Identity{DeviceID: "device", SessionID: "session"},
	}
	request, errRequest := newAPIRequest(context.Background(), session, Credentials{AccessToken: "token"}, http.MethodPost, "https://chatgpt.com", "/backend-api/test", []byte(`{}`))
	if errRequest != nil {
		t.Fatalf("newAPIRequest() error = %v", errRequest)
	}

	if got := request.Header.Get("Sec-CH-UA"); got != `"Not_A Brand";v="99", "Chromium";v="152", "Google Chrome";v="152"` {
		t.Fatalf("Sec-CH-UA = %q", got)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer token" {
		t.Fatalf("Authorization = %q", got)
	}
	if request.Header.Get("OAI-Device-ID") != "device" || request.Header.Get("OAI-Session-ID") != "session" {
		t.Fatalf("identity headers = %#v", request.Header)
	}
}

func TestChromeMajorVersionFallsBackForNonChromeUserAgent(t *testing.T) {
	if got := chromeMajorVersion("custom-agent"); got != "151" {
		t.Fatalf("chromeMajorVersion() = %q, want 151", got)
	}
}
