package openai

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/tiktoken-go/tokenizer"
)

const (
	chatWebImageDefaultKeepAlive   = 15 * time.Second
	chatWebImageMaxChoices         = 10
	chatWebImageMaxRedirects       = 3
	chatWebImageInvalidImageURLMsg = "image_url must be a data URL or an http(s) URL"
)

// chatWebImageAllowedMIMETypes lists the reference and output image types the web
// image pipeline can decode (see codexWebImageDimensions in the executor).
var chatWebImageAllowedMIMETypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// chatWebImageNonRoutablePrefixes mirrors nonRoutableProxyTargetPrefixes in
// internal/client/codex/live/tcp_proxy.go: special-purpose ranges that must never be
// reached when fetching a client-supplied reference image URL.
var chatWebImageNonRoutablePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("::ffff:0:0:0/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// isPublicWebImageReferenceAddr reports whether a reference image download may connect
// to address. It applies the same rule as isPublicProxyTarget in the codex live package.
func isPublicWebImageReferenceAddr(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsUnspecified() || address.IsLoopback() ||
		address.IsPrivate() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() {
		return false
	}
	for _, prefix := range chatWebImageNonRoutablePrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

// chatWebImageError is a client-facing failure detected before or after web image execution.
type chatWebImageError struct {
	status  int
	message string
	errType string
}

func invalidChatWebImageRequest(format string, args ...any) *chatWebImageError {
	return &chatWebImageError{status: http.StatusBadRequest, message: "Invalid request: " + fmt.Sprintf(format, args...), errType: "invalid_request_error"}
}

func (e *chatWebImageError) write(c *gin.Context) {
	c.JSON(e.status, handlers.ErrorResponse{Error: handlers.ErrorDetail{Message: e.message, Type: e.errType}})
}

// chatWebImageRequest is the part of a chat request the web image pipeline consumes.
type chatWebImageRequest struct {
	prompt       string
	imageURLs    []string
	n            int
	includeUsage bool
}

// parseChatWebImageRequest extracts the prompt, reference images and choice count from the
// original client body. It reads the last user turn by its original role, so Responses-shaped
// bodies are inspected before the chat conversion rewrites developer or tool items as user.
func parseChatWebImageRequest(rawJSON []byte) (chatWebImageRequest, *chatWebImageError) {
	parsed := chatWebImageRequest{n: 1}
	if n := gjson.GetBytes(rawJSON, "n"); n.Exists() && n.Type != gjson.Null {
		if n.Type != gjson.Number || n.Num != float64(int64(n.Num)) || n.Int() < 1 || n.Int() > chatWebImageMaxChoices {
			return parsed, invalidChatWebImageRequest("n must be an integer between 1 and %d for web image generation", chatWebImageMaxChoices)
		}
		parsed.n = int(n.Int())
	}
	for _, path := range []string{"response_format.type", "text.format.type"} {
		switch strings.TrimSpace(gjson.GetBytes(rawJSON, path).String()) {
		case "json_object", "json_schema":
			return parsed, invalidChatWebImageRequest("response_format json_object/json_schema is not supported for web image models")
		}
	}
	parsed.includeUsage = gjson.GetBytes(rawJSON, "stream_options.include_usage").Bool()

	var errParse *chatWebImageError
	if shouldTreatAsResponsesFormat(rawJSON) {
		parsed.prompt, parsed.imageURLs, errParse = lastResponsesUserTurn(gjson.GetBytes(rawJSON, "input"))
	} else {
		parsed.prompt, parsed.imageURLs, errParse = lastChatUserTurn(gjson.GetBytes(rawJSON, "messages"))
	}
	if errParse != nil {
		return parsed, errParse
	}
	if strings.TrimSpace(parsed.prompt) == "" {
		return parsed, invalidChatWebImageRequest("prompt is required")
	}
	parsed.prompt = strings.TrimSpace(parsed.prompt)
	if len(parsed.imageURLs) > internalconfig.WebImageMaxInputImages {
		return parsed, invalidChatWebImageRequest("web image edits contain too many reference images")
	}
	return parsed, nil
}

func lastChatUserTurn(messages gjson.Result) (string, []string, *chatWebImageError) {
	if !messages.IsArray() {
		return "", nil, nil
	}
	items := messages.Array()
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Get("role").String() != "user" {
			continue
		}
		content := items[i].Get("content")
		if content.Type == gjson.String {
			return content.String(), nil, nil
		}
		var texts, images []string
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "text":
				texts = append(texts, part.Get("text").String())
			case "image_url":
				imageURL := part.Get("image_url")
				if imageURL.IsObject() {
					imageURL = imageURL.Get("url")
				}
				if imageURL.Type != gjson.String || strings.TrimSpace(imageURL.String()) == "" {
					return "", nil, invalidChatWebImageRequest(chatWebImageInvalidImageURLMsg)
				}
				images = append(images, strings.TrimSpace(imageURL.String()))
			}
		}
		return strings.Join(texts, "\n"), images, nil
	}
	return "", nil, nil
}

func lastResponsesUserTurn(input gjson.Result) (string, []string, *chatWebImageError) {
	if input.Type == gjson.String {
		return input.String(), nil, nil
	}
	if !input.IsArray() {
		return "", nil, nil
	}
	items := input.Array()
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		itemType := item.Get("type").String()
		if item.Get("role").String() != "user" || (itemType != "" && itemType != "message") {
			continue
		}
		content := item.Get("content")
		if content.Type == gjson.String {
			return content.String(), nil, nil
		}
		var texts, images []string
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "input_text", "text":
				texts = append(texts, part.Get("text").String())
			case "input_image":
				imageURL := part.Get("image_url")
				if imageURL.IsObject() {
					imageURL = imageURL.Get("url")
				}
				if imageURL.Type != gjson.String || strings.TrimSpace(imageURL.String()) == "" {
					return "", nil, invalidChatWebImageRequest(chatWebImageInvalidImageURLMsg)
				}
				images = append(images, strings.TrimSpace(imageURL.String()))
			}
		}
		return strings.Join(texts, "\n"), images, nil
	}
	return "", nil, nil
}

// webImageReferenceFetcher downloads http(s) reference images while refusing to connect to
// non-public addresses. The address check runs at dial time, so every redirect hop and every
// DNS answer is validated.
type webImageReferenceFetcher struct {
	allow func(netip.AddrPort) bool
}

func newWebImageReferenceFetcher(allow func(netip.AddrPort) bool) *webImageReferenceFetcher {
	return &webImageReferenceFetcher{allow: allow}
}

// client builds a one-off client whose dials and TLS handshakes are bound to ctx. The shared
// http.Transport dials with a context detached from the request, so a cancelled download could
// otherwise leave a stalled handshake holding a socket; keep-alives are disabled for the same
// reason, so no connection outlives its download.
func (f *webImageReferenceFetcher) client(ctx context.Context) *http.Client {
	dialer := &net.Dialer{
		Control: func(_, address string, _ syscall.RawConn) error {
			addrPort, errParse := netip.ParseAddrPort(address)
			if errParse != nil || !f.allow(netip.AddrPortFrom(addrPort.Addr().Unmap(), addrPort.Port())) {
				return fmt.Errorf("reference image address %s is not allowed", address)
			}
			return nil
		},
	}
	tlsDialer := &tls.Dialer{NetDialer: dialer}
	transport := &http.Transport{
		// Never use an environment proxy: the proxy would make the dial-time address check moot.
		Proxy:             nil,
		DisableKeepAlives: true,
		DialContext: func(_ context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
		DialTLSContext: func(_ context.Context, network, address string) (net.Conn, error) {
			return tlsDialer.DialContext(ctx, network, address)
		},
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > chatWebImageMaxRedirects {
				return fmt.Errorf("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to unsupported scheme")
			}
			return nil
		},
	}
}

// chatWebImageFetcher is replaced in tests to reach local servers.
var chatWebImageFetcher = newWebImageReferenceFetcher(func(addrPort netip.AddrPort) bool {
	return isPublicWebImageReferenceAddr(addrPort.Addr())
})

// fetch downloads at most limit bytes; it returns errReferenceTooLarge when the body is larger.
func (f *webImageReferenceFetcher) fetch(ctx context.Context, rawURL string, limit int64) ([]byte, int, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if errReq != nil {
		return nil, 0, errReq
	}
	resp, errDo := f.client(ctx).Do(req)
	if errDo != nil {
		return nil, 0, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("close reference image response body: %v", errClose)
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, resp.StatusCode, nil
	}
	data, errRead := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if errRead != nil {
		return nil, 0, errRead
	}
	if int64(len(data)) > limit {
		return nil, 0, errReferenceTooLarge
	}
	return data, resp.StatusCode, nil
}

var errReferenceTooLarge = errors.New("reference image exceeds the remaining byte budget")

// webImageReferenceDownloadLimiter bounds how many requests download reference images at
// once, across requests. Capacity follows web-image-max-concurrency.
type webImageReferenceDownloadLimiter struct {
	mu       sync.Mutex
	capacity int
	slots    chan struct{}
}

var chatWebImageDownloadLimiter = &webImageReferenceDownloadLimiter{}

func (l *webImageReferenceDownloadLimiter) acquire(ctx context.Context, capacity int) (func(), error) {
	if capacity <= 0 {
		capacity = internalconfig.DefaultWebImageMaxConcurrency
	}
	l.mu.Lock()
	if l.slots == nil || l.capacity != capacity {
		l.slots = make(chan struct{}, capacity)
		l.capacity = capacity
	}
	slots := l.slots
	l.mu.Unlock()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// normalizeChatWebImageReferences turns every reference into a base64 data URL whose media
// type matches the actual bytes, enforcing one shared decoded-byte budget.
func normalizeChatWebImageReferences(ctx context.Context, cfg *internalconfig.SDKConfig, imageURLs []string) ([]string, *chatWebImageError, error) {
	if len(imageURLs) == 0 {
		return nil, nil, nil
	}
	budget := internalconfig.DefaultWebImageMaxBytes
	capacity := 0
	if cfg != nil {
		if cfg.WebImageMaxBytes > 0 {
			budget = cfg.WebImageMaxBytes
		}
		capacity = cfg.WebImageMaxConcurrency
	}
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	tooLarge := invalidChatWebImageRequest("web image reference images exceed the configured byte limit")
	normalized := make([]string, 0, len(imageURLs))
	for index, imageURL := range imageURLs {
		var data []byte
		switch {
		case hasPrefixFold(imageURL, "data:"):
			header, encoded, ok := strings.Cut(imageURL, ",")
			if !ok || !strings.Contains(strings.ToLower(header), ";base64") {
				return nil, invalidChatWebImageRequest("web image reference image data is invalid"), nil
			}
			// Decode as a stream bounded by the remaining budget, like the executor's
			// decodeCodexWebImageBase64, so oversized inline images are never fully allocated.
			encoding := base64.RawStdEncoding
			if strings.HasSuffix(encoded, "=") {
				encoding = base64.StdEncoding
			}
			decoded, errDecode := io.ReadAll(io.LimitReader(base64.NewDecoder(encoding, strings.NewReader(encoded)), budget+1))
			if int64(len(decoded)) > budget {
				return nil, tooLarge, nil
			}
			if errDecode != nil || len(decoded) == 0 {
				return nil, invalidChatWebImageRequest("web image reference image data is invalid"), nil
			}
			data = decoded
		case hasPrefixFold(imageURL, "http://") || hasPrefixFold(imageURL, "https://"):
			if release == nil {
				releaseSlot, errAcquire := chatWebImageDownloadLimiter.acquire(ctx, capacity)
				if errAcquire != nil {
					return nil, nil, errAcquire
				}
				release = releaseSlot
			}
			downloaded, status, errFetch := chatWebImageFetcher.fetch(ctx, imageURL, budget)
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			if errors.Is(errFetch, errReferenceTooLarge) {
				return nil, tooLarge, nil
			}
			if errFetch != nil {
				return nil, invalidChatWebImageRequest("failed to fetch reference image %d", index), nil
			}
			if status < 200 || status > 299 {
				return nil, invalidChatWebImageRequest("failed to fetch reference image %d: status %d", index, status), nil
			}
			data = downloaded
		default:
			return nil, invalidChatWebImageRequest(chatWebImageInvalidImageURLMsg), nil
		}
		budget -= int64(len(data))
		mimeType := http.DetectContentType(data)
		if !chatWebImageAllowedMIMETypes[mimeType] {
			return nil, invalidChatWebImageRequest("reference image %d is not a supported image", index), nil
		}
		normalized = append(normalized, "data:"+mimeType+";base64,"+base64.StdEncoding.EncodeToString(data))
	}
	return normalized, nil, nil
}

func hasPrefixFold(value, prefix string) bool {
	return len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix)
}

// buildChatWebImageImagesRequest builds the internal Images API body for the web executor.
// Only fields defined by the chat protocol are forwarded.
func buildChatWebImageImagesRequest(model string, parsed chatWebImageRequest, references []string) []byte {
	body := []byte(`{"response_format":"b64_json"}`)
	body, _ = sjson.SetBytes(body, "model", model)
	body, _ = sjson.SetBytes(body, "prompt", parsed.prompt)
	body, _ = sjson.SetBytes(body, "n", parsed.n)
	for index, reference := range references {
		body, _ = sjson.SetBytes(body, fmt.Sprintf("images.%d.image_url", index), reference)
	}
	return body
}

// chatWebImageMarkdown converts an Images API response into one markdown image per choice.
// It requires exactly n non-empty images.
func chatWebImageMarkdown(imagesResponse []byte, n int) ([]string, int64, *chatWebImageError) {
	data := gjson.GetBytes(imagesResponse, "data").Array()
	unexpected := &chatWebImageError{status: http.StatusBadGateway, message: "web image upstream returned an unexpected number of images", errType: "server_error"}
	if len(data) != n {
		return nil, 0, unexpected
	}
	fallbackMIME := "image/png"
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(imagesResponse, "output_format").String())) {
	case "jpeg", "jpg":
		fallbackMIME = "image/jpeg"
	case "webp":
		fallbackMIME = "image/webp"
	}
	contents := make([]string, 0, n)
	for _, item := range data {
		b64 := strings.TrimSpace(item.Get("b64_json").String())
		if b64 == "" {
			return nil, 0, unexpected
		}
		contents = append(contents, "![image](data:"+chatWebImageOutputMIME(b64, fallbackMIME)+";base64,"+b64+")")
	}
	created := gjson.GetBytes(imagesResponse, "created").Int()
	if created <= 0 {
		created = time.Now().Unix()
	}
	return contents, created, nil
}

// chatWebImageOutputMIME sniffs each image separately: the executor drops the top-level
// output_format when results disagree.
func chatWebImageOutputMIME(b64 string, fallback string) string {
	prefix := b64
	if len(prefix) > 64 {
		prefix = prefix[:64]
	}
	head, errDecode := base64.StdEncoding.DecodeString(prefix[:len(prefix)/4*4])
	if errDecode != nil || len(head) == 0 {
		return fallback
	}
	if sniffed := http.DetectContentType(head); chatWebImageAllowedMIMETypes[sniffed] {
		return sniffed
	}
	return fallback
}

var chatWebImagePromptCodec = sync.OnceValues(func() (tokenizer.Codec, error) {
	return tokenizer.Get(tokenizer.O200kBase)
})

// chatWebImagePromptTokenChunk bounds each tokenizer call: BPE merging is quadratic in the
// length of a single pre-tokenized piece, and prompts have no length limit.
const chatWebImagePromptTokenChunk = 1024

// chatWebImagePromptTokens estimates prompt tokens in bounded UTF-8 chunks, stopping when ctx is
// cancelled. It never returns 0, because a zero prompt count makes downstream gateways recount
// the whole (base64) assistant content as text.
func chatWebImagePromptTokens(ctx context.Context, prompt string) int64 {
	codec, errCodec := chatWebImagePromptCodec()
	if errCodec != nil {
		return 1
	}
	var total int64
	for len(prompt) > 0 {
		if ctx.Err() != nil {
			break
		}
		end := min(len(prompt), chatWebImagePromptTokenChunk)
		for end < len(prompt) && end > 0 && !utf8.RuneStart(prompt[end]) {
			end--
		}
		if end == 0 {
			// Invalid UTF-8 at the chunk start: cut at the byte limit instead.
			end = min(len(prompt), chatWebImagePromptTokenChunk)
		}
		if count, errCount := codec.Count(prompt[:end]); errCount == nil {
			total += int64(count)
		}
		prompt = prompt[end:]
	}
	return max(total, 1)
}

func newChatCompletionID() string {
	buf := make([]byte, 12)
	if _, errRead := rand.Read(buf); errRead != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + hex.EncodeToString(buf)
}

func chatWebImageUsage(promptTokens int64) []byte {
	usage := []byte(`{"completion_tokens":0}`)
	usage, _ = sjson.SetBytes(usage, "prompt_tokens", promptTokens)
	usage, _ = sjson.SetBytes(usage, "total_tokens", promptTokens)
	return usage
}

func buildChatWebImageCompletion(id, model string, created int64, contents []string, promptTokens int64) []byte {
	body := []byte(`{"object":"chat.completion","choices":[]}`)
	body, _ = sjson.SetBytes(body, "id", id)
	body, _ = sjson.SetBytes(body, "created", created)
	body, _ = sjson.SetBytes(body, "model", model)
	for index, content := range contents {
		choice := []byte(`{"message":{"role":"assistant"},"finish_reason":"stop"}`)
		choice, _ = sjson.SetBytes(choice, "index", index)
		choice, _ = sjson.SetBytes(choice, "message.content", content)
		body, _ = sjson.SetRawBytes(body, "choices.-1", choice)
	}
	body, _ = sjson.SetRawBytes(body, "usage", chatWebImageUsage(promptTokens))
	return body
}

func buildChatWebImageChunk(id, model string, created int64, choice []byte) []byte {
	chunk := []byte(`{"object":"chat.completion.chunk","choices":[]}`)
	chunk, _ = sjson.SetBytes(chunk, "id", id)
	chunk, _ = sjson.SetBytes(chunk, "created", created)
	chunk, _ = sjson.SetBytes(chunk, "model", model)
	if choice != nil {
		chunk, _ = sjson.SetRawBytes(chunk, "choices.-1", choice)
	}
	return chunk
}

// chatWebImageKeepAliveTicker is replaced in tests to drive keep-alives deterministically.
var chatWebImageKeepAliveTicker = func(interval time.Duration) (<-chan time.Time, func()) {
	ticker := time.NewTicker(interval)
	return ticker.C, ticker.Stop
}

type chatWebImageResult struct {
	body    []byte
	headers http.Header
	errMsg  *interfaces.ErrorMessage
}

// chatCompletionsWebImage serves a chat completion for a web image model by running the web
// image pipeline and returning each image as a markdown data URL.
func (h *OpenAIAPIHandler) chatCompletionsWebImage(c *gin.Context, originalJSON []byte, model string, stream bool) {
	var cfg *internalconfig.SDKConfig
	if h != nil && h.BaseAPIHandler != nil {
		cfg = h.BaseAPIHandler.Cfg
	}
	if !json.Valid(originalJSON) {
		invalidChatWebImageRequest("body must be valid JSON").write(c)
		return
	}
	if cfg == nil || cfg.DisableImageGeneration == internalconfig.DisableImageGenerationAll || !cfg.WebImageGeneration {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if strings.TrimSpace(cfg.WebImageBaseModel) == "" {
		(&chatWebImageError{status: http.StatusServiceUnavailable, message: "Web image generation is not configured", errType: "server_error"}).write(c)
		return
	}
	parsed, errParse := parseChatWebImageRequest(originalJSON)
	if errParse != nil {
		errParse.write(c)
		return
	}

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	if cfg.WebImageFreeOnly {
		cliCtx = handlers.WithOnlyFreeAuth(cliCtx)
	}
	references, errReference, errCtx := normalizeChatWebImageReferences(cliCtx, cfg, parsed.imageURLs)
	if errCtx != nil {
		cliCancel(errCtx)
		return
	}
	if errReference != nil {
		errReference.write(c)
		cliCancel(nil)
		return
	}
	executorPath := imagesGenerationsPath
	if len(references) > 0 {
		executorPath = imagesEditsPath
	}
	imageReq := buildChatWebImageImagesRequest(model, parsed, references)
	execute := func() chatWebImageResult {
		body, headers, errMsg := h.ExecuteWebImageForPathWithAuthManager(cliCtx, webImagesHandlerType, model, imageReq, "", internalconfig.DefaultWebImageModel, executorPath)
		return chatWebImageResult{body: body, headers: headers, errMsg: errMsg}
	}
	id := newChatCompletionID()

	if !stream {
		c.Header("Content-Type", "application/json")
		stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
		result := execute()
		stopKeepAlive()
		if result.errMsg != nil {
			h.WriteErrorResponse(c, result.errMsg)
			cliCancel(result.errMsg.Error)
			return
		}
		contents, created, errImages := chatWebImageMarkdown(result.body, parsed.n)
		if errImages != nil {
			errImages.write(c)
			cliCancel(nil)
			return
		}
		handlers.WriteUpstreamHeaders(c.Writer.Header(), result.headers)
		_, _ = c.Writer.Write(buildChatWebImageCompletion(id, model, created, contents, chatWebImagePromptTokens(cliCtx, parsed.prompt)))
		cliCancel(nil)
		return
	}

	h.streamChatWebImage(c, cfg, cliCancel, execute, chatWebImageStream{id: id, model: model, n: parsed.n, includeUsage: parsed.includeUsage, prompt: parsed.prompt})
}

type chatWebImageStream struct {
	id           string
	model        string
	n            int
	includeUsage bool
	prompt       string
}

// streamChatWebImage runs the generation while sending SSE keep-alives. Headers are committed
// on the first write, so failures before any keep-alive still return a real status code.
func (h *OpenAIAPIHandler) streamChatWebImage(c *gin.Context, cfg *internalconfig.SDKConfig, cliCancel handlers.APIHandlerCancelFunc, execute func() chatWebImageResult, meta chatWebImageStream) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		(&chatWebImageError{status: http.StatusInternalServerError, message: "Streaming not supported", errType: "server_error"}).write(c)
		cliCancel(nil)
		return
	}
	interval := handlers.StreamingKeepAliveInterval(cfg)
	if interval <= 0 {
		interval = chatWebImageDefaultKeepAlive
	}
	ticks, stopTicks := chatWebImageKeepAliveTicker(interval)
	defer stopTicks()

	results := make(chan chatWebImageResult, 1)
	go func() { results <- execute() }()

	committed := false
	commit := func(headers http.Header) {
		if committed {
			return
		}
		committed = true
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
		handlers.WriteUpstreamHeaders(c.Writer.Header(), headers)
	}
	writeTerminalError := func(status int, message string) {
		_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", string(handlers.BuildErrorResponseBody(status, message)))
		flusher.Flush()
	}

	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			// The execution goroutine reads the gin context through cliCtx; wait for it to
			// unwind so gin does not recycle the context while it is still in use.
			<-results
			return
		case <-ticks:
			commit(nil)
			_, _ = fmt.Fprint(c.Writer, ": keep-alive\n\n")
			flusher.Flush()
		case result := <-results:
			if result.errMsg != nil {
				if !committed {
					h.WriteErrorResponse(c, result.errMsg)
				} else {
					status := http.StatusInternalServerError
					if result.errMsg.StatusCode > 0 {
						status = result.errMsg.StatusCode
					}
					message := http.StatusText(status)
					if result.errMsg.Error != nil && result.errMsg.Error.Error() != "" {
						message = result.errMsg.Error.Error()
					}
					writeTerminalError(status, message)
				}
				cliCancel(result.errMsg.Error)
				return
			}
			contents, created, errImages := chatWebImageMarkdown(result.body, meta.n)
			if errImages != nil {
				if !committed {
					errImages.write(c)
				} else {
					writeTerminalError(errImages.status, errImages.message)
				}
				cliCancel(nil)
				return
			}
			commit(result.headers)
			for index, content := range contents {
				choice := []byte(`{"delta":{"role":"assistant"},"finish_reason":null}`)
				choice, _ = sjson.SetBytes(choice, "index", index)
				choice, _ = sjson.SetBytes(choice, "delta.content", content)
				_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", string(buildChatWebImageChunk(meta.id, meta.model, created, choice)))
			}
			for index := range contents {
				choice := []byte(`{"delta":{},"finish_reason":"stop"}`)
				choice, _ = sjson.SetBytes(choice, "index", index)
				_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", string(buildChatWebImageChunk(meta.id, meta.model, created, choice)))
			}
			if meta.includeUsage {
				usageChunk := buildChatWebImageChunk(meta.id, meta.model, created, nil)
				usageChunk, _ = sjson.SetRawBytes(usageChunk, "usage", chatWebImageUsage(chatWebImagePromptTokens(c.Request.Context(), meta.prompt)))
				_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", string(usageChunk))
			}
			_, _ = fmt.Fprint(c.Writer, "data: [DONE]\n\n")
			flusher.Flush()
			cliCancel(nil)
			return
		}
	}
}
