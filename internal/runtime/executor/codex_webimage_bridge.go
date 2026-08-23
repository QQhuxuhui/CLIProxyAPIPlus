package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
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
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/webimage"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	_ "golang.org/x/image/webp"
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
	inputFidelity := strings.TrimSpace(gjson.GetBytes(payload, "input_fidelity").String())
	background := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "background").String()))
	// Only official values affect prompting and opt in to output alpha
	// inspection; anything else is dropped from the response metadata.
	switch background {
	case "opaque", "transparent", "auto":
	default:
		background = ""
	}
	promptSize := size
	if promptSize == "" && isEdit && len(inputImages) > 0 && inputImages[0].Width > 0 && inputImages[0].Height > 0 {
		promptSize = fmt.Sprintf("%dx%d", inputImages[0].Width, inputImages[0].Height)
	}
	// The web upstream ignores output_format; honor jpeg/webp by re-encoding
	// the final bytes ourselves (see helps.EnsureImageBase64Format).
	outputFormat, outputCompression, errEncoding := codexWebImageOutputEncodingFromJSON(payload)
	if errEncoding != nil {
		return cliproxyexecutor.Response{}, badRequestErr(errEncoding)
	}
	webRequest := webimage.Request{
		Prompt: buildCodexWebImagePrompt(prompt, promptSize, quality, inputFidelity, background, isEdit),
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
	imageCount := 1
	if v := gjson.GetBytes(payload, "n"); v.Exists() && v.Type != gjson.Null {
		if v.Type != gjson.Number || v.Num != float64(int64(v.Num)) || v.Int() < 1 || v.Int() > webImageMaxImagesPerRequest {
			return cliproxyexecutor.Response{}, badRequestErr(fmt.Errorf("n must be an integer between 1 and %d for web image generation", webImageMaxImagesPerRequest))
		}
		imageCount = int(v.Int())
	}
	results, meta, errGenerate := e.generateWebImages(ctx, credentials, webRequest, imageCount)
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
		convertedResult := codexImageCallResult{
			Result:        result.Base64Data,
			RevisedPrompt: result.RevisedPrompt,
			OutputFormat:  result.OutputFormat,
			Size:          responseSize,
		}
		converted = append(converted, convertedResult)
	}
	if outputFormat == "jpeg" || outputFormat == "webp" {
		converted = codexEnsureImageResultsFormatAllOrNone(converted, outputFormat, outputCompression)
	}
	if background != "" {
		for i := range converted {
			// Inspect alpha after any requested transcode so metadata describes
			// the bytes actually returned to the client.
			converted[i].Background = codexWebImageActualBackground(converted[i].Result)
		}
	}
	firstMeta := codexResponseMetadata(converted, converted[0])
	response, errBuild := codexBuildImagesAPIResponse(converted, createdAt, nil, firstMeta, "b64_json")
	if errBuild != nil {
		return cliproxyexecutor.Response{}, errBuild
	}
	return cliproxyexecutor.Response{Payload: response}, nil
}

// webImageMaxImagesPerRequest mirrors the official n upper bound.
const webImageMaxImagesPerRequest = 10

func codexWebImageOutputEncodingFromJSON(payload []byte) (string, int, error) {
	formatValue := gjson.GetBytes(payload, "output_format")
	format := ""
	if formatValue.Exists() && formatValue.Type != gjson.Null {
		if formatValue.Type != gjson.String {
			return "", -1, fmt.Errorf("output_format must be png, jpeg, or webp")
		}
		format = strings.ToLower(strings.TrimSpace(formatValue.String()))
		if format != "png" && format != "jpeg" && format != "webp" {
			return "", -1, fmt.Errorf("output_format must be png, jpeg, or webp")
		}
	}
	compression := -1
	compressionValue := gjson.GetBytes(payload, "output_compression")
	if compressionValue.Exists() && compressionValue.Type != gjson.Null {
		if compressionValue.Type != gjson.Number || compressionValue.Num != float64(int64(compressionValue.Num)) || compressionValue.Int() < 0 || compressionValue.Int() > 100 {
			return "", -1, fmt.Errorf("output_compression must be an integer between 0 and 100")
		}
		compression = int(compressionValue.Int())
	}
	if errCombination := codexValidateImageOutputEncodingCombination(format, compression, gjson.GetBytes(payload, "background").String()); errCombination != nil {
		return "", -1, errCombination
	}
	return format, compression, nil
}

// codexEnsureImageResultsFormatAllOrNone re-encodes every result to the
// requested format, or none of them: output_format is a top-level field, so a
// response can only be truthful if all items share one format. On any failure
// the originals are kept and labeled with their actual (sniffed) format.
func codexEnsureImageResultsFormatAllOrNone(results []codexImageCallResult, format string, compression int) []codexImageCallResult {
	converted := make([]codexImageCallResult, len(results))
	copy(converted, results)
	for i := range converted {
		b64, actual, ok := helps.EnsureImageBase64Format(converted[i].Result, format, compression)
		if !ok {
			for j := range results {
				if _, actualJ, _ := helps.EnsureImageBase64Format(results[j].Result, "png", -1); actualJ != "" {
					results[j].OutputFormat = actualJ
				}
			}
			return results
		}
		converted[i].Result = b64
		converted[i].OutputFormat = actual
	}
	return converted
}

// generateWebImages serves n>1 by running the (single-image) web generation n
// times and concatenating the results in request order. The calls are issued
// together, but the executor's per-account slots (WebImageMaxConcurrencyPerAccount,
// default 1) and global slots decide how many actually run in parallel — with
// the default config they run one after another, so latency grows ~n×. Raise
// the per-account limit to parallelize. All-or-nothing: a failed generation
// cancels the rest and surfaces its error — the client indexed data[0..n-1]
// and the billing layer requires the returned count to match n.
func (e *CodexAutoExecutor) generateWebImages(ctx context.Context, credentials webimage.Credentials, request webimage.Request, n int) ([]webimage.ImageResult, *webimage.Meta, error) {
	if n <= 1 {
		results, meta, err := e.webImageExec.GenerateRequest(ctx, credentials, request)
		if err != nil {
			return nil, nil, err
		}
		return codexRequireSingleWebImageResult(results, meta)
	}
	fanCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		results []webimage.ImageResult
		meta    *webimage.Meta
		err     error
	}
	outcomes := make([]outcome, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results, meta, err := e.webImageExec.GenerateRequest(fanCtx, credentials, request)
			if err == nil {
				results, meta, err = codexRequireSingleWebImageResult(results, meta)
			}
			outcomes[index] = outcome{results: results, meta: meta, err: err}
			if err != nil {
				cancel()
			}
		}(i)
	}
	wg.Wait()
	combined := make([]webimage.ImageResult, 0, n)
	var meta *webimage.Meta
	var firstErr error
	for _, o := range outcomes {
		if o.err != nil {
			// Prefer the root-cause error over the cancellations it triggered.
			if firstErr == nil || (errors.Is(firstErr, context.Canceled) && !errors.Is(o.err, context.Canceled)) {
				firstErr = o.err
			}
			continue
		}
		if len(o.results) == 0 {
			if firstErr == nil {
				firstErr = statusErr{code: http.StatusBadGateway, msg: "web image upstream returned no image"}
			}
			continue
		}
		combined = append(combined, o.results...)
		if meta == nil {
			meta = o.meta
		}
	}
	if firstErr != nil {
		return nil, nil, firstErr
	}
	return combined, meta, nil
}

func codexRequireSingleWebImageResult(results []webimage.ImageResult, meta *webimage.Meta) ([]webimage.ImageResult, *webimage.Meta, error) {
	if len(results) != 1 {
		return nil, nil, statusErr{code: http.StatusBadGateway, msg: fmt.Sprintf("web image upstream returned %d images; expected exactly one", len(results))}
	}
	return results, meta, nil
}

func isCodexWebImageEditRequest(opts cliproxyexecutor.Options) bool {
	path := strings.TrimSpace(helps.PayloadRequestPath(opts))
	return path == codexImagesEditsPath || strings.HasSuffix(path, codexImagesEditsPath)
}

func buildCodexWebImagePrompt(prompt, size, quality, inputFidelity, background string, isEdit bool) string {
	directives := make([]string, 0, 5)
	if size != "" {
		directives = append(directives, "Requested output size: "+size+".")
		if matches := webImageDimensionsPattern.FindStringSubmatch(size); len(matches) == 3 {
			width, errWidth := strconv.ParseUint(matches[1], 10, 64)
			height, errHeight := strconv.ParseUint(matches[2], 10, 64)
			if errWidth == nil && errHeight == nil && width > 0 && height > 0 {
				switch {
				case width == height:
					directives = append(directives, "Use a square composition that follows the requested dimensions.")
				case width > height && float64(width)/float64(height) > 1.05:
					directives = append(directives, "Use a landscape composition that follows the requested dimensions.")
				case width < height && float64(height)/float64(width) > 1.05:
					directives = append(directives, "Use a portrait composition that follows the requested dimensions.")
				}
			}
		}
	}
	if isEdit {
		directives = append(directives, "Preserve the reference image's original dimensions and aspect ratio.")
	}
	// input_fidelity has no API surface on the web pipeline; translate the
	// high setting into a faithfulness directive (same mechanism as size).
	if isEdit && strings.EqualFold(strings.TrimSpace(inputFidelity), "high") {
		directives = append(directives, "Reproduce the reference image as faithfully as possible: apply the requested change precisely, and keep faces, text and fine details outside the requested edit region identical to the original.")
	}
	// Background modes are translated into natural language because the web
	// pipeline has no structured background control surface.
	switch {
	case strings.EqualFold(strings.TrimSpace(background), "transparent"):
		directives = append(directives, "The image must have a fully transparent background: render only the subject on true PNG alpha transparency, with no background color, scenery, or checkerboard pattern.")
	case strings.EqualFold(strings.TrimSpace(background), "opaque"):
		directives = append(directives, "The image must have a fully opaque background with no transparent pixels or alpha transparency.")
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

func codexWebImageActualBackground(base64Data string) string {
	data, errDecode := base64.StdEncoding.DecodeString(base64Data)
	if errDecode != nil || len(data) == 0 {
		return ""
	}
	config, _, errConfig := image.DecodeConfig(bytes.NewReader(data))
	if errConfig != nil || config.Width <= 0 || config.Height <= 0 || config.Width > 4096 || config.Height > 4096 {
		return ""
	}
	img, _, errImage := image.Decode(bytes.NewReader(data))
	if errImage != nil {
		return ""
	}
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if _, _, _, alpha := img.At(x, y).RGBA(); alpha < 0xffff {
				return "transparent"
			}
		}
	}
	return "opaque"
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
