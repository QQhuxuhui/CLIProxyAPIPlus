package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/webimage"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type codexWebImageGenerator interface {
	GenerateRequest(context.Context, webimage.Credentials, webimage.Request) ([]webimage.ImageResult, *webimage.Meta, error)
}

var webImageDimensionsPattern = regexp.MustCompile(`(?i)^\s*([0-9]+)\s*x\s*([0-9]+)\s*$`)

func isCodexWebImageRequest(opts cliproxyexecutor.Options) bool {
	if !strings.EqualFold(strings.TrimSpace(opts.SourceFormat.String()), webimage.SourceFormat) {
		return false
	}
	path := strings.TrimSpace(helps.PayloadRequestPath(opts))
	return path == codexImagesGenerationsPath || strings.HasSuffix(path, codexImagesGenerationsPath) || path == codexImagesEditsPath || strings.HasSuffix(path, codexImagesEditsPath)
}

func (e *CodexAutoExecutor) executeWebImage(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e == nil || e.webImageExec == nil {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusServiceUnavailable, msg: "web image executor is not configured"}
	}
	payload := req.Payload
	if len(opts.OriginalRequest) > 0 {
		payload = opts.OriginalRequest
	}
	prompt := strings.TrimSpace(gjson.GetBytes(payload, "prompt").String())
	if prompt == "" {
		return cliproxyexecutor.Response{}, badRequestErr(fmt.Errorf("prompt is required"))
	}
	isEdit := isCodexWebImageEditRequest(opts)
	maxInputBytes := config.DefaultWebImageMaxBytes
	if e.httpExec != nil && e.httpExec.cfg != nil && e.httpExec.cfg.WebImageMaxBytes > 0 {
		maxInputBytes = e.httpExec.cfg.WebImageMaxBytes
	}
	inputImages, errImages := parseCodexWebImageInputsWithLimits(payload, isEdit, config.WebImageMaxInputImages, maxInputBytes)
	if errImages != nil {
		return cliproxyexecutor.Response{}, badRequestErr(errImages)
	}
	size := strings.TrimSpace(gjson.GetBytes(payload, "size").String())
	quality := strings.TrimSpace(gjson.GetBytes(payload, "quality").String())
	promptSize := size
	if promptSize == "" && isEdit && len(inputImages) > 0 && inputImages[0].Width > 0 && inputImages[0].Height > 0 {
		promptSize = fmt.Sprintf("%dx%d", inputImages[0].Width, inputImages[0].Height)
	}
	webRequest := webimage.Request{
		Prompt: buildCodexWebImagePrompt(prompt, promptSize, quality, isEdit),
		Images: inputImages,
	}

	accessToken, _ := codexCreds(auth)
	credentials := webimage.Credentials{AccessToken: accessToken}
	if auth != nil {
		credentials.AuthID = auth.ID
		credentials.ProxyURL = auth.ProxyURL
		if auth.Metadata != nil {
			credentials.AccountID, _ = auth.Metadata["account_id"].(string)
		}
	}
	results, meta, errGenerate := e.webImageExec.GenerateRequest(ctx, credentials, webRequest)
	if errGenerate != nil {
		return cliproxyexecutor.Response{}, errGenerate
	}
	if len(results) == 0 {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadGateway, msg: "web image upstream returned no image"}
	}

	createdAt := time.Now().Unix()
	if meta != nil && meta.CreatedAt > 0 {
		createdAt = meta.CreatedAt
	}
	converted := make([]codexImageCallResult, 0, len(results))
	for _, result := range results {
		responseSize := codexWebImageResponseSize(promptSize, size, inputImages, isEdit, result.Base64Data)
		converted = append(converted, codexImageCallResult{
			Result:        result.Base64Data,
			RevisedPrompt: result.RevisedPrompt,
			OutputFormat:  result.OutputFormat,
			Size:          responseSize,
		})
	}
	firstMeta := converted[0]
	response, errBuild := codexBuildImagesAPIResponse(converted, createdAt, nil, firstMeta, "b64_json")
	if errBuild != nil {
		return cliproxyexecutor.Response{}, errBuild
	}
	return cliproxyexecutor.Response{Payload: response}, nil
}

func isCodexWebImageEditRequest(opts cliproxyexecutor.Options) bool {
	path := strings.TrimSpace(helps.PayloadRequestPath(opts))
	return path == codexImagesEditsPath || strings.HasSuffix(path, codexImagesEditsPath)
}

func buildCodexWebImagePrompt(prompt, size, quality string, isEdit bool) string {
	directives := make([]string, 0, 3)
	if size != "" {
		directives = append(directives, "Requested output size: "+size+".")
		if matches := webImageDimensionsPattern.FindStringSubmatch(size); len(matches) == 3 {
			width, errWidth := strconv.ParseUint(matches[1], 10, 64)
			height, errHeight := strconv.ParseUint(matches[2], 10, 64)
			if errWidth == nil && errHeight == nil && width > 0 && height > 0 {
				switch {
				case width > height:
					directives = append(directives, "Use a landscape composition that follows the requested dimensions.")
				case width < height:
					directives = append(directives, "Use a portrait composition that follows the requested dimensions.")
				default:
					directives = append(directives, "Use a square composition that follows the requested dimensions.")
				}
			}
		}
	}
	if isEdit {
		directives = append(directives, "Preserve the reference image's original dimensions and aspect ratio.")
	}
	if quality != "" {
		directives = append(directives, "Requested output quality: "+quality+".")
	}
	if len(directives) == 0 {
		return prompt
	}
	return prompt + "\n\n" + strings.Join(directives, "\n")
}

func codexWebImageResponseSize(promptSize, requestedSize string, images []webimage.InputImage, isEdit bool, base64Data string) string {
	if width, height, ok := codexWebImageOutputDimensions(base64Data); ok {
		return fmt.Sprintf("%dx%d", width, height)
	}
	if promptSize = strings.TrimSpace(promptSize); promptSize != "" {
		return promptSize
	}
	if requestedSize = strings.TrimSpace(requestedSize); requestedSize != "" {
		return requestedSize
	}
	if isEdit && len(images) > 0 && images[0].Width > 0 && images[0].Height > 0 {
		return fmt.Sprintf("%dx%d", images[0].Width, images[0].Height)
	}
	return "1024x1024"
}

func codexWebImageOutputDimensions(base64Data string) (int, int, bool) {
	data, errDecode := base64.StdEncoding.DecodeString(base64Data)
	if errDecode != nil || len(data) == 0 {
		return 0, 0, false
	}
	config, _, errConfig := image.DecodeConfig(bytes.NewReader(data))
	if errConfig != nil || config.Width <= 0 || config.Height <= 0 {
		return 0, 0, false
	}
	return config.Width, config.Height, true
}

func parseCodexWebImageInputsWithLimits(payload []byte, required bool, maxImages int, maxBytes int64) ([]webimage.InputImage, error) {
	results := make([]gjson.Result, 0, 4)
	if imageValue := gjson.GetBytes(payload, "image"); imageValue.Exists() {
		results = append(results, imageValue)
	}
	if imageValues := gjson.GetBytes(payload, "images"); imageValues.IsArray() {
		results = append(results, imageValues.Array()...)
	}
	if maxImages > 0 && len(results) > maxImages {
		return nil, fmt.Errorf("web image edits contain too many reference images")
	}
	if maxBytes <= 0 {
		maxBytes = config.DefaultWebImageMaxBytes
	}
	images := make([]webimage.InputImage, 0, len(results))
	var totalBytes int64
	for index, result := range results {
		dataURL, filename := codexWebImageDataURL(result)
		if strings.TrimSpace(dataURL) == "" {
			continue
		}
		remaining := maxBytes - totalBytes
		if remaining <= 0 {
			return nil, fmt.Errorf("web image reference images exceed the configured byte limit")
		}
		input, errDecode := decodeCodexWebImageInput(dataURL, filename, index, remaining)
		if errDecode != nil {
			return nil, errDecode
		}
		totalBytes += int64(len(input.Data))
		images = append(images, input)
	}
	if required && len(images) == 0 {
		return nil, fmt.Errorf("image is required for web image edits")
	}
	return images, nil
}

func codexWebImageDataURL(result gjson.Result) (string, string) {
	if result.Type == gjson.String {
		return result.String(), ""
	}
	filename := strings.TrimSpace(result.Get("filename").String())
	if filename == "" {
		filename = strings.TrimSpace(result.Get("name").String())
	}
	if imageURL := result.Get("image_url"); imageURL.Type == gjson.String {
		return imageURL.String(), filename
	}
	if imageURL := result.Get("image_url.url"); imageURL.Type == gjson.String {
		return imageURL.String(), filename
	}
	if rawURL := result.Get("url"); rawURL.Type == gjson.String {
		return rawURL.String(), filename
	}
	return "", filename
}

func decodeCodexWebImageInput(dataURL, filename string, index int, maxBytes int64) (webimage.InputImage, error) {
	header, encoded, ok := strings.Cut(strings.TrimSpace(dataURL), ",")
	if !ok || !strings.HasPrefix(strings.ToLower(header), "data:image/") || !strings.Contains(strings.ToLower(header), ";base64") {
		return webimage.InputImage{}, fmt.Errorf("web image edits require base64 image data URLs")
	}
	data, errDecode := decodeCodexWebImageBase64(encoded, maxBytes)
	if errDecode != nil || len(data) == 0 {
		if errDecode != nil {
			return webimage.InputImage{}, errDecode
		}
		return webimage.InputImage{}, fmt.Errorf("web image reference image data is invalid")
	}
	mediaHeader := strings.TrimSpace(strings.Split(header, ";")[0])
	separator := strings.IndexByte(mediaHeader, ':')
	if separator < 0 || separator == len(mediaHeader)-1 {
		return webimage.InputImage{}, fmt.Errorf("web image reference image data is invalid")
	}
	mimeType := strings.ToLower(strings.TrimSpace(mediaHeader[separator+1:]))
	width, height, errDimensions := codexWebImageDimensions(data, mimeType)
	if errDimensions != nil {
		return webimage.InputImage{}, fmt.Errorf("web image reference dimensions are invalid")
	}
	filename = strings.TrimSpace(filepath.Base(strings.ReplaceAll(filename, "\\", "/")))
	if filename == "" || filename == "." {
		filename = fmt.Sprintf("reference-%d%s", index+1, codexWebImageExtension(mimeType))
	}
	return webimage.InputImage{Filename: filename, MIMEType: mimeType, Data: data, Width: width, Height: height}, nil
}

func decodeCodexWebImageBase64(encoded string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("web image reference images exceed the configured byte limit")
	}
	encoding := base64.RawStdEncoding
	if strings.HasSuffix(strings.TrimSpace(encoded), "=") {
		encoding = base64.StdEncoding
	}
	reader := base64.NewDecoder(encoding, strings.NewReader(encoded))
	data, errDecode := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("web image reference images exceed the configured byte limit")
	}
	if errDecode != nil {
		return nil, fmt.Errorf("web image reference image data is invalid")
	}
	return data, nil
}

func codexWebImageDimensions(data []byte, mimeType string) (int, int, error) {
	config, _, errDecode := image.DecodeConfig(bytes.NewReader(data))
	if errDecode == nil && config.Width > 0 && config.Height > 0 {
		return config.Width, config.Height, nil
	}
	if mimeType == "image/webp" {
		return codexWebPDimensions(data)
	}
	return 0, 0, errDecode
}

func codexWebPDimensions(data []byte) (int, int, error) {
	if len(data) < 30 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, fmt.Errorf("invalid WebP image")
	}
	switch string(data[12:16]) {
	case "VP8X":
		width := 1 + int(data[24]) + (int(data[25]) << 8) + (int(data[26]) << 16)
		height := 1 + int(data[27]) + (int(data[28]) << 8) + (int(data[29]) << 16)
		return width, height, nil
	case "VP8 ":
		if len(data) < 30 || data[23] != 0x9d || data[24] != 0x01 || data[25] != 0x2a {
			return 0, 0, fmt.Errorf("invalid WebP image")
		}
		width := int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff)
		height := int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff)
		return width, height, nil
	case "VP8L":
		if len(data) < 25 || data[20] != 0x2f {
			return 0, 0, fmt.Errorf("invalid WebP image")
		}
		width := 1 + int(data[21]) + (int(data[22]&0x3f) << 8)
		height := 1 + (int(data[23]) << 2) + (int(data[24]&0x0f) << 10) + (int(data[22]&0xc0) >> 6)
		return width, height, nil
	default:
		return 0, 0, fmt.Errorf("invalid WebP image")
	}
}

func codexWebImageExtension(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}
