package helps

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

var chatGPTWebOpaqueAssetPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

func (c *ChatGPTWebClient) downloadAsset(ctx context.Context, conversationID, ref string) ([]byte, string, error) {
	if strings.HasPrefix(ref, "data:") {
		return chatGPTWebDecodeDataURL(ref)
	}
	assetURL, errAsset := c.assetURL(ref, conversationID)
	if errAsset != nil {
		return nil, "", errAsset
	}
	currentURL, errResolve := c.resolveURL(assetURL)
	if errResolve != nil {
		return nil, "", errResolve
	}

	for attempts := 0; attempts < 6; attempts++ {
		if errValidate := c.validateAssetURL(currentURL); errValidate != nil {
			return nil, "", errValidate
		}
		withAuth := strings.EqualFold(currentURL.Hostname(), c.baseURL.Hostname())
		resp, errDo := c.do(ctx, http.MethodGet, currentURL.String(), nil, nil, withAuth)
		if errDo != nil {
			return nil, "", errDo
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location := strings.TrimSpace(resp.Header.Get("Location"))
			_ = resp.Body.Close()
			if location == "" {
				return nil, "", newChatGPTWebError(http.StatusBadGateway, "asset_redirect_invalid", "image redirect has no location", nil)
			}
			next, errParse := url.Parse(location)
			if errParse != nil {
				return nil, "", newChatGPTWebError(http.StatusBadGateway, "asset_redirect_invalid", "image redirect URL is invalid", errParse)
			}
			currentURL = currentURL.ResolveReference(next)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_ = resp.Body.Close()
			return nil, "", c.httpError("image download", resp.StatusCode)
		}
		data, errRead := io.ReadAll(io.LimitReader(resp.Body, c.maxImageBytes+1))
		_ = resp.Body.Close()
		if errRead != nil {
			return nil, "", newChatGPTWebError(http.StatusBadGateway, "image_download_failed", "image download failed", errRead)
		}
		if int64(len(data)) > c.maxImageBytes {
			return nil, "", newChatGPTWebError(http.StatusRequestEntityTooLarge, "image_too_large", "image exceeds the configured size limit", nil)
		}
		if len(data) == 0 {
			return nil, "", newChatGPTWebError(http.StatusBadGateway, "empty_image", "image asset is empty", nil)
		}
		contentType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
		looksLikeJSON := bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) || bytes.HasPrefix(bytes.TrimSpace(data), []byte("["))
		if contentType == "application/json" || looksLikeJSON {
			var payload any
			if errJSON := json.Unmarshal(data, &payload); errJSON != nil {
				return nil, "", newChatGPTWebError(http.StatusBadGateway, "asset_download_invalid", "image download endpoint returned invalid JSON", errJSON)
			}
			downloadURL := chatGPTWebString(chatGPTWebFindFirst(payload, map[string]bool{"download_url": true, "downloadUrl": true, "url": true}))
			if downloadURL == "" {
				return nil, "", newChatGPTWebError(http.StatusBadGateway, "asset_download_missing", "image download endpoint returned no URL", nil)
			}
			next, errParse := url.Parse(downloadURL)
			if errParse != nil {
				return nil, "", newChatGPTWebError(http.StatusBadGateway, "asset_download_invalid", "signed image URL is invalid", errParse)
			}
			currentURL = currentURL.ResolveReference(next)
			continue
		}
		if !strings.HasPrefix(contentType, "image/") {
			contentType = mime.TypeByExtension(strings.ToLower(filepath.Ext(currentURL.Path)))
		}
		if !strings.HasPrefix(contentType, "image/") {
			contentType = http.DetectContentType(data)
		}
		if !strings.HasPrefix(contentType, "image/") {
			contentType = "application/octet-stream"
		}
		return data, contentType, nil
	}
	return nil, "", newChatGPTWebError(http.StatusBadGateway, "asset_redirect_limit", "image download redirect limit was reached", nil)
}

func (c *ChatGPTWebClient) assetURL(ref, conversationID string) (string, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case strings.HasPrefix(ref, "file-service://"):
		fileID := strings.TrimPrefix(ref, "file-service://")
		return "/backend-api/files/" + url.PathEscape(fileID) + "/download", nil
	case strings.HasPrefix(ref, "sediment://"):
		if strings.TrimSpace(conversationID) == "" {
			return "", newChatGPTWebError(http.StatusBadGateway, "asset_pointer_invalid", "sediment asset has no conversation id", nil)
		}
		fileID := strings.TrimPrefix(ref, "sediment://")
		return "/backend-api/conversation/" + url.PathEscape(conversationID) + "/attachment/" + url.PathEscape(fileID) + "/download", nil
	case strings.HasPrefix(ref, "http://"), strings.HasPrefix(ref, "https://"), strings.HasPrefix(ref, "/"):
		return ref, nil
	case chatGPTWebOpaqueAssetPattern.MatchString(ref):
		return "/backend-api/files/" + url.PathEscape(ref) + "/download", nil
	default:
		return "", newChatGPTWebError(http.StatusBadGateway, "asset_pointer_invalid", "image asset pointer is not recognized", nil)
	}
}

func (c *ChatGPTWebClient) validateAssetURL(target *url.URL) error {
	if target == nil || target.Hostname() == "" {
		return newChatGPTWebError(http.StatusBadGateway, "asset_url_rejected", "image asset URL is invalid", nil)
	}
	host := strings.ToLower(target.Hostname())
	baseHost := strings.ToLower(c.baseURL.Hostname())
	if host == baseHost && strings.EqualFold(target.Scheme, c.baseURL.Scheme) {
		return nil
	}
	if !strings.EqualFold(target.Scheme, "https") {
		return newChatGPTWebError(http.StatusBadGateway, "asset_url_rejected", "external image asset URL must use HTTPS", nil)
	}
	allowed := host == "openai.com" || strings.HasSuffix(host, ".openai.com") || host == "oaiusercontent.com" || strings.HasSuffix(host, ".oaiusercontent.com")
	if !allowed {
		return newChatGPTWebError(http.StatusBadGateway, "asset_url_rejected", "image asset host is not allowed", nil)
	}
	return nil
}

func chatGPTWebDecodeDataURL(value string) ([]byte, string, error) {
	header, encoded, ok := strings.Cut(value, ",")
	if !ok || !strings.Contains(header, ";base64") || !strings.HasPrefix(header, "data:image/") {
		return nil, "", newChatGPTWebError(http.StatusBadGateway, "invalid_data_url", "inline image data URL is invalid", nil)
	}
	mimeType := strings.TrimPrefix(strings.Split(header, ";")[0], "data:")
	data, errDecode := base64.StdEncoding.DecodeString(encoded)
	if errDecode != nil {
		return nil, "", newChatGPTWebError(http.StatusBadGateway, "invalid_data_url", "inline image base64 is invalid", errDecode)
	}
	if len(data) == 0 {
		return nil, "", newChatGPTWebError(http.StatusBadGateway, "empty_image", "inline image is empty", nil)
	}
	return data, mimeType, nil
}

func chatGPTWebAssetDebugString(ref string) string {
	if strings.HasPrefix(ref, "data:") {
		return "inline-image"
	}
	parsed, errParse := url.Parse(ref)
	if errParse == nil && parsed.Hostname() != "" {
		return parsed.Scheme + "://" + parsed.Hostname()
	}
	return fmt.Sprintf("asset(%d)", len(ref))
}
