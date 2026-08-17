package webimage

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"unicode"
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
	request.Header.Set("OAI-Language", "en-US")
	request.Header.Set("X-OAI-Is-Client-Observation", "false")
	request.Header.Set("X-OpenAI-Target-Path", path)
	request.Header.Set("X-OpenAI-Target-Route", path)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}

func applyBrowserHeaders(request *http.Request, session *Session) {
	if request == nil || session == nil {
		return
	}
	request.Header.Set("User-Agent", session.UserAgent)
	request.Header.Set("Referer", "https://chatgpt.com/")
	majorVersion := chromeMajorVersion(session.UserAgent)
	request.Header.Set("Sec-CH-UA", `"Not_A Brand";v="99", "Chromium";v="`+majorVersion+`", "Google Chrome";v="`+majorVersion+`"`)
	request.Header.Set("Sec-CH-UA-Mobile", "?0")
	request.Header.Set("Sec-CH-UA-Platform", `"Windows"`)
	request.Header.Set("OAI-Device-ID", session.Identity.DeviceID)
	request.Header.Set("OAI-Session-ID", session.Identity.SessionID)
	if session.ClientVersion != "" {
		request.Header.Set("OAI-Client-Version", session.ClientVersion)
		request.Header.Set("OAI-Client-Build-Number", session.ClientVersion)
	}
}

func chromeMajorVersion(userAgent string) string {
	const fallback = "151"
	marker := "Chrome/"
	index := strings.Index(userAgent, marker)
	if index < 0 {
		return fallback
	}
	remaining := userAgent[index+len(marker):]
	end := 0
	for end < len(remaining) && unicode.IsDigit(rune(remaining[end])) {
		end++
	}
	if end == 0 {
		return fallback
	}
	return remaining[:end]
}
