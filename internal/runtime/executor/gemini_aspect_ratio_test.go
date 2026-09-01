package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

// TestFixGeminiImageAspectRatioAutoStripped verifies that an "auto" aspect ratio
// is silently removed for Gemini image models so the upstream does not reject it
// with a 400 INVALID_ARGUMENT, while any other imageConfig fields are preserved.
func TestFixGeminiImageAspectRatioAutoStripped(t *testing.T) {
	cases := []struct {
		name  string
		model string
		value string
	}{
		{"gemini-3.1 lowercase auto", "gemini-3.1-flash-image-preview", "auto"},
		{"gemini-3.1 uppercase AUTO", "gemini-3.1-flash-image-preview", "AUTO"},
		{"gemini-3.1 padded auto", "gemini-3.1-flash-image-preview", " auto "},
		{"gemini-2.5 auto", "gemini-2.5-flash-image-preview", "auto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(`{"contents":[{"role":"user","parts":[{"text":"sea"}]}],"generationConfig":{"responseModalities":["IMAGE"],"imageConfig":{"aspectRatio":"` + tc.value + `","imageSize":"1K"}}}`)
			out := fixGeminiImageAspectRatio(tc.model, in)
			if gjson.GetBytes(out, "generationConfig.imageConfig.aspectRatio").Exists() {
				t.Fatalf("aspectRatio should be removed, got: %s", out)
			}
			if got := gjson.GetBytes(out, "generationConfig.imageConfig.imageSize").String(); got != "1K" {
				t.Fatalf("imageSize should be preserved, want 1K got %q; payload=%s", got, out)
			}
		})
	}
}

// TestFixGeminiImageAspectRatioAutoOnlyField verifies that when aspectRatio is the
// only key inside imageConfig, the now-empty imageConfig object is dropped as well.
func TestFixGeminiImageAspectRatioAutoOnlyField(t *testing.T) {
	in := []byte(`{"contents":[{"role":"user","parts":[{"text":"sea"}]}],"generationConfig":{"responseModalities":["IMAGE"],"imageConfig":{"aspectRatio":"auto"}}}`)
	out := fixGeminiImageAspectRatio("gemini-3.1-flash-image-preview", in)
	if gjson.GetBytes(out, "generationConfig.imageConfig").Exists() {
		t.Fatalf("empty imageConfig should be removed, got: %s", out)
	}
}

// TestFixGeminiImageAspectRatioValidUntouched verifies that a valid, non-auto ratio
// is left untouched for a plain (non-2.5) image model so real ratios still reach upstream.
func TestFixGeminiImageAspectRatioValidUntouched(t *testing.T) {
	in := []byte(`{"contents":[{"role":"user","parts":[{"text":"sea"}]}],"generationConfig":{"responseModalities":["IMAGE"],"imageConfig":{"aspectRatio":"16:9","imageSize":"1K"}}}`)
	out := fixGeminiImageAspectRatio("gemini-3.1-flash-image-preview", in)
	if got := gjson.GetBytes(out, "generationConfig.imageConfig.aspectRatio").String(); got != "16:9" {
		t.Fatalf("valid aspectRatio must be preserved, want 16:9 got %q; payload=%s", got, out)
	}
}

// TestStripAutoImageAspectRatioAntigravityPath verifies the shared helper strips
// "auto" for the antigravity-wrapped payload shape (request.generationConfig.imageConfig),
// which is the path image models actually take through cloudcode-pa, while keeping
// imageSize and leaving a valid ratio alone.
func TestStripAutoImageAspectRatioAntigravityPath(t *testing.T) {
	const base = "request.generationConfig.imageConfig"

	auto := []byte(`{"model":"gemini-3.1-flash-image-preview","request":{"contents":[{"role":"user","parts":[{"text":"sea"}]}],"generationConfig":{"responseModalities":["IMAGE"],"imageConfig":{"aspectRatio":"auto","imageSize":"1K"}}}}`)
	out := stripAutoImageAspectRatio(auto, base)
	if gjson.GetBytes(out, base+".aspectRatio").Exists() {
		t.Fatalf("auto aspectRatio should be stripped on antigravity path, got: %s", out)
	}
	if got := gjson.GetBytes(out, base+".imageSize").String(); got != "1K" {
		t.Fatalf("imageSize should survive, want 1K got %q; payload=%s", got, out)
	}

	valid := []byte(`{"request":{"generationConfig":{"imageConfig":{"aspectRatio":"16:9"}}}}`)
	out = stripAutoImageAspectRatio(valid, base)
	if got := gjson.GetBytes(out, base+".aspectRatio").String(); got != "16:9" {
		t.Fatalf("valid ratio must be preserved on antigravity path, want 16:9 got %q", got)
	}
}
