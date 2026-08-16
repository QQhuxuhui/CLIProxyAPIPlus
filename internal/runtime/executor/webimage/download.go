package webimage

import (
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
	descriptorPath := "/backend-api/files/download/" + url.PathEscape(state.fileID)
	request, errRequest := newAPIRequest(ctx, session, credentials, http.MethodGet, e.baseURL, descriptorPath, nil)
	if errRequest != nil {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "download descriptor", Msg: "web image download descriptor request failed"}
	}
	query := request.URL.Query()
	query.Set("conversation_id", state.conversationID)
	query.Set("download_intent", "1")
	query.Set("include_library_file_state", "true")
	query.Set("inline", "false")
	request.URL.RawQuery = query.Encode()
	response, errDo := session.Client.Do(request)
	if errDo != nil {
		return "", "", &StatusError{Status: http.StatusServiceUnavailable, Kind: ErrorKindChallenge, Stage: "download descriptor", Msg: "web image download descriptor transport failed"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", "", classifyHTTPError("download descriptor", response)
	}
	data, errRead := io.ReadAll(io.LimitReader(response.Body, maxProtocolResponseBytes+1))
	if errRead != nil || len(data) > maxProtocolResponseBytes {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "download descriptor", Msg: "web image download descriptor read failed"}
	}
	var decoded any
	if errDecode := json.Unmarshal(data, &decoded); errDecode != nil {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "download descriptor", Msg: "web image download descriptor schema changed"}
	}
	downloadURL := findString(decoded, "download_url", "downloadUrl", "url")
	if downloadURL == "" {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "download descriptor", Msg: "web image download URL missing"}
	}
	return e.downloadImage(ctx, session, downloadURL)
}

func (e *Executor) downloadImage(ctx context.Context, session *Session, downloadURL string) (string, string, error) {
	parsedURL, errParse := url.Parse(strings.TrimSpace(downloadURL))
	if errParse != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image download URL is invalid"}
	}
	if parsedURL.Scheme != "https" && !strings.HasPrefix(strings.ToLower(e.baseURL), "http://") {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image download URL must use HTTPS"}
	}
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if errRequest != nil {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image download request failed"}
	}
	applyBrowserHeaders(request, session)
	request.Header.Del("OAI-Device-ID")
	request.Header.Del("OAI-Session-ID")
	request.Header.Del("OAI-Client-Version")
	request.Header.Del("OAI-Client-Build-Number")

	downloadClient := *session.Client
	downloadClient.Jar = nil
	response, errDo := downloadClient.Do(request)
	if errDo != nil {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindUpstream, Stage: "image download", Msg: "web image download transport failed"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", "", classifyHTTPError("image download", response)
	}
	maxBytes := config.DefaultWebImageMaxBytes
	if e.cfg != nil && e.cfg.WebImageMaxBytes > 0 {
		maxBytes = e.cfg.WebImageMaxBytes
	}
	imageData, errRead := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if errRead != nil {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindUpstream, Stage: "image download", Msg: "web image download read failed"}
	}
	if int64(len(imageData)) > maxBytes {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindOversize, Stage: "image download", Msg: "web image exceeded configured byte limit"}
	}
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		return "", "", &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image download", Msg: "web image download returned non-image content"}
	}
	outputFormat := strings.TrimPrefix(strings.ToLower(contentType), "image/")
	if outputFormat == "" {
		outputFormat = strings.TrimPrefix(strings.ToLower(path.Ext(parsedURL.Path)), ".")
	}
	return base64.StdEncoding.EncodeToString(imageData), outputFormat, nil
}
