package openai

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"strings"

	"github.com/tidwall/gjson"
	_ "golang.org/x/image/webp"
)

// webImageMaskMaxBytes caps how much of an uploaded mask file is read before
// decoding. Masks are simple alpha stencils; anything larger is rejected.
const webImageMaskMaxBytes = 32 << 20

// webImageMaskMaxSide caps the mask's decoded pixel dimensions. The byte cap
// alone does not bound decoded size for PNG (a small compressed file can
// declare enormous dimensions), so the header is checked before the full
// decode. A 4096 px side limit keeps the decoded image and pixel scan bounded.
const webImageMaskMaxSide = 4096

// webImageMaskRegionDirective converts an OpenAI-style edit mask into a
// natural-language region directive for the ChatGPT web image pipeline.
//
// The web pipeline cannot forward a mask upstream - its only control surface
// is the prompt (the same mechanism used for the requested size). Per the
// OpenAI images/edits contract the mask's transparent pixels (alpha < 50%)
// mark the editable region; this helper reduces solid rectangles to percent
// coordinates and describes sparse regions on a coarse location grid.
//
// A fully transparent mask returns an empty directive because an unconstrained
// edit needs no region instruction.
func webImageMaskRegionDirective(maskBytes []byte) (string, error) {
	if int64(len(maskBytes)) > webImageMaskMaxBytes {
		return "", fmt.Errorf("mask exceeds the %d byte limit", int64(webImageMaskMaxBytes))
	}
	// Header-only pre-check bounds the decoded size before the full decode.
	config, _, err := image.DecodeConfig(bytes.NewReader(maskBytes))
	if err != nil {
		return "", fmt.Errorf("mask could not be decoded as an image: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 {
		return "", fmt.Errorf("mask has empty dimensions")
	}
	if config.Width > webImageMaskMaxSide || config.Height > webImageMaskMaxSide {
		return "", fmt.Errorf("mask dimensions %dx%d exceed the %d px per-side limit", config.Width, config.Height, webImageMaskMaxSide)
	}
	img, _, err := image.Decode(bytes.NewReader(maskBytes))
	if err != nil {
		return "", fmt.Errorf("mask could not be decoded as an image: %w", err)
	}
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	if width <= 0 || height <= 0 {
		return "", fmt.Errorf("mask has empty dimensions")
	}
	minX, minY := width, height
	maxX, maxY := -1, -1
	editable := 0
	var editableCells [3][3]bool
	var protectedCells [3][3]bool
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			px, py := x-b.Min.X, y-b.Min.Y
			if _, _, _, alpha := img.At(x, y).RGBA(); alpha < 0x8000 {
				if px < minX {
					minX = px
				}
				if py < minY {
					minY = py
				}
				if px > maxX {
					maxX = px
				}
				if py > maxY {
					maxY = py
				}
				editableCells[py*3/height][px*3/width] = true
				editable++
			} else {
				protectedCells[py*3/height][px*3/width] = true
			}
		}
	}
	if maxX < 0 {
		return "", fmt.Errorf("mask has no transparent (editable) region")
	}
	x1 := minX * 100 / width
	x2 := (maxX + 1) * 100 / width
	y1 := minY * 100 / height
	y2 := (maxY + 1) * 100 / height
	// Integer flooring can collapse a narrow span into a zero-width range
	// (x1 == x2); keep the described rectangle at least 1% wide.
	if x2 <= x1 {
		x2 = x1 + 1
	}
	if y2 <= y1 {
		y2 = y1 + 1
	}
	totalPixels := width * height
	coverage := editable * 100 / totalPixels
	if coverage < 1 {
		coverage = 1
	}
	// Only a fully transparent mask is truly unconstrained. Even a small opaque
	// area represents content that the caller asked the edit to preserve.
	fullCanvasBounds := x1 <= 1 && y1 <= 1 && x2 >= 99 && y2 >= 99
	if editable == totalPixels {
		return "", nil
	}
	protected := totalPixels - editable
	protectedAreas := webImageMaskGridAreas(protectedCells)
	if fullCanvasBounds && editable > protected {
		protectedCoverage := protected * 100 / totalPixels
		if protectedCoverage < 1 {
			protectedCoverage = 1
		}
		return fmt.Sprintf(
			"Edit the image except for the protected portions within the %s (about %d%% of the picture). Apply the requested change everywhere else and keep those protected portions unchanged from the reference image.",
			strings.Join(protectedAreas, ", "), protectedCoverage), nil
	}
	boundingPixels := (maxX - minX + 1) * (maxY - minY + 1)
	if editable != boundingPixels {
		var protectedWithinBoundsCells [3][3]bool
		for py := minY; py <= maxY; py++ {
			for px := minX; px <= maxX; px++ {
				if _, _, _, alpha := img.At(px+b.Min.X, py+b.Min.Y).RGBA(); alpha >= 0x8000 {
					protectedWithinBoundsCells[py*3/height][px*3/width] = true
				}
			}
		}
		protectedWithinBounds := webImageMaskGridAreas(protectedWithinBoundsCells)
		protectedDirective := ""
		if len(protectedWithinBounds) > 0 {
			protectedDirective = fmt.Sprintf(
				" The editable-area bounds also contain protected pixels in the %s; keep those protected pixels unchanged.",
				strings.Join(protectedWithinBounds, ", "))
		}
		return fmt.Sprintf(
			"Edit only the sparse masked areas in the %s (about %d%% of the picture).%s Apply the requested change only to the editable portions of those areas and keep everything else unchanged from the reference image.",
			strings.Join(webImageMaskGridAreas(editableCells), ", "), coverage, protectedDirective), nil
	}
	return fmt.Sprintf(
		"Edit only the masked region: the rectangular area from %d%% to %d%% of the image width and from %d%% to %d%% of the image height (the %s of the image, about %d%% of the picture). Apply the requested change strictly inside that area and keep everything outside it unchanged from the reference image.",
		x1, x2, y1, y2, webImageMaskLocationPhrase(x1, x2, y1, y2), coverage), nil
}

func webImageMaskGridAreas(cells [3][3]bool) []string {
	areas := make([]string, 0, 9)
	for row := 0; row < 3; row++ {
		for col := 0; col < 3; col++ {
			if !cells[row][col] {
				continue
			}
			x1 := col * 100 / 3
			x2 := (col + 1) * 100 / 3
			y1 := row * 100 / 3
			y2 := (row + 1) * 100 / 3
			areas = append(areas, webImageMaskLocationPhrase(x1, x2, y1, y2))
		}
	}
	return areas
}

// webImageMaskLocationPhrase names the bounding box's position on a 3x3 grid,
// derived from its center point.
func webImageMaskLocationPhrase(x1, x2, y1, y2 int) string {
	cx := (x1 + x2) / 2
	cy := (y1 + y2) / 2
	var row, col string
	switch {
	case cy < 34:
		row = "top"
	case cy < 67:
		row = "middle"
	default:
		row = "bottom"
	}
	switch {
	case cx < 34:
		col = "left"
	case cx < 67:
		col = "center"
	default:
		col = "right"
	}
	if row == "middle" && col == "center" {
		return "central area"
	}
	if row == "middle" {
		return "middle-" + col + " area"
	}
	if col == "center" {
		return row + "-center area"
	}
	return row + "-" + col + " area"
}

// webImageMaskFileBytes reads a multipart mask upload with a hard size cap.
func webImageMaskFileBytes(fh *multipart.FileHeader) ([]byte, error) {
	if fh == nil {
		return nil, fmt.Errorf("mask file is missing")
	}
	file, err := fh.Open()
	if err != nil {
		return nil, fmt.Errorf("mask could not be opened: %w", err)
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			_ = errClose
		}
	}()
	data, err := io.ReadAll(io.LimitReader(file, webImageMaskMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("mask could not be read: %w", err)
	}
	if int64(len(data)) > webImageMaskMaxBytes {
		return nil, fmt.Errorf("mask exceeds the %d byte limit", int64(webImageMaskMaxBytes))
	}
	return data, nil
}

// webImageMaskBytesFromJSON extracts mask bytes from a JSON edits payload.
// Accepted shapes: a data-URL string, or an object carrying image_url with a
// data URL (the codex-compatible form). Remote URLs and file_id references are
// rejected - the mask never leaves this process, so it must arrive inline.
func webImageMaskBytesFromJSON(rawJSON []byte) ([]byte, error) {
	mask := gjson.GetBytes(rawJSON, "mask")
	value := strings.TrimSpace(mask.String())
	if mask.IsObject() {
		if fileID := strings.TrimSpace(gjson.GetBytes(rawJSON, "mask.file_id").String()); fileID != "" {
			return nil, fmt.Errorf("mask file_id references are not supported for web image edits; inline the mask as a data URL")
		}
		value = strings.TrimSpace(gjson.GetBytes(rawJSON, "mask.image_url").String())
	}
	if value == "" {
		return nil, fmt.Errorf("mask is empty")
	}
	return webImageDataURLBytes(value, webImageMaskMaxBytes, "mask")
}

func webImageDataURLBytes(value string, maxBytes int, label string) ([]byte, error) {
	payload, ok := strings.CutPrefix(strings.TrimSpace(value), "data:")
	if !ok {
		return nil, fmt.Errorf("%s must be provided as a data URL for web image edits", label)
	}
	header, encoded, ok := strings.Cut(payload, ",")
	if !ok || !strings.Contains(strings.ToLower(header), ";base64") || !strings.HasPrefix(strings.ToLower(header), "image/") {
		return nil, fmt.Errorf("%s data URL is malformed", label)
	}
	encoded = strings.TrimSpace(encoded)
	encoding := base64.StdEncoding
	if !strings.HasSuffix(encoded, "=") {
		encoding = base64.RawStdEncoding
	}
	if len(encoded) > encoding.EncodedLen(maxBytes) {
		return nil, fmt.Errorf("%s exceeds the %d byte limit", label, int64(maxBytes))
	}
	data, err := encoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%s data URL could not be decoded: %w", label, err)
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("%s exceeds the %d byte limit", label, int64(maxBytes))
	}
	return data, nil
}

func webImageMaskMatchesReferenceDimensions(maskBytes []byte, referenceDataURL string, maxReferenceBytes int) error {
	if maxReferenceBytes <= 0 {
		maxReferenceBytes = webImageMaskMaxBytes
	}
	maskConfig, _, errMask := image.DecodeConfig(bytes.NewReader(maskBytes))
	if errMask != nil || maskConfig.Width <= 0 || maskConfig.Height <= 0 {
		return fmt.Errorf("mask dimensions are invalid")
	}
	referenceBytes, errReference := webImageDataURLBytes(referenceDataURL, maxReferenceBytes, "reference image")
	if errReference != nil {
		return errReference
	}
	referenceConfig, _, errReferenceConfig := image.DecodeConfig(bytes.NewReader(referenceBytes))
	if errReferenceConfig != nil || referenceConfig.Width <= 0 || referenceConfig.Height <= 0 {
		return fmt.Errorf("reference image dimensions are invalid")
	}
	if maskConfig.Width != referenceConfig.Width || maskConfig.Height != referenceConfig.Height {
		return fmt.Errorf("mask dimensions %dx%d must match reference image dimensions %dx%d", maskConfig.Width, maskConfig.Height, referenceConfig.Width, referenceConfig.Height)
	}
	return nil
}
