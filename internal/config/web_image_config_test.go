package config

import (
	"reflect"
	"testing"
)

func TestParseConfigBytesWebImageDefaults(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte("host: ''\n"))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}

	if cfg.WebImageGeneration {
		t.Fatal("WebImageGeneration = true, want false")
	}
	if !cfg.WebImageFreeOnly {
		t.Fatal("WebImageFreeOnly = false, want true")
	}
	if want := []string{DefaultWebImageModel}; !reflect.DeepEqual(cfg.WebImageModels, want) {
		t.Fatalf("WebImageModels = %#v, want %#v", cfg.WebImageModels, want)
	}
	if cfg.WebImagePollTimeout != DefaultWebImagePollTimeout {
		t.Fatalf("WebImagePollTimeout = %q, want %q", cfg.WebImagePollTimeout, DefaultWebImagePollTimeout)
	}
	if cfg.WebImageTotalDeadline != DefaultWebImageTotalDeadline {
		t.Fatalf("WebImageTotalDeadline = %q, want %q", cfg.WebImageTotalDeadline, DefaultWebImageTotalDeadline)
	}
	if cfg.WebImagePollInterval != DefaultWebImagePollInterval {
		t.Fatalf("WebImagePollInterval = %q, want %q", cfg.WebImagePollInterval, DefaultWebImagePollInterval)
	}
	if cfg.WebImagePoWTimeout != DefaultWebImagePoWTimeout {
		t.Fatalf("WebImagePoWTimeout = %q, want %q", cfg.WebImagePoWTimeout, DefaultWebImagePoWTimeout)
	}
	if cfg.WebImageMaxBytes != DefaultWebImageMaxBytes {
		t.Fatalf("WebImageMaxBytes = %d, want %d", cfg.WebImageMaxBytes, DefaultWebImageMaxBytes)
	}
	if cfg.WebImageMaxConcurrency != DefaultWebImageMaxConcurrency {
		t.Fatalf("WebImageMaxConcurrency = %d, want %d", cfg.WebImageMaxConcurrency, DefaultWebImageMaxConcurrency)
	}
	if cfg.WebImageMaxConcurrencyPerAccount != DefaultWebImageMaxConcurrencyPerAccount {
		t.Fatalf("WebImageMaxConcurrencyPerAccount = %d, want %d", cfg.WebImageMaxConcurrencyPerAccount, DefaultWebImageMaxConcurrencyPerAccount)
	}
	if cfg.WebImageUserAgent != DefaultWebImageUserAgent {
		t.Fatalf("WebImageUserAgent = %q, want %q", cfg.WebImageUserAgent, DefaultWebImageUserAgent)
	}
	if cfg.WebImageClientVersion != DefaultWebImageClientVersion {
		t.Fatalf("WebImageClientVersion = %q, want %q", cfg.WebImageClientVersion, DefaultWebImageClientVersion)
	}
}

func TestParseConfigBytesWebImageOverrides(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`
web-image-generation: true
web-image-free-only: false
web-image-models: [gpt-image-web-test]
web-image-base-model: internal-image-model
web-image-poll-timeout: 90s
web-image-total-deadline: 2m
web-image-poll-interval: 2s
web-image-pow-timeout: 7s
web-image-max-bytes: 12345
web-image-max-concurrency: 9
web-image-max-concurrency-per-account: 2
web-image-user-agent: test-agent
web-image-client-version: test-version
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}

	if !cfg.WebImageGeneration {
		t.Fatal("WebImageGeneration = false, want true")
	}
	if cfg.WebImageFreeOnly {
		t.Fatal("WebImageFreeOnly = true, want false")
	}
	if want := []string{"gpt-image-web-test"}; !reflect.DeepEqual(cfg.WebImageModels, want) {
		t.Fatalf("WebImageModels = %#v, want %#v", cfg.WebImageModels, want)
	}
	if cfg.WebImageBaseModel != "internal-image-model" {
		t.Fatalf("WebImageBaseModel = %q", cfg.WebImageBaseModel)
	}
	if cfg.WebImagePollTimeout != "90s" || cfg.WebImageTotalDeadline != "2m" || cfg.WebImagePollInterval != "2s" || cfg.WebImagePoWTimeout != "7s" {
		t.Fatalf("duration overrides were not preserved: %+v", cfg.WebImageConfig)
	}
	if cfg.WebImageMaxBytes != 12345 || cfg.WebImageMaxConcurrency != 9 || cfg.WebImageMaxConcurrencyPerAccount != 2 {
		t.Fatalf("limit overrides were not preserved: %+v", cfg.WebImageConfig)
	}
	if cfg.WebImageUserAgent != "test-agent" || cfg.WebImageClientVersion != "test-version" {
		t.Fatalf("client overrides were not preserved: %+v", cfg.WebImageConfig)
	}
}

func TestNormalizeWebImageConfigRepairsInvalidValues(t *testing.T) {
	cfg := &Config{}
	cfg.WebImageModels = []string{" ", "gpt-image-web", "gpt-image-web", " custom "}
	cfg.WebImagePollTimeout = "invalid"
	cfg.WebImageTotalDeadline = "0s"
	cfg.WebImagePollInterval = "-1s"
	cfg.WebImagePoWTimeout = "invalid"
	cfg.WebImageMaxBytes = -1
	cfg.WebImageMaxConcurrency = -1
	cfg.WebImageMaxConcurrencyPerAccount = 0

	cfg.NormalizeWebImageConfig()

	if want := []string{"gpt-image-web", "custom"}; !reflect.DeepEqual(cfg.WebImageModels, want) {
		t.Fatalf("WebImageModels = %#v, want %#v", cfg.WebImageModels, want)
	}
	if cfg.WebImagePollTimeout != DefaultWebImagePollTimeout || cfg.WebImageTotalDeadline != DefaultWebImageTotalDeadline || cfg.WebImagePollInterval != DefaultWebImagePollInterval || cfg.WebImagePoWTimeout != DefaultWebImagePoWTimeout {
		t.Fatalf("invalid durations were not repaired: %+v", cfg.WebImageConfig)
	}
	if cfg.WebImageMaxBytes != DefaultWebImageMaxBytes || cfg.WebImageMaxConcurrency != DefaultWebImageMaxConcurrency || cfg.WebImageMaxConcurrencyPerAccount != DefaultWebImageMaxConcurrencyPerAccount {
		t.Fatalf("invalid limits were not repaired: %+v", cfg.WebImageConfig)
	}
}
