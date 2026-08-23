package openai

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"strings"
	"testing"

	"github.com/gen2brain/webp"
)

// maskPNG builds a width x height mask that is fully opaque except for the
// given editable rectangle, which is fully transparent (the OpenAI contract).
func maskPNG(t *testing.T, width, height int, editable image.Rectangle) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if editable.Min.X <= x && x < editable.Max.X && editable.Min.Y <= y && y < editable.Max.Y {
				img.SetNRGBA(x, y, color.NRGBA{})
			} else {
				img.SetNRGBA(x, y, color.NRGBA{A: 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode mask: %v", err)
	}
	return buf.Bytes()
}

func TestWebImageMaskRegionDirective(t *testing.T) {
	// Top-right quadrant editable, matching the live verification shape.
	directive, err := webImageMaskRegionDirective(maskPNG(t, 200, 200, image.Rect(100, 0, 200, 100)))
	if err != nil {
		t.Fatalf("directive: %v", err)
	}
	for _, fragment := range []string{
		"from 50% to 100% of the image width",
		"from 0% to 50% of the image height",
		"top-right area",
		"keep everything outside it unchanged",
	} {
		if !strings.Contains(directive, fragment) {
			t.Fatalf("directive %q missing %q", directive, fragment)
		}
	}

	// A mask freeing the whole canvas imposes no constraint.
	directive, err = webImageMaskRegionDirective(maskPNG(t, 64, 64, image.Rect(0, 0, 64, 64)))
	if err != nil || directive != "" {
		t.Fatalf("full-canvas mask should yield no directive, got %q, %v", directive, err)
	}

	// A fully opaque mask has no editable region.
	if _, err = webImageMaskRegionDirective(maskPNG(t, 64, 64, image.Rect(0, 0, 0, 0))); err == nil {
		t.Fatal("fully opaque mask must be rejected")
	}

	// Garbage bytes are rejected.
	if _, err = webImageMaskRegionDirective([]byte("not an image")); err == nil {
		t.Fatal("non-image mask must be rejected")
	}

	// Oversized dimensions are rejected before the full decode (bomb guard).
	if _, err = webImageMaskRegionDirective(maskPNG(t, webImageMaskMaxSide+1, 8, image.Rect(0, 0, 4, 4))); err == nil {
		t.Fatal("mask wider than the per-side limit must be rejected")
	}

	// A narrow span must not collapse to a zero-width percent range.
	directive, err = webImageMaskRegionDirective(maskPNG(t, 200, 200, image.Rect(100, 100, 101, 101)))
	if err != nil {
		t.Fatalf("narrow mask: %v", err)
	}
	if !strings.Contains(directive, "from 50% to 51% of the image width") ||
		!strings.Contains(directive, "from 50% to 51% of the image height") {
		t.Fatalf("narrow mask directive %q must span at least 1%%", directive)
	}
}

func TestWebImageMaskRegionDirectiveKeepsSparseFullCanvasMask(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img.SetNRGBA(x, y, color.NRGBA{A: 255})
		}
	}
	img.SetNRGBA(0, 0, color.NRGBA{})
	img.SetNRGBA(99, 99, color.NRGBA{})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode sparse mask: %v", err)
	}

	directive, err := webImageMaskRegionDirective(buf.Bytes())
	if err != nil {
		t.Fatalf("sparse mask: %v", err)
	}
	if directive == "" || !strings.Contains(directive, "sparse") ||
		!strings.Contains(directive, "top-left area") || !strings.Contains(directive, "bottom-right area") ||
		strings.Contains(directive, "mask pixels") {
		t.Fatalf("sparse full-canvas mask directive = %q", directive)
	}
}

func TestWebImageMaskRegionDirectiveKeepsProtectedAreaInNearFullMask(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img.SetNRGBA(x, y, color.NRGBA{})
		}
	}
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			img.SetNRGBA(x, y, color.NRGBA{A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode near-full mask: %v", err)
	}

	directive, err := webImageMaskRegionDirective(buf.Bytes())
	if err != nil {
		t.Fatalf("near-full mask: %v", err)
	}
	if directive == "" || !strings.Contains(directive, "except") || !strings.Contains(directive, "top-left area") {
		t.Fatalf("near-full mask directive = %q", directive)
	}
}

func TestWebImageMaskRegionDirectiveDoesNotWidenDisconnectedMask(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img.SetNRGBA(x, y, color.NRGBA{A: 255})
		}
	}
	img.SetNRGBA(20, 20, color.NRGBA{})
	img.SetNRGBA(80, 80, color.NRGBA{})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode disconnected mask: %v", err)
	}

	directive, err := webImageMaskRegionDirective(buf.Bytes())
	if err != nil {
		t.Fatalf("disconnected mask: %v", err)
	}
	if !strings.Contains(directive, "top-left area") || !strings.Contains(directive, "bottom-right area") || strings.Contains(directive, "from 20% to 81%") {
		t.Fatalf("disconnected mask directive = %q", directive)
	}
}

func TestWebImageMaskRegionDirectiveDoesNotFillProtectedHole(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			alpha := uint8(255)
			if 20 <= x && x < 80 && 20 <= y && y < 80 {
				alpha = 0
			}
			img.SetNRGBA(x, y, color.NRGBA{A: alpha})
		}
	}
	img.SetNRGBA(50, 50, color.NRGBA{A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode mask with hole: %v", err)
	}

	directive, err := webImageMaskRegionDirective(buf.Bytes())
	if err != nil {
		t.Fatalf("mask with hole: %v", err)
	}
	if strings.Contains(directive, "rectangular area") || !strings.Contains(directive, "protected pixels in the central area") {
		t.Fatalf("mask with protected hole was not preserved: %q", directive)
	}
}

func TestWebImageMaskRegionDirectiveRejectsOversizedBytes(t *testing.T) {
	if _, err := webImageMaskRegionDirective(make([]byte, webImageMaskMaxBytes+1)); err == nil || !strings.Contains(err.Error(), "mask exceeds") {
		t.Fatalf("oversized mask error = %v", err)
	}
}

func TestWebImageMaskMatchesReferenceDimensions(t *testing.T) {
	mask := maskPNG(t, 100, 80, image.Rect(10, 10, 30, 30))
	reference := maskPNG(t, 100, 80, image.Rect(0, 0, 100, 80))
	referenceURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(reference)
	if err := webImageMaskMatchesReferenceDimensions(mask, referenceURL, len(reference)); err != nil {
		t.Fatalf("matching dimensions rejected: %v", err)
	}
	mismatched := maskPNG(t, 80, 100, image.Rect(10, 10, 30, 30))
	if err := webImageMaskMatchesReferenceDimensions(mismatched, referenceURL, len(reference)); err == nil {
		t.Fatal("mismatched mask dimensions must be rejected")
	}
	if err := webImageMaskMatchesReferenceDimensions(mask, referenceURL, 1); err == nil {
		t.Fatal("reference byte limit must be enforced independently from the mask limit")
	}
}

func TestWebImageMaskMatchesReferenceAlternativeFormats(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var gifBuffer bytes.Buffer
	if err := gif.Encode(&gifBuffer, img, nil); err != nil {
		t.Fatalf("gif.Encode: %v", err)
	}
	var webpBuffer bytes.Buffer
	if err := webp.Encode(&webpBuffer, img, webp.Options{Quality: 80}); err != nil {
		t.Fatalf("webp.Encode: %v", err)
	}
	mask := maskPNG(t, 2, 2, image.Rect(0, 0, 1, 1))
	for name, data := range map[string][]byte{"gif": gifBuffer.Bytes(), "webp": webpBuffer.Bytes()} {
		referenceURL := "data:image/" + name + ";base64," + base64.StdEncoding.EncodeToString(data)
		if err := webImageMaskMatchesReferenceDimensions(mask, referenceURL, len(data)); err != nil {
			t.Fatalf("%s reference rejected: %v", name, err)
		}
	}
}

func TestWebImageMaskLocationPhrase(t *testing.T) {
	cases := []struct {
		x1, x2, y1, y2 int
		want           string
	}{
		{50, 100, 0, 50, "top-right area"},
		{0, 50, 50, 100, "bottom-left area"},
		{33, 67, 33, 67, "central area"},
		{0, 30, 40, 60, "middle-left area"},
		{35, 65, 0, 20, "top-center area"},
	}
	for _, tc := range cases {
		if got := webImageMaskLocationPhrase(tc.x1, tc.x2, tc.y1, tc.y2); got != tc.want {
			t.Fatalf("phrase(%d,%d,%d,%d) = %q, want %q", tc.x1, tc.x2, tc.y1, tc.y2, got, tc.want)
		}
	}
}

func TestWebImageMaskBytesFromJSON(t *testing.T) {
	maskBytes := maskPNG(t, 32, 32, image.Rect(0, 0, 16, 16))
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(maskBytes)

	// String form.
	raw := []byte(`{"prompt":"p","mask":"` + dataURL + `"}`)
	got, err := webImageMaskBytesFromJSON(raw)
	if err != nil || !bytes.Equal(got, maskBytes) {
		t.Fatalf("string mask: %v, equal=%v", err, bytes.Equal(got, maskBytes))
	}

	// Object image_url form (codex-compatible).
	raw = []byte(`{"prompt":"p","mask":{"image_url":"` + dataURL + `"}}`)
	got, err = webImageMaskBytesFromJSON(raw)
	if err != nil || !bytes.Equal(got, maskBytes) {
		t.Fatalf("object mask: %v, equal=%v", err, bytes.Equal(got, maskBytes))
	}

	// Unpadded base64 (real clients send it) must decode too.
	unpadded := "data:image/png;base64," + base64.RawStdEncoding.EncodeToString(maskBytes)
	raw = []byte(`{"prompt":"p","mask":"` + unpadded + `"}`)
	got, err = webImageMaskBytesFromJSON(raw)
	if err != nil || !bytes.Equal(got, maskBytes) {
		t.Fatalf("unpadded mask: %v, equal=%v", err, bytes.Equal(got, maskBytes))
	}

	// Rejections: file_id, remote URL, empty.
	for name, payload := range map[string]string{
		"file_id":    `{"mask":{"file_id":"file-123"}}`,
		"remote URL": `{"mask":"https://example.com/mask.png"}`,
		"empty":      `{"mask":""}`,
	} {
		if _, err = webImageMaskBytesFromJSON([]byte(payload)); err == nil {
			t.Fatalf("%s mask must be rejected", name)
		}
	}
}

func TestWebImageMaskBytesFromJSONRejectsOversizedMask(t *testing.T) {
	encoded := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{0}, webImageMaskMaxBytes+1))
	raw := []byte(`{"mask":"data:image/png;base64,` + encoded + `"}`)
	if _, err := webImageMaskBytesFromJSON(raw); err == nil || !strings.Contains(err.Error(), "mask exceeds") {
		t.Fatalf("oversized JSON mask error = %v", err)
	}
}
