package webimage

import (
	"context"
	"net/http"
	"testing"
)

func TestNewAPIRequestBuildsConsistentBrowserHeaders(t *testing.T) {
	session := &Session{
		UserAgent:     "Mozilla/5.0 Chrome/124.0.0.0 Safari/537.36",
		ClientVersion: "client-version",
		ClientBuild:   "client-build",
		Identity:      Identity{DeviceID: "device", SessionID: "session"},
	}
	request, errRequest := newAPIRequest(context.Background(), session, Credentials{AccessToken: "token", AccountID: "account"}, http.MethodPost, "https://chatgpt.com", "/backend-api/test", []byte(`{}`))
	if errRequest != nil {
		t.Fatalf("newAPIRequest() error = %v", errRequest)
	}

	if got := request.Header.Get("Sec-CH-UA"); got != "" {
		t.Fatalf("Sec-CH-UA = %q, want omitted", got)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := request.Header.Get("ChatGPT-Account-ID"); got != "account" {
		t.Fatalf("ChatGPT-Account-ID = %q", got)
	}
	if request.Header.Get("OAI-Device-ID") != "device" || request.Header.Get("OAI-Session-ID") != "session" {
		t.Fatalf("identity headers = %#v", request.Header)
	}
	if request.Header.Get("Accept-Language") != "zh-CN,zh;q=0.9,en;q=0.8" || request.Header.Get("Origin") != "https://chatgpt.com" || request.Header.Get("OAI-Language") != "zh-CN" {
		t.Fatalf("browser headers = %#v", request.Header)
	}
	if request.Header.Get("OAI-Client-Version") != "client-version" || request.Header.Get("OAI-Client-Build-Number") != "client-build" {
		t.Fatalf("client headers = %#v", request.Header)
	}
}

func TestSentinelHeadersPreserveFinalizedProof(t *testing.T) {
	headers := sentinelHeaders(&generationState{
		chatRequirementsToken: "requirements",
		proofToken:            "proof",
	})
	if headers.Get("OpenAI-Sentinel-Chat-Requirements-Token") != "requirements" || headers.Get("OpenAI-Sentinel-Proof-Token") != "proof" {
		t.Fatalf("sentinel headers = %#v", headers)
	}
}
