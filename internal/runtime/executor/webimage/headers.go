package webimage

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
)

func newAPIRequest(ctx context.Context, session *Session, credentials Credentials, method, baseURL, path string, body []byte) (*http.Request, error) {
	endpoint, errParse := url.Parse(strings.TrimSuffix(baseURL, "/") + path)
	if errParse != nil {
		return nil, errParse
	}
	request, errRequest := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(body))
	if errRequest != nil {
		return nil, errRequest
	}
	applyBrowserHeaders(request, session)
	if token := strings.TrimSpace(credentials.AccessToken); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if accountID := strings.TrimSpace(credentials.AccountID); accountID != "" {
		request.Header.Set("ChatGPT-Account-ID", accountID)
	}
	request.Header.Set("X-OpenAI-Target-Path", path)
	request.Header.Set("X-OpenAI-Target-Route", path)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}

func sentinelHeaders(state *generationState) http.Header {
	headers := make(http.Header)
	if state == nil {
		return headers
	}
	if state.chatRequirementsToken != "" {
		headers.Set("OpenAI-Sentinel-Chat-Requirements-Token", state.chatRequirementsToken)
		if state.proofToken != "" {
			headers.Set("OpenAI-Sentinel-Proof-Token", state.proofToken)
		}
		return headers
	}
	headers.Set("OpenAI-Sentinel-Chat-Requirements-Prepare-Token", state.prepareToken)
	headers.Set("OpenAI-Sentinel-Proof-Token", state.proofToken)
	headers.Set("OpenAI-Sentinel-Turnstile-Token", state.turnstileToken)
	return headers
}

func applyBrowserHeaders(request *http.Request, session *Session) {
	if request == nil || session == nil {
		return
	}
	request.Header.Set("User-Agent", session.UserAgent)
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	request.Header.Set("Origin", "https://chatgpt.com")
	request.Header.Set("Referer", "https://chatgpt.com/")
	request.Header.Set("OAI-Device-ID", session.Identity.DeviceID)
	request.Header.Set("OAI-Session-ID", session.Identity.SessionID)
	request.Header.Set("OAI-Language", "zh-CN")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Pragma", "no-cache")
	if session.ClientVersion != "" {
		request.Header.Set("OAI-Client-Version", session.ClientVersion)
		request.Header.Set("OAI-Client-Build-Number", session.ClientBuild)
	}
}
