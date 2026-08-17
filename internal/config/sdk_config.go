// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import (
	"strings"
	"time"
)

const (
	DefaultWebImageModel                          = "gpt-image-web"
	DefaultWebImagePollTimeout                    = "120s"
	DefaultWebImageTotalDeadline                  = "150s"
	DefaultWebImagePollInterval                   = "1s"
	DefaultWebImagePoWTimeout                     = "20s"
	DefaultWebImageMaxBytes                 int64 = 20 * 1024 * 1024
	WebImageMaxInputImages                        = 16
	DefaultWebImageMaxConcurrency                 = 4
	DefaultWebImageMaxConcurrencyPerAccount       = 1
	DefaultWebImageUserAgent                      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	DefaultWebImageClientVersion                  = "prod-be885abbfcfe7b1f511e88b3003d9ee44757fbad"
	DefaultWebImageClientBuild                    = "5955942"
)

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// DisableImageGeneration controls whether the built-in image_generation tool is injected/allowed.
	//
	// Supported values:
	//   - false (default): image_generation is enabled everywhere (normal behavior).
	//   - true: image_generation is disabled everywhere. The server stops injecting it, removes it from request payloads,
	//     and returns 404 for /v1/images/generations and /v1/images/edits.
	//   - "chat": disable image_generation injection for all non-images endpoints (e.g. /v1/responses, /v1/chat/completions),
	//     while keeping /v1/images/generations and /v1/images/edits enabled and preserving image_generation there.
	//   - "passthrough": do not modify the tool list on non-images endpoints — keep image_generation if the client
	//     sent it and do not inject it otherwise; on /v1/images/generations and /v1/images/edits behave like "chat".
	DisableImageGeneration DisableImageGenerationMode `yaml:"disable-image-generation" json:"disable-image-generation"`

	// GPTImage2BaseModel sets the base (mainline) model used by the legacy hosted
	// image_generation tool path when a Codex image request is not proxied directly
	// through the Image API.
	//
	// The value must start with "gpt-" (case-insensitive). If empty or invalid, the
	// default base model ("gpt-5.4-mini") is used.
	GPTImage2BaseModel string `yaml:"gpt-image-2-base-model,omitempty" json:"gpt-image-2-base-model,omitempty"`

	// VideoResultAuthCacheTTL controls how long video IDs stay pinned to the credential
	// that created them. Accepts duration strings like "30m" or "3h".
	// Empty or invalid values use the default 3h.
	VideoResultAuthCacheTTL string `yaml:"video-result-auth-cache-ttl,omitempty" json:"video-result-auth-cache-ttl,omitempty"`

	WebImageConfig `yaml:",inline"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`
}

// WebImageConfig controls the opt-in ChatGPT web image generation route.
type WebImageConfig struct {
	WebImageGeneration               bool     `yaml:"web-image-generation" json:"web-image-generation"`
	WebImageFreeOnly                 bool     `yaml:"web-image-free-only" json:"web-image-free-only"`
	WebImageModels                   []string `yaml:"web-image-models,omitempty" json:"web-image-models,omitempty"`
	WebImageBaseModel                string   `yaml:"web-image-base-model,omitempty" json:"web-image-base-model,omitempty"`
	WebImagePollTimeout              string   `yaml:"web-image-poll-timeout,omitempty" json:"web-image-poll-timeout,omitempty"`
	WebImageTotalDeadline            string   `yaml:"web-image-total-deadline,omitempty" json:"web-image-total-deadline,omitempty"`
	WebImagePollInterval             string   `yaml:"web-image-poll-interval,omitempty" json:"web-image-poll-interval,omitempty"`
	WebImagePoWTimeout               string   `yaml:"web-image-pow-timeout,omitempty" json:"web-image-pow-timeout,omitempty"`
	WebImageMaxBytes                 int64    `yaml:"web-image-max-bytes,omitempty" json:"web-image-max-bytes,omitempty"`
	WebImageMaxConcurrency           int      `yaml:"web-image-max-concurrency,omitempty" json:"web-image-max-concurrency,omitempty"`
	WebImageMaxConcurrencyPerAccount int      `yaml:"web-image-max-concurrency-per-account,omitempty" json:"web-image-max-concurrency-per-account,omitempty"`
	WebImageUserAgent                string   `yaml:"web-image-user-agent,omitempty" json:"web-image-user-agent,omitempty"`
	WebImageClientVersion            string   `yaml:"web-image-client-version,omitempty" json:"web-image-client-version,omitempty"`
	WebImageClientBuild              string   `yaml:"web-image-client-build,omitempty" json:"web-image-client-build,omitempty"`
}

// SetWebImageDefaults applies defaults before unmarshalling so explicit false
// values remain distinguishable from an absent web-image-free-only key.
func (c *SDKConfig) SetWebImageDefaults() {
	if c == nil {
		return
	}
	c.WebImageFreeOnly = true
	c.WebImageModels = []string{DefaultWebImageModel}
	c.WebImagePollTimeout = DefaultWebImagePollTimeout
	c.WebImageTotalDeadline = DefaultWebImageTotalDeadline
	c.WebImagePollInterval = DefaultWebImagePollInterval
	c.WebImagePoWTimeout = DefaultWebImagePoWTimeout
	c.WebImageMaxBytes = DefaultWebImageMaxBytes
	c.WebImageMaxConcurrency = DefaultWebImageMaxConcurrency
	c.WebImageMaxConcurrencyPerAccount = DefaultWebImageMaxConcurrencyPerAccount
	c.WebImageUserAgent = DefaultWebImageUserAgent
	c.WebImageClientVersion = DefaultWebImageClientVersion
	c.WebImageClientBuild = DefaultWebImageClientBuild
}

// NormalizeWebImageConfig removes duplicate aliases and repairs invalid limits.
func (c *SDKConfig) NormalizeWebImageConfig() {
	if c == nil {
		return
	}

	models := make([]string, 0, len(c.WebImageModels))
	seen := make(map[string]struct{}, len(c.WebImageModels))
	for _, model := range c.WebImageModels {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, model)
	}
	if len(models) == 0 {
		models = []string{DefaultWebImageModel}
	}
	c.WebImageModels = models
	c.WebImageBaseModel = strings.TrimSpace(c.WebImageBaseModel)
	c.WebImagePollTimeout = positiveDurationOrDefault(c.WebImagePollTimeout, DefaultWebImagePollTimeout)
	c.WebImageTotalDeadline = positiveDurationOrDefault(c.WebImageTotalDeadline, DefaultWebImageTotalDeadline)
	c.WebImagePollInterval = positiveDurationOrDefault(c.WebImagePollInterval, DefaultWebImagePollInterval)
	c.WebImagePoWTimeout = positiveDurationOrDefault(c.WebImagePoWTimeout, DefaultWebImagePoWTimeout)
	if c.WebImageMaxBytes <= 0 {
		c.WebImageMaxBytes = DefaultWebImageMaxBytes
	}
	if c.WebImageMaxConcurrency <= 0 {
		c.WebImageMaxConcurrency = DefaultWebImageMaxConcurrency
	}
	if c.WebImageMaxConcurrencyPerAccount <= 0 {
		c.WebImageMaxConcurrencyPerAccount = DefaultWebImageMaxConcurrencyPerAccount
	}
	c.WebImageUserAgent = strings.TrimSpace(c.WebImageUserAgent)
	if c.WebImageUserAgent == "" {
		c.WebImageUserAgent = DefaultWebImageUserAgent
	}
	c.WebImageClientVersion = strings.TrimSpace(c.WebImageClientVersion)
	c.WebImageClientBuild = strings.TrimSpace(c.WebImageClientBuild)
}

func positiveDurationOrDefault(value, fallback string) string {
	value = strings.TrimSpace(value)
	duration, errParse := time.ParseDuration(value)
	if errParse != nil || duration <= 0 {
		return fallback
	}
	return value
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n").
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`
}
