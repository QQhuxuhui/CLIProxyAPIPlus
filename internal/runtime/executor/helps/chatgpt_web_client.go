package helps

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"golang.org/x/crypto/sha3"
)

const (
	chatGPTWebDefaultBaseURL       = "https://chatgpt.com"
	chatGPTWebDefaultUserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	chatGPTWebDefaultClientVersion = "prod-be885abbfcfe7b1f511e88b3003d9ee44757fbad"
	chatGPTWebDefaultClientBuild   = "5955942"
	chatGPTWebDefaultTextModel     = "gpt-5-5"
	chatGPTWebDefaultImageModel    = "auto"
	chatGPTWebDefaultPollInterval  = 6 * time.Second
	chatGPTWebDefaultMaxImageBytes = 20 << 20
	chatGPTWebDefaultMaxPoW        = 500_000
	chatGPTWebMaxJSONBytes         = 16 << 20
)

var chatGPTWebHexPattern = regexp.MustCompile(`^[0-9a-f]+$`)

// ChatGPTWebError carries a safe public error code and an upstream HTTP status.
type ChatGPTWebError struct {
	Status  int
	Code    string
	Message string
	Cause   error
}

func (e *ChatGPTWebError) Error() string {
	if e == nil {
		return "chatgpt web error"
	}
	if strings.TrimSpace(e.Code) == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

func (e *ChatGPTWebError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// StatusCode allows the auth conductor to apply its existing retry and cooldown policy.
func (e *ChatGPTWebError) StatusCode() int {
	if e == nil || e.Status == 0 {
		return http.StatusBadGateway
	}
	return e.Status
}

type chatGPTWebRequirements struct {
	Token      string
	ProofToken string
}

// ChatGPTWebResult is the provider-neutral result returned to the executor.
type ChatGPTWebResult struct {
	Text           string
	ConversationID string
	Image          []byte
	MIMEType       string
	RevisedPrompt  string
}

// ChatGPTWebClient implements the isolated private Web protocol for one selected auth.
type ChatGPTWebClient struct {
	httpClient        *http.Client
	baseURL           *url.URL
	accessToken       string
	cookie            string
	requirementsToken string
	deviceID          string
	sessionID         string
	userAgent         string
	clientVersion     string
	clientBuild       string
	textModel         string
	imageModel        string
	pollInterval      time.Duration
	maxImageBytes     int64
	maxPoWIterations  int
	textBootstrap     bool
	textPrepare       bool
	imageBootstrap    bool
	imagePrepare      bool
	requirementsV2    bool
}

// NewChatGPTWebClient creates a per-request client bound to one auth selected by Auth Manager.
func NewChatGPTWebClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) (*ChatGPTWebClient, error) {
	if auth == nil {
		return nil, newChatGPTWebError(http.StatusServiceUnavailable, "credentials_missing", "ChatGPT Web auth is missing", nil)
	}
	accessToken := chatGPTWebMetadataString(auth.Metadata, "access_token", "accessToken")
	if accessToken == "" {
		if token, ok := auth.Metadata["token"].(map[string]any); ok {
			accessToken = chatGPTWebMetadataString(token, "access_token", "accessToken")
		}
	}
	cookie := chatGPTWebMetadataString(auth.Metadata, "cookie")
	if accessToken == "" && cookie == "" {
		return nil, newChatGPTWebError(http.StatusServiceUnavailable, "credentials_missing", "ChatGPT Web access_token or cookie is required", nil)
	}

	baseRaw := chatGPTWebMetadataString(auth.Metadata, "base_url")
	if baseRaw == "" {
		baseRaw = chatGPTWebDefaultBaseURL
	}
	baseURL, errParse := url.Parse(strings.TrimRight(baseRaw, "/"))
	if errParse != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, newChatGPTWebError(http.StatusBadRequest, "base_url_invalid", "ChatGPT Web base_url is invalid", errParse)
	}

	client := NewUtlsHTTPClient(ctx, cfg, auth, 0)
	jar, errJar := cookiejar.New(nil)
	if errJar != nil {
		return nil, fmt.Errorf("chatgpt web: create cookie jar: %w", errJar)
	}
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	deviceSeed := strings.TrimSpace(auth.ID)
	if deviceSeed == "" {
		deviceSeed = strings.TrimSpace(auth.FileName)
	}
	if deviceSeed == "" {
		deviceSeed = accessToken
	}
	deviceID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(deviceSeed)).String()

	return &ChatGPTWebClient{
		httpClient:        client,
		baseURL:           baseURL,
		accessToken:       accessToken,
		cookie:            cookie,
		requirementsToken: chatGPTWebMetadataString(auth.Metadata, "requirements_token"),
		deviceID:          deviceID,
		sessionID:         uuid.NewString(),
		userAgent:         chatGPTWebMetadataStringDefault(auth.Metadata, chatGPTWebDefaultUserAgent, "user_agent"),
		clientVersion:     chatGPTWebMetadataStringDefault(auth.Metadata, chatGPTWebDefaultClientVersion, "client_version"),
		clientBuild:       chatGPTWebMetadataStringDefault(auth.Metadata, chatGPTWebDefaultClientBuild, "client_build_number"),
		textModel:         chatGPTWebMetadataStringDefault(auth.Metadata, chatGPTWebDefaultTextModel, "text_model"),
		imageModel:        chatGPTWebMetadataStringDefault(auth.Metadata, chatGPTWebDefaultImageModel, "image_model"),
		pollInterval:      chatGPTWebMetadataDuration(auth.Metadata, "poll_interval_seconds", chatGPTWebDefaultPollInterval),
		maxImageBytes:     int64(chatGPTWebMetadataInt(auth.Metadata, "max_image_bytes", chatGPTWebDefaultMaxImageBytes)),
		maxPoWIterations:  chatGPTWebMetadataInt(auth.Metadata, "max_pow_iterations", chatGPTWebDefaultMaxPoW),
		textBootstrap:     chatGPTWebMetadataBool(auth.Metadata, "text_bootstrap", false),
		textPrepare:       chatGPTWebMetadataBool(auth.Metadata, "text_prepare", false),
		imageBootstrap:    chatGPTWebMetadataBool(auth.Metadata, "image_bootstrap", true),
		imagePrepare:      chatGPTWebMetadataBool(auth.Metadata, "image_prepare", true),
		requirementsV2:    chatGPTWebMetadataBool(auth.Metadata, "requirements_v2", true),
	}, nil
}

func newChatGPTWebError(status int, code, message string, cause error) *ChatGPTWebError {
	return &ChatGPTWebError{Status: status, Code: code, Message: message, Cause: cause}
}

func chatGPTWebMetadataString(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func chatGPTWebMetadataStringDefault(metadata map[string]any, fallback string, keys ...string) string {
	if value := chatGPTWebMetadataString(metadata, keys...); value != "" {
		return value
	}
	return fallback
}

func chatGPTWebMetadataInt(metadata map[string]any, key string, fallback int) int {
	value, ok := metadata[key]
	if !ok {
		return fallback
	}
	var parsed int64
	switch current := value.(type) {
	case float64:
		parsed = int64(current)
	case json.Number:
		parsed, _ = current.Int64()
	case int:
		parsed = int64(current)
	case int64:
		parsed = current
	case string:
		parsed, _ = strconv.ParseInt(strings.TrimSpace(current), 10, 64)
	}
	if parsed <= 0 || parsed > int64(^uint(0)>>1) {
		return fallback
	}
	return int(parsed)
}

func chatGPTWebMetadataDuration(metadata map[string]any, key string, fallback time.Duration) time.Duration {
	seconds := chatGPTWebMetadataInt(metadata, key, int(fallback/time.Second))
	return time.Duration(seconds) * time.Second
}

func chatGPTWebMetadataBool(metadata map[string]any, key string, fallback bool) bool {
	value, ok := metadata[key]
	if !ok {
		return fallback
	}
	switch current := value.(type) {
	case bool:
		return current
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(current))
		if errParse == nil {
			return parsed
		}
	}
	return fallback
}

func (c *ChatGPTWebClient) ensureAccessToken(ctx context.Context) error {
	if c.accessToken != "" {
		return nil
	}
	resp, errDo := c.do(ctx, http.MethodGet, "/api/auth/session", nil, nil, true)
	if errDo != nil {
		return errDo
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.httpError("session_auth", resp.StatusCode)
	}
	payload, errJSON := chatGPTWebReadJSON(resp.Body)
	if errJSON != nil {
		return newChatGPTWebError(http.StatusBadGateway, "session_invalid", "ChatGPT session returned invalid JSON", errJSON)
	}
	c.accessToken = chatGPTWebMetadataString(payload, "accessToken", "access_token")
	if c.accessToken == "" {
		return newChatGPTWebError(http.StatusUnauthorized, "session_expired", "ChatGPT cookie session has no access token", nil)
	}
	return nil
}

func (c *ChatGPTWebClient) bootstrap(ctx context.Context) error {
	resp, errDo := c.do(ctx, http.MethodGet, "/", nil, nil, true)
	if errDo != nil {
		return errDo
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return c.httpError("bootstrap", resp.StatusCode)
	}
	return nil
}

func (c *ChatGPTWebClient) requirements(ctx context.Context) (chatGPTWebRequirements, error) {
	pToken := c.requirementsToken
	if pToken == "" {
		pToken = c.generatedRequirementsToken()
	}
	payload := map[string]any{"p": pToken}
	if c.requirementsV2 {
		prepared, ok := c.requirementsV2Prepare(ctx, payload)
		if ok {
			return prepared, nil
		}
	}

	response, errPost := c.postJSON(ctx, "/backend-api/sentinel/chat-requirements", payload, nil)
	if errPost != nil {
		return chatGPTWebRequirements{}, errPost
	}
	if required, _ := chatGPTWebNestedBool(response, "arkose", "required"); required {
		return chatGPTWebRequirements{}, newChatGPTWebError(http.StatusForbidden, "arkose_required", "ChatGPT requires interactive Arkose verification", nil)
	}
	token := chatGPTWebFirstString(response, "token", "chat_token", "chat_requirements_token", "requirements_token")
	pow := chatGPTWebMap(response["proofofwork"])
	if len(pow) == 0 {
		pow = chatGPTWebMap(response["proof_of_work"])
	}
	proofToken, errPoW := c.solveRequiredPoW(pow)
	if errPoW != nil {
		return chatGPTWebRequirements{}, errPoW
	}
	if token == "" {
		return chatGPTWebRequirements{}, newChatGPTWebError(http.StatusBadGateway, "requirements_missing", "Sentinel did not return a requirements token", nil)
	}
	return chatGPTWebRequirements{Token: token, ProofToken: proofToken}, nil
}

func (c *ChatGPTWebClient) requirementsV2Prepare(ctx context.Context, payload map[string]any) (chatGPTWebRequirements, bool) {
	prepared, errPrepare := c.postJSON(ctx, "/backend-api/sentinel/chat-requirements/prepare", payload, nil)
	if errPrepare != nil {
		return chatGPTWebRequirements{}, false
	}
	prepareToken := chatGPTWebFirstString(prepared, "prepare_token", "prepareToken")
	turnstile := chatGPTWebMap(prepared["turnstile"])
	if prepareToken == "" || chatGPTWebBool(turnstile["required"]) {
		return chatGPTWebRequirements{}, false
	}
	pow := chatGPTWebMap(prepared["proofofwork"])
	if len(pow) == 0 {
		pow = chatGPTWebMap(prepared["proof_of_work"])
	}
	proofToken, errPoW := c.solveRequiredPoW(pow)
	if errPoW != nil {
		return chatGPTWebRequirements{}, false
	}
	finalPayload := map[string]any{"prepare_token": prepareToken}
	if proofToken != "" {
		finalPayload["proofofwork"] = proofToken
	}
	finalized, errFinalize := c.postJSON(ctx, "/backend-api/sentinel/chat-requirements/finalize", finalPayload, nil)
	if errFinalize != nil {
		return chatGPTWebRequirements{}, false
	}
	token := chatGPTWebFirstString(finalized, "token", "chat_token", "chat_requirements_token")
	if token == "" {
		return chatGPTWebRequirements{}, false
	}
	return chatGPTWebRequirements{Token: token, ProofToken: proofToken}, true
}

func (c *ChatGPTWebClient) solveRequiredPoW(pow map[string]any) (string, error) {
	if !chatGPTWebBool(pow["required"]) {
		return "", nil
	}
	seed := chatGPTWebString(pow["seed"])
	difficulty := chatGPTWebString(pow["difficulty"])
	if seed == "" || difficulty == "" {
		return "", newChatGPTWebError(http.StatusBadGateway, "pow_invalid", "Sentinel proof-of-work parameters are incomplete", nil)
	}
	return c.solvePoW(seed, difficulty)
}

func (c *ChatGPTWebClient) sentinelHeaders(requirements chatGPTWebRequirements) http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("OpenAI-Sentinel-Chat-Requirements-Token", requirements.Token)
	if requirements.ProofToken != "" {
		headers.Set("OpenAI-Sentinel-Proof-Token", requirements.ProofToken)
	}
	return headers
}

func (c *ChatGPTWebClient) postJSON(ctx context.Context, path string, payload any, headers http.Header) (map[string]any, error) {
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("chatgpt web: marshal request: %w", errMarshal)
	}
	resp, errDo := c.do(ctx, http.MethodPost, path, body, headers, true)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.httpError(path, resp.StatusCode)
	}
	result, errJSON := chatGPTWebReadJSON(resp.Body)
	if errJSON != nil {
		return nil, newChatGPTWebError(http.StatusBadGateway, "upstream_invalid_json", "ChatGPT Web returned invalid JSON", errJSON)
	}
	return result, nil
}

func (c *ChatGPTWebClient) do(ctx context.Context, method, target string, body []byte, extra http.Header, withAuth bool) (*http.Response, error) {
	requestURL, errResolve := c.resolveURL(target)
	if errResolve != nil {
		return nil, errResolve
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, errNew := http.NewRequestWithContext(ctx, method, requestURL.String(), reader)
	if errNew != nil {
		return nil, fmt.Errorf("chatgpt web: create request: %w", errNew)
	}
	for key, values := range c.baseHeaders(requestURL, withAuth) {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	for key, values := range extra {
		req.Header.Del(key)
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
			return nil, errDo
		}
		return nil, newChatGPTWebError(http.StatusBadGateway, "upstream_network_error", "ChatGPT Web network request failed", errDo)
	}
	return resp, nil
}

// DoRequest executes an executor-owned request through the same per-auth transport.
func (c *ChatGPTWebClient) DoRequest(ctx context.Context, request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, newChatGPTWebError(http.StatusBadRequest, "request_invalid", "HTTP request is nil", nil)
	}
	if errToken := c.ensureAccessToken(ctx); errToken != nil {
		return nil, errToken
	}
	clone := request.Clone(ctx)
	if strings.EqualFold(clone.URL.Hostname(), c.baseURL.Hostname()) {
		for key, values := range c.baseHeaders(clone.URL, true) {
			if clone.Header.Get(key) != "" {
				continue
			}
			for _, value := range values {
				clone.Header.Add(key, value)
			}
		}
	}
	return c.httpClient.Do(clone)
}

func (c *ChatGPTWebClient) baseHeaders(requestURL *url.URL, withAuth bool) http.Header {
	headers := make(http.Header)
	headers.Set("User-Agent", c.userAgent)
	headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	headers.Set("Origin", strings.TrimRight(c.baseURL.String(), "/"))
	headers.Set("Referer", strings.TrimRight(c.baseURL.String(), "/")+"/")
	headers.Set("OAI-Device-Id", c.deviceID)
	headers.Set("OAI-Session-Id", c.sessionID)
	headers.Set("OAI-Language", "zh-CN")
	headers.Set("OAI-Client-Version", c.clientVersion)
	headers.Set("OAI-Client-Build-Number", c.clientBuild)
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")
	if requestURL != nil && strings.EqualFold(requestURL.Hostname(), c.baseURL.Hostname()) {
		headers.Set("x-openai-target-path", requestURL.Path)
		headers.Set("x-openai-target-route", requestURL.Path)
	}
	if withAuth {
		if c.accessToken != "" {
			headers.Set("Authorization", "Bearer "+c.accessToken)
		}
		if c.cookie != "" {
			headers.Set("Cookie", c.cookie)
		}
	}
	return headers
}

func (c *ChatGPTWebClient) resolveURL(target string) (*url.URL, error) {
	parsed, errParse := url.Parse(strings.TrimSpace(target))
	if errParse != nil {
		return nil, newChatGPTWebError(http.StatusBadRequest, "upstream_url_invalid", "ChatGPT Web URL is invalid", errParse)
	}
	if !parsed.IsAbs() {
		parsed = c.baseURL.ResolveReference(parsed)
	}
	return parsed, nil
}

func (c *ChatGPTWebClient) httpError(phase string, status int) error {
	code := "upstream_http_error"
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		code = "upstream_auth_error"
	} else if status == http.StatusTooManyRequests {
		code = "upstream_rate_limit"
	}
	return newChatGPTWebError(status, code, fmt.Sprintf("%s returned HTTP %d", phase, status), nil)
}

func chatGPTWebReadJSON(reader io.Reader) (map[string]any, error) {
	data, errRead := io.ReadAll(io.LimitReader(reader, chatGPTWebMaxJSONBytes+1))
	if errRead != nil {
		return nil, errRead
	}
	if len(data) > chatGPTWebMaxJSONBytes {
		return nil, fmt.Errorf("JSON response exceeds %d bytes", chatGPTWebMaxJSONBytes)
	}
	result := make(map[string]any)
	if errJSON := json.Unmarshal(data, &result); errJSON != nil {
		return nil, errJSON
	}
	return result, nil
}

func chatGPTWebMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func chatGPTWebString(value any) string {
	result, _ := value.(string)
	return strings.TrimSpace(result)
}

func chatGPTWebFirstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := chatGPTWebString(values[key]); value != "" {
			return value
		}
	}
	return ""
}

func chatGPTWebBool(value any) bool {
	result, _ := value.(bool)
	return result
}

func chatGPTWebNestedBool(values map[string]any, parent, child string) (bool, bool) {
	nested := chatGPTWebMap(values[parent])
	if nested == nil {
		return false, false
	}
	value, ok := nested[child].(bool)
	return value, ok
}

func (c *ChatGPTWebClient) solvePoW(seed, difficulty string) (string, error) {
	difficulty = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(difficulty), "0x"))
	if difficulty == "" || len(difficulty) > 64*2 || !chatGPTWebHexPattern.MatchString(difficulty) {
		return "", newChatGPTWebError(http.StatusBadGateway, "pow_invalid", "Sentinel proof-of-work difficulty is invalid", nil)
	}
	config := []any{
		chatGPTWebRandomChoice([]int{3000, 4000, 6000}) * chatGPTWebRandomChoice([]int{1, 2, 4}),
		time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"),
		nil,
		0,
		c.userAgent,
		"https://tcr9i.chat.openai.com/v2/35536E1E-65B4-4D96-9D97-6ADB7EFF8147/api.js",
		"dpl=1440a687921de39ff5ee56b92807faaadce73f13",
		"en",
		"en-US",
		nil,
		"plugins−[object PluginArray]",
		chatGPTWebRandomChoice([]string{"_reactListeningcfilawjnerp", "_reactListening9ne2dfo1i47"}),
		chatGPTWebRandomChoice([]string{"alert", "ontransitionend", "onprogress"}),
	}
	for nonce := 0; nonce < c.maxPoWIterations; nonce++ {
		config[3] = nonce
		raw, _ := json.Marshal(config)
		encoded := base64.StdEncoding.EncodeToString(raw)
		digest := sha3.Sum512([]byte(seed + encoded))
		hexDigest := hex.EncodeToString(digest[:])
		if hexDigest[:len(difficulty)] <= difficulty {
			return "gAAAAAB" + encoded, nil
		}
	}
	return "", newChatGPTWebError(http.StatusServiceUnavailable, "pow_exhausted", "Sentinel proof-of-work iteration limit was reached", nil)
}

func (c *ChatGPTWebClient) generatedRequirementsToken() string {
	config := []any{
		chatGPTWebRandomChoice([]int{16, 24, 32}) + chatGPTWebRandomChoice([]int{3000, 4000, 6000}),
		time.Now().UTC().Format("Mon Jan 02 2006 15:04:05 GMT+0000 (UTC)"),
		nil,
		0,
		c.userAgent,
		nil,
		"dpl=1440a687921de39ff5ee56b92807faaadce73f13",
		"en-US",
		"en-US,zh-CN",
		0,
		chatGPTWebRandomChoice([]string{
			"webdriver−false",
			"vendor−Google Inc.",
			"cookieEnabled−true",
			"pdfViewerEnabled−true",
			"hardwareConcurrency−32",
			"language−zh-CN",
			"mimeTypes−[object MimeTypeArray]",
			"userAgentData−[object NavigatorUAData]",
		}),
		"location",
		chatGPTWebRandomChoice([]string{"innerWidth", "innerHeight", "devicePixelRatio", "screen", "chrome", "location", "history", "navigator"}),
		chatGPTWebRandomFloat64(),
		uuid.NewString(),
		"",
		8,
		time.Now().Unix(),
	}
	seed := strconv.FormatFloat(chatGPTWebRandomFloat64(), 'f', -1, 64)
	target := []byte{0x0f, 0xff, 0xff}
	for nonce := 0; nonce < c.maxPoWIterations; nonce++ {
		config[3] = nonce
		config[9] = nonce >> 1
		raw, _ := json.Marshal(config)
		encoded := base64.StdEncoding.EncodeToString(raw)
		digest := sha3.Sum512([]byte(seed + encoded))
		if bytes.Compare(digest[:len(target)], target) <= 0 {
			return "gAAAAAC" + encoded
		}
	}
	encodedSeed, _ := json.Marshal(seed)
	return "gAAAAACgAAAAABwQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D" + base64.StdEncoding.EncodeToString(encodedSeed)
}

func chatGPTWebRandomChoice[T any](values []T) T {
	index := 0
	if len(values) > 1 {
		limit := big.NewInt(int64(len(values)))
		if value, errRandom := cryptorand.Int(cryptorand.Reader, limit); errRandom == nil {
			index = int(value.Int64())
		}
	}
	return values[index]
}

func chatGPTWebRandomFloat64() float64 {
	var raw [8]byte
	if _, errRead := cryptorand.Read(raw[:]); errRead != nil {
		return float64(time.Now().UnixNano()&((1<<53)-1)) / float64(1<<53)
	}
	return float64(binary.BigEndian.Uint64(raw[:])>>11) / float64(1<<53)
}
