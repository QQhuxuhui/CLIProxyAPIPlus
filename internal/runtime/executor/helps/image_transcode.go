// Package helps: image output-format transcoding for the images API.
//
// Reverse upstreams (ChatGPT web / codex) effectively always return PNG and
// ignore the client's output_format. This helper makes the proxy honor
// output_format=jpeg/webp itself by re-encoding the final image bytes.
// Requests that pass through new-api's upscale pipeline are re-encoded there
// instead (the upscaled result replaces whatever bytes we emit), so this path
// only needs to cover responses that leave this service as the final product.
package helps

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"strings"

	"github.com/gen2brain/webp"
	_ "golang.org/x/image/webp"
)

// is16BitImageColorModel reports a 16-bit declared bit depth: decoding doubles
// the bitmap footprint and legitimate upstreams only produce 8-bit output.
func is16BitImageColorModel(m color.Model) bool {
	return m == color.RGBA64Model || m == color.NRGBA64Model || m == color.Gray16Model
}

// imageTranscodeMaxSide bounds decode memory: upstream images never exceed
// 4096 per side; anything larger is anomalous and left untouched.
const imageTranscodeMaxSide = 4096

// NormalizeImageOutputFormat maps a client output_format value to its
// canonical form. Unsupported or absent values map to "".
func NormalizeImageOutputFormat(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "jpg", "jpeg":
		return "jpeg"
	case "webp":
		return "webp"
	case "png":
		return "png"
	}
	return ""
}

// EnsureImageBase64Format re-encodes a base64 image so its bytes match the
// requested format ("jpeg" or "webp"). quality maps from output_compression
// (0-100) and drives lossy encoding for both formats; pass a negative value
// for the official default of 100.
//
// Returns the (possibly re-encoded) base64 payload, the ACTUAL format of the
// returned bytes ("" when undeterminable), and whether the returned bytes are
// in the requested format. Any failure returns the input unchanged with
// ok=false: a paid generation must never be lost to an encoding issue. The
// actual format lets callers label degraded responses truthfully instead of
// echoing the upstream's (possibly wrong) output_format claim.
func EnsureImageBase64Format(b64, format string, quality int) (string, string, bool) {
	format = NormalizeImageOutputFormat(format)
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		if raw, err = base64.RawStdEncoding.DecodeString(b64); err != nil {
			return b64, "", false
		}
	}
	cfg, actual, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return b64, "", false
	}
	// png (or unsupported) requests never transcode; the sniffed format is
	// still reported so callers can label responses truthfully.
	if format != "jpeg" && format != "webp" {
		return b64, actual, actual == format
	}
	// Preserve upstream bytes when no compression was explicitly requested.
	// An explicit compression value is a local contract even when the upstream
	// happened to return the requested container format already.
	if actual == format && quality < 0 {
		return b64, actual, true
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > imageTranscodeMaxSide || cfg.Height > imageTranscodeMaxSide {
		return b64, actual, false
	}
	if is16BitImageColorModel(cfg.ColorModel) {
		return b64, actual, false
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return b64, actual, false
	}
	var buf bytes.Buffer
	switch format {
	case "jpeg":
		q := quality
		if q < 0 {
			q = 100 // official output_compression default
		}
		// jpeg has no alpha: flatten translucent pixels onto white first.
		if err := jpeg.Encode(&buf, flattenImageAlphaToWhite(img), &jpeg.Options{Quality: q}); err != nil {
			return b64, actual, false
		}
	case "webp":
		// Pure Go (wasm-transpiled libwebp, no cgo); lossy quality tracks
		// output_compression. Lossless VP8L was ~7s for 4K and larger than PNG.
		q := quality
		if q < 0 {
			q = 100
		}
		if q == 0 {
			q = 1
		}
		if err := webp.Encode(&buf, img, webp.Options{Quality: q}); err != nil {
			return b64, actual, false
		}
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), format, true
}

func flattenImageAlphaToWhite(img image.Image) image.Image {
	if o, ok := img.(interface{ Opaque() bool }); ok && o.Opaque() {
		return img
	}
	bounds := img.Bounds()
	flat := image.NewRGBA(bounds)
	draw.Draw(flat, bounds, image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(flat, bounds, img, bounds.Min, draw.Over)
	return flat
}
