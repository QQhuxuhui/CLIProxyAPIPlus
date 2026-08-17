package webimage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func (e *Executor) validateInputImages(images []InputImage) error {
	if len(images) > config.WebImageMaxInputImages {
		return &StatusError{Status: http.StatusBadRequest, Kind: ErrorKindOversize, Stage: "image upload", Msg: "web image edits contain too many reference images"}
	}
	maxBytes := config.DefaultWebImageMaxBytes
	if e != nil && e.cfg != nil && e.cfg.WebImageMaxBytes > 0 {
		maxBytes = e.cfg.WebImageMaxBytes
	}
	var totalBytes int64
	for _, image := range images {
		if len(image.Data) == 0 || image.Width <= 0 || image.Height <= 0 {
			return &StatusError{Status: http.StatusBadRequest, Kind: ErrorKindProtocol, Stage: "image upload", Msg: "web image reference image is invalid"}
		}
		imageBytes := int64(len(image.Data))
		if imageBytes > maxBytes || totalBytes > maxBytes-imageBytes {
			return &StatusError{Status: http.StatusBadRequest, Kind: ErrorKindOversize, Stage: "image upload", Msg: "web image reference images exceed the configured byte limit"}
		}
		totalBytes += imageBytes
	}
	return nil
}

func (e *Executor) uploadInputImages(ctx context.Context, session *Session, credentials Credentials, state *generationState, images []InputImage, deadline time.Time) error {
	for _, image := range images {
		if errBudget := e.checkBudget(ctx, deadline, "image upload"); errBudget != nil {
			return errBudget
		}
		if len(image.Data) == 0 || image.Width <= 0 || image.Height <= 0 {
			return &StatusError{Status: http.StatusBadRequest, Kind: ErrorKindProtocol, Stage: "image upload", Msg: "web image reference image is invalid"}
		}
		filename := strings.TrimSpace(image.Filename)
		if filename == "" {
			filename = "reference-image"
		}
		mimeType := strings.TrimSpace(image.MIMEType)
		if mimeType == "" {
			mimeType = http.DetectContentType(image.Data)
		}
		createBody, errMarshal := json.Marshal(map[string]any{
			"file_name":                       filename,
			"file_size":                       len(image.Data),
			"use_case":                        "multimodal",
			"timezone_offset_min":             -480,
			"reset_rate_limits":               false,
			"supports_direct_azure_multipart": true,
			"mime_type":                       mimeType,
			"entry_surface":                   "composer",
		})
		if errMarshal != nil {
			return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image upload", Msg: "web image upload body failed"}
		}
		created, errCreate := e.doJSON(ctx, session, credentials, http.MethodPost, "/backend-api/files", createBody, nil, "image upload create")
		if errCreate != nil {
			return errCreate
		}
		fileID := findString(created.Body, "file_id", "fileId")
		uploadURL := findString(created.Body, "upload_url", "uploadUrl")
		if fileID == "" || uploadURL == "" {
			return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image upload create", Msg: "web image upload descriptor is missing"}
		}
		if errPut := e.putInputImage(ctx, session, uploadURL, mimeType, image.Data); errPut != nil {
			return errPut
		}
		if errProcess := e.processInputImage(ctx, session, credentials, fileID, filename); errProcess != nil {
			return errProcess
		}
		state.uploadedImages = append(state.uploadedImages, uploadedImage{
			FileID:       fileID,
			Filename:     filename,
			MIMEType:     mimeType,
			AssetPointer: uploadAssetPointer(fileID),
			SizeBytes:    len(image.Data),
			Width:        image.Width,
			Height:       image.Height,
		})
	}
	return nil
}

func (e *Executor) putInputImage(ctx context.Context, session *Session, uploadURL, mimeType string, data []byte) error {
	parsedURL, errParse := url.Parse(strings.TrimSpace(uploadURL))
	if errParse != nil || !parsedURL.IsAbs() {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image upload", Msg: "web image upload URL is invalid"}
	}
	if errValidate := e.validateUploadURL(parsedURL); errValidate != nil {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image upload", Msg: "web image upload URL is not allowed"}
	}
	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPut, parsedURL.String(), bytes.NewReader(data))
	if errRequest != nil {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image upload", Msg: "web image upload request failed"}
	}
	request.Header.Set("User-Agent", session.UserAgent)
	request.Header.Set("Origin", "https://chatgpt.com")
	request.Header.Set("Referer", "https://chatgpt.com/")
	request.Header.Set("Content-Type", mimeType)
	request.Header.Set("X-Ms-Blob-Type", "BlockBlob")
	uploadClient := *session.Client
	uploadClient.Jar = nil
	response, errDo := uploadClient.Do(request)
	if errDo != nil {
		if errContext := preserveContextError(errDo); errContext != nil {
			return errContext
		}
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindUpstream, Stage: "image upload", Msg: "web image upload transport failed"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindUpstream, Stage: "image upload", Msg: "web image signed upload was rejected", Scoped: true}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return nil
}

func (e *Executor) processInputImage(ctx context.Context, session *Session, credentials Credentials, fileID, filename string) error {
	body, errMarshal := json.Marshal(map[string]any{
		"file_id":             fileID,
		"use_case":            "multimodal",
		"index_for_retrieval": false,
		"file_name":           filename,
		"entry_surface":       "composer",
	})
	if errMarshal != nil {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image processing", Msg: "web image processing body failed"}
	}
	request, errRequest := newAPIRequest(ctx, session, credentials, http.MethodPost, e.baseURL, "/backend-api/files/process_upload_stream", body)
	if errRequest != nil {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image processing", Msg: "web image processing request failed"}
	}
	request.Header.Set("Accept", "text/event-stream")
	response, errDo := session.Client.Do(request)
	if errDo != nil {
		if errContext := preserveContextError(errDo); errContext != nil {
			return errContext
		}
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindUpstream, Stage: "image processing", Msg: "web image processing transport failed"}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return classifyHTTPError("image processing", response)
	}
	return parseUploadProcessingStream(response.Body)
}

func parseUploadProcessingStream(reader io.Reader) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxProtocolResponseBytes)
	ready := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		} else if strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:") {
			continue
		}
		if line == "" || line == "[DONE]" {
			continue
		}
		var event map[string]any
		if errDecode := json.Unmarshal([]byte(line), &event); errDecode != nil {
			return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image processing", Msg: "web image processing stream schema changed"}
		}
		states := uploadProcessingStates(event)
		for _, state := range states {
			if strings.Contains(state, "error") || strings.Contains(state, "failed") {
				return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindUpstream, Stage: "image processing", Msg: "web image processing failed"}
			}
		}
		for _, state := range states {
			if strings.Contains(state, "file_ready") {
				ready = true
			}
			if strings.Contains(state, "completed") {
				return nil
			}
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		if errContext := preserveContextError(errScan); errContext != nil {
			return errContext
		}
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image processing", Msg: "web image processing stream was interrupted"}
	}
	if ready {
		return nil
	}
	return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image processing", Msg: "web image processing completion is missing"}
}

func uploadProcessingStates(event map[string]any) []string {
	states := make([]string, 0, 3)
	for _, key := range []string{"type", "event", "status"} {
		value, ok := event[key].(string)
		if !ok {
			continue
		}
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			states = append(states, value)
		}
	}
	return states
}

func uploadAssetPointer(fileID string) string {
	fileID = strings.TrimSpace(fileID)
	if strings.HasPrefix(fileID, "file_") {
		return "sediment://" + fileID
	}
	return "file-service://" + fileID
}

func (e *Executor) validateUploadURL(target *url.URL) error {
	if target == nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image upload", Msg: "web image upload URL is invalid"}
	}
	if e.sameOrigin(target) {
		return nil
	}
	if !strings.EqualFold(target.Scheme, "https") {
		return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image upload", Msg: "web image upload URL must use HTTPS"}
	}
	host := strings.ToLower(target.Hostname())
	if host == "openai.com" || strings.HasSuffix(host, ".openai.com") || host == "oaiusercontent.com" || strings.HasSuffix(host, ".oaiusercontent.com") || strings.HasSuffix(host, ".blob.core.windows.net") {
		return nil
	}
	return &StatusError{Status: http.StatusBadGateway, Kind: ErrorKindProtocol, Stage: "image upload", Msg: "web image upload host is not allowed"}
}
