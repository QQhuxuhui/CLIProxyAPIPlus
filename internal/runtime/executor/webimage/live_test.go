package webimage

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestLiveWebImageGeneration(t *testing.T) {
	accessToken := strings.TrimSpace(os.Getenv("CPA_WEBIMAGE_LIVE_TOKEN"))
	baseModel := strings.TrimSpace(os.Getenv("CPA_WEBIMAGE_LIVE_BASE_MODEL"))
	if accessToken == "" || baseModel == "" {
		t.Skip("set CPA_WEBIMAGE_LIVE_TOKEN and CPA_WEBIMAGE_LIVE_BASE_MODEL to run the live smoke test")
	}

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	cfg.WebImageBaseModel = baseModel
	if value := strings.TrimSpace(os.Getenv("CPA_WEBIMAGE_LIVE_CLIENT_VERSION")); value != "" {
		cfg.WebImageClientVersion = value
	}
	executor := NewExecutor(cfg)
	defer executor.Close()

	prompt := strings.TrimSpace(os.Getenv("CPA_WEBIMAGE_LIVE_PROMPT"))
	if prompt == "" {
		prompt = "A small green triangle centered on a plain white background, flat studio lighting, square image."
	}
	results, _, errGenerate := executor.Generate(context.Background(), Credentials{
		AccessToken: accessToken,
		AuthID:      "web-image-live-smoke",
	}, prompt)
	if errGenerate != nil {
		t.Fatalf("live web image generation failed: %v", errGenerate)
	}
	if len(results) != 1 || strings.TrimSpace(results[0].Base64Data) == "" {
		t.Fatalf("live web image generation returned %d images", len(results))
	}

	imageBytes, errDecode := base64.StdEncoding.DecodeString(results[0].Base64Data)
	if errDecode != nil || len(imageBytes) == 0 {
		t.Fatalf("live web image payload decode failed: %v", errDecode)
	}
	if outputPath := strings.TrimSpace(os.Getenv("CPA_WEBIMAGE_LIVE_OUTPUT")); outputPath != "" {
		if errWrite := os.WriteFile(outputPath, imageBytes, 0o600); errWrite != nil {
			t.Fatalf("write live web image output: %v", errWrite)
		}
	}
}
