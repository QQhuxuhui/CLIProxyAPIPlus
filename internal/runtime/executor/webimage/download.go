package webimage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func (e *Executor) resolveAndDownload(ctx context.Context, session *Session, credentials Credentials, state *generationState, deadline time.Time) (string, string, error) {
	if errBudget := e.checkBudget(ctx, deadline, "download descriptor"); errBudget != nil {
		return "", "", errBudget
	}
	refs := deduplicateAssetRefs(state.assetRefs)
	if len(refs) == 0 {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "download descriptor", Msg: "web image asset pointer is missing"}
	}
	var lastError error
	for index := len(refs) - 1; index >= 0; index-- {
		imageData, outputFormat, errDownload := e.resolveAsset(ctx, session, credentials, state, refs[index])
		if errDownload == nil {
			return imageData, outputFormat, nil
		}
		if errContext := preserveContextError(errDownload); errContext != nil {
			return "", "", errContext
		}
		lastError = errDownload
	}
	return "", "", lastError
}

func (e *Executor) resolveAsset(ctx context.Context, session *Session, credentials Credentials, state *generationState, assetRef string) (string, string, error) {
	assetRef = strings.TrimSpace(assetRef)
	if strings.HasPrefix(strings.ToLower(assetRef), "data:image/") {
		return decodeDataImage(assetRef)
	}
	if strings.HasPrefix(assetRef, "http://") || strings.HasPrefix(assetRef, "https://") || strings.HasPrefix(assetRef, "/") {
		return e.downloadImageWithAuth(ctx, session, credentials, state, assetRef, nil)
	}
	fileID := assetID(assetRef)
	if fileID == "" {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "download descriptor", Msg: "web image asset pointer is missing"}
	}
	descriptorPath := "/backend-api/files/" + url.PathEscape(fileID) + "/download"
	if strings.HasPrefix(assetRef, "sediment://") {
		descriptorPath = "/backend-api/conversation/" + url.PathEscape(state.conversationID) + "/attachment/" + url.PathEscape(fileID) + "/download"
	}
	request, errRequest := newAPIRequest(ctx, session, credentials, http.MethodGet, e.baseURL, descriptorPath, nil)
	if errRequest != nil {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "download descriptor", Msg: "web image download descriptor request failed"}
	}
	for name, values := range sentinelHeaders(state) {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, errDo := session.Client.Do(request)
	if errDo != nil {
		if errContext := preserveContextError(errDo); errContext != nil {
			return "", "", errContext
		}
		return "", "", &StatusError{Status: http.StatusServiceUnavailable, Kind: ErrorKindChallenge, Stage: "download descriptor", Msg: "web image download descriptor transport failed"}
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		location := strings.TrimSpace(response.Header.Get("Location"))
		if location == "" {
			return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "download descriptor", Msg: "web image download redirect missing"}
		}
		nextURL, errLocation := request.URL.Parse(location)
		if errLocation != nil {
			return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "download descriptor", Msg: "web image download redirect invalid"}
		}
		return e.downloadImageWithAuth(ctx, session, credentials, state, nextURL.String(), request.URL)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", "", classifyHTTPError("download descriptor", response)
	}
	maxBytes := config.DefaultWebImageMaxBytes
	if e.cfg != nil && e.cfg.WebImageMaxBytes > 0 {
		maxBytes = e.cfg.WebImageMaxBytes
	}
	data, errRead := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if errRead != nil {
		if errContext := preserveContextError(errRead); errContext != nil {
			return "", "", errContext
		}
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "download descriptor", Msg: "web image download descriptor read failed"}
	}
	if int64(len(data)) > maxBytes {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindOversize, Stage: "download descriptor", Msg: "web image exceeded configured byte limit"}
	}
	if len(data) == 0 {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "download descriptor", Msg: "web image asset is empty"}
	}
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	looksLikeJSON := bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) || bytes.HasPrefix(bytes.TrimSpace(data), []byte("["))
	if strings.HasPrefix(strings.ToLower(contentType), "image/") && !looksLikeJSON {
		outputFormat := strings.TrimPrefix(strings.ToLower(contentType), "image/")
		return base64.StdEncoding.EncodeToString(data), outputFormat, nil
	}
	var decoded any
	if errDecode := json.Unmarshal(data, &decoded); errDecode != nil {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "download descriptor", Msg: "web image download descriptor schema changed"}
	}
	downloadURL := findString(decoded, "download_url", "downloadUrl", "url")
	if downloadURL == "" {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "download descriptor", Msg: "web image download URL missing"}
	}
	return e.downloadImageWithAuth(ctx, session, credentials, state, downloadURL, request.URL)
}

func (e *Executor) downloadImage(ctx context.Context, session *Session, downloadURL string) (string, string, error) {
	return e.downloadImageWithAuth(ctx, session, Credentials{}, nil, downloadURL, nil)
}

func (e *Executor) downloadImageWithAuth(ctx context.Context, session *Session, credentials Credentials, state *generationState, downloadURL string, resolveBase *url.URL) (string, string, error) {
	parsedURL, errParse := url.Parse(strings.TrimSpace(downloadURL))
	if errParse != nil {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image download URL is invalid"}
	}
	if !parsedURL.IsAbs() {
		if resolveBase == nil {
			resolveBase, _ = url.Parse(strings.TrimSuffix(e.baseURL, "/") + "/")
		}
		if resolveBase == nil {
			return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image download URL is invalid"}
		}
		parsedURL = resolveBase.ResolveReference(parsedURL)
	}
	currentURL := parsedURL
	downloadClient := *session.Client
	downloadClient.Jar = nil
	maxBytes := config.DefaultWebImageMaxBytes
	if e.cfg != nil && e.cfg.WebImageMaxBytes > 0 {
		maxBytes = e.cfg.WebImageMaxBytes
	}
	for attempts := 0; attempts < 6; attempts++ {
		if errValidate := e.validateImageURL(currentURL); errValidate != nil {
			return "", "", errValidate
		}
		request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, currentURL.String(), nil)
		if errRequest != nil {
			return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image download request failed"}
		}
		applyBrowserHeaders(request, session)
		if e.sameOrigin(currentURL) {
			if token := strings.TrimSpace(credentials.AccessToken); token != "" {
				request.Header.Set("Authorization", "Bearer "+token)
			}
			if accountID := strings.TrimSpace(credentials.AccountID); accountID != "" {
				request.Header.Set("ChatGPT-Account-ID", accountID)
			}
			request.Header.Set("X-OpenAI-Target-Path", currentURL.Path)
			request.Header.Set("X-OpenAI-Target-Route", currentURL.Path)
			for name, values := range sentinelHeaders(state) {
				for _, value := range values {
					request.Header.Add(name, value)
				}
			}
		} else {
			request.Header.Del("Authorization")
			request.Header.Del("ChatGPT-Account-ID")
			request.Header.Del("OpenAI-Sentinel-Chat-Requirements-Token")
			request.Header.Del("OpenAI-Sentinel-Proof-Token")
			request.Header.Del("OAI-Device-ID")
			request.Header.Del("OAI-Session-ID")
			request.Header.Del("OAI-Client-Version")
			request.Header.Del("OAI-Client-Build-Number")
		}

		response, errDo := downloadClient.Do(request)
		if errDo != nil {
			if errContext := preserveContextError(errDo); errContext != nil {
				return "", "", errContext
			}
			return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindUpstream, Stage: "image download", Msg: "web image download transport failed"}
		}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			location := strings.TrimSpace(response.Header.Get("Location"))
			_ = response.Body.Close()
			if location == "" {
				return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image download redirect missing"}
			}
			nextURL, errLocation := currentURL.Parse(location)
			if errLocation != nil {
				return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image download redirect invalid"}
			}
			currentURL = nextURL
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			errStatus := classifyHTTPError("image download", response)
			_ = response.Body.Close()
			return "", "", errStatus
		}
		imageData, errRead := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
		_ = response.Body.Close()
		if errRead != nil {
			if errContext := preserveContextError(errRead); errContext != nil {
				return "", "", errContext
			}
			return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindUpstream, Stage: "image download", Msg: "web image download read failed"}
		}
		if int64(len(imageData)) > maxBytes {
			return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindOversize, Stage: "image download", Msg: "web image exceeded configured byte limit"}
		}
		if len(imageData) == 0 {
			return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image asset is empty"}
		}
		contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
		looksLikeJSON := bytes.HasPrefix(bytes.TrimSpace(imageData), []byte("{")) || bytes.HasPrefix(bytes.TrimSpace(imageData), []byte("["))
		if !strings.HasPrefix(strings.ToLower(contentType), "image/") || looksLikeJSON {
			var decoded any
			if json.Unmarshal(imageData, &decoded) == nil {
				if next := findString(decoded, "download_url", "downloadUrl", "url"); next != "" {
					nextURL, errNext := currentURL.Parse(next)
					if errNext != nil {
						return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image download URL is invalid"}
					}
					currentURL = nextURL
					continue
				}
			}
			contentType = http.DetectContentType(imageData)
		}
		if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
			return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image download returned non-image content"}
		}
		outputFormat := strings.TrimPrefix(strings.ToLower(contentType), "image/")
		if outputFormat == "" {
			outputFormat = strings.TrimPrefix(strings.ToLower(path.Ext(currentURL.Path)), ".")
		}
		return base64.StdEncoding.EncodeToString(imageData), outputFormat, nil
	}
	return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindUpstream, Stage: "image download", Msg: "web image download redirect limit reached"}
}

func (e *Executor) sameOrigin(target *url.URL) bool {
	baseURL, _ := url.Parse(e.baseURL)
	return baseURL != nil && target != nil && strings.EqualFold(target.Host, baseURL.Host) && strings.EqualFold(target.Scheme, baseURL.Scheme)
}

func decodeDataImage(value string) (string, string, error) {
	header, encoded, ok := strings.Cut(value, ",")
	if !ok || !strings.HasPrefix(strings.ToLower(header), "data:image/") || !strings.Contains(strings.ToLower(header), ";base64") {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image inline data is invalid"}
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(encoded)
	if errDecode != nil || len(decoded) == 0 {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image inline data is invalid"}
	}
	mediaType := strings.TrimPrefix(strings.ToLower(strings.Split(header, ";")[0]), "data:image/")
	return base64.StdEncoding.EncodeToString(decoded), mediaType, nil
}

func (e *Executor) validateImageURL(target *url.URL) error {
	if target == nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image download URL is invalid"}
	}
	if e.sameOrigin(target) {
		return nil
	}
	if !strings.EqualFold(target.Scheme, "https") {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image download URL must use HTTPS"}
	}
	host := strings.ToLower(target.Hostname())
	if host == "openai.com" || strings.HasSuffix(host, ".openai.com") || host == "oaiusercontent.com" || strings.HasSuffix(host, ".oaiusercontent.com") {
		return nil
	}
	return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image download host is not allowed"}
}
