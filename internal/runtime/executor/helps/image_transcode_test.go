package helps

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func testPNGB64(t *testing.T, w, h int, alpha bool) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	a := uint8(255)
	if alpha {
		a = 128
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 40, A: a})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func sniffB64(t *testing.T, b64 string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	_, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return format
}

func TestNormalizeImageOutputFormat(t *testing.T) {
	cases := map[string]string{
		"jpeg": "jpeg", "JPG": "jpeg", " jpg ": "jpeg", "webp": "webp",
		"png": "png", "gif": "", "": "", "JPEG": "jpeg",
	}
	for in, want := range cases {
		if got := NormalizeImageOutputFormat(in); got != want {
			t.Errorf("NormalizeImageOutputFormat(%q)=%q want %q", in, got, want)
		}
	}
}

func TestEnsureImageBase64FormatTranscodesJPEG(t *testing.T) {
	b64 := testPNGB64(t, 16, 16, false)
	out, actual, ok := EnsureImageBase64Format(b64, "jpeg", 85)
	if !ok || actual != "jpeg" {
		t.Fatalf("expected ok with actual=jpeg, got actual=%q ok=%v", actual, ok)
	}
	if f := sniffB64(t, out); f != "jpeg" {
		t.Fatalf("format=%q want jpeg", f)
	}
}

func TestEnsureImageBase64FormatTranscodesWebP(t *testing.T) {
	b64 := testPNGB64(t, 16, 16, false)
	out, actual, ok := EnsureImageBase64Format(b64, "webp", -1)
	if !ok || actual != "webp" {
		t.Fatalf("expected ok with actual=webp, got actual=%q ok=%v", actual, ok)
	}
	if f := sniffB64(t, out); f != "webp" {
		t.Fatalf("format=%q want webp", f)
	}
}

func TestEnsureImageBase64FormatWebPQualityAffectsSize(t *testing.T) {
	b64 := testPNGB64(t, 96, 96, false)
	low, _, ok := EnsureImageBase64Format(b64, "webp", 10)
	if !ok {
		t.Fatal("expected ok")
	}
	high, _, ok := EnsureImageBase64Format(b64, "webp", 100)
	if !ok {
		t.Fatal("expected ok")
	}
	if len(low) >= len(high) {
		t.Fatalf("webp quality 10 (%d) should be smaller than quality 100 (%d)", len(low), len(high))
	}
}

func TestEnsureImageBase64FormatAlreadyMatching(t *testing.T) {
	b64 := testPNGB64(t, 16, 16, false)
	jpegB64, _, ok := EnsureImageBase64Format(b64, "jpeg", 85)
	if !ok {
		t.Fatal("setup transcode failed")
	}
	again, actual, ok := EnsureImageBase64Format(jpegB64, "jpeg", 85)
	if !ok || actual != "jpeg" || again != jpegB64 {
		t.Fatal("already-matching bytes must be returned untouched with ok=true")
	}
}

func TestEnsureImageBase64FormatReencodesMatchingFormatWhenCompressionSpecified(t *testing.T) {
	pngB64 := testPNGB64(t, 256, 256, false)
	jpegB64, _, ok := EnsureImageBase64Format(pngB64, "jpeg", 100)
	if !ok {
		t.Fatal("setup transcode failed")
	}
	compressed, actual, ok := EnsureImageBase64Format(jpegB64, "jpeg", 10)
	if !ok || actual != "jpeg" {
		t.Fatalf("expected explicit same-format compression, actual=%q ok=%v", actual, ok)
	}
	if compressed == jpegB64 {
		t.Fatal("explicit compression must re-encode already matching bytes")
	}
}

func TestEnsureImageBase64FormatWebPQualityZeroUsesMinimumQuality(t *testing.T) {
	pngB64 := testPNGB64(t, 96, 96, false)
	zero, _, ok := EnsureImageBase64Format(pngB64, "webp", 0)
	if !ok {
		t.Fatal("quality zero transcode failed")
	}
	minimum, _, ok := EnsureImageBase64Format(pngB64, "webp", 1)
	if !ok {
		t.Fatal("quality one transcode failed")
	}
	if zero != minimum {
		t.Fatal("WebP quality zero should use the encoder minimum, not its default quality")
	}
}

func TestEnsureImageBase64FormatJPEGFlattensAlpha(t *testing.T) {
	b64 := testPNGB64(t, 16, 16, true)
	out, _, ok := EnsureImageBase64Format(b64, "jpeg", 90)
	if !ok {
		t.Fatal("expected ok")
	}
	if f := sniffB64(t, out); f != "jpeg" {
		t.Fatalf("format=%q want jpeg", f)
	}
}

func TestEnsureImageBase64FormatFailOpen(t *testing.T) {
	// Garbage input / unsupported format must return the input unchanged.
	if out, actual, ok := EnsureImageBase64Format("!!notbase64!!", "jpeg", -1); ok || actual != "" || out != "!!notbase64!!" {
		t.Fatal("garbage base64 must fail open")
	}
	b64 := testPNGB64(t, 16, 16, false)
	if out, _, ok := EnsureImageBase64Format(b64, "gif", -1); ok || out != b64 {
		t.Fatal("unsupported format must fail open")
	}
	// png request: no transcode performed; the sniffed format is reported and
	// ok reflects whether the bytes already match.
	if out, actual, ok := EnsureImageBase64Format(b64, "png", -1); !ok || actual != "png" || out != b64 {
		t.Fatal("png request on png bytes must report actual=png ok=true without transcoding")
	}
}
