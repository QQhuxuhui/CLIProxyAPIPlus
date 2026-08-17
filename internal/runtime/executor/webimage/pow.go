package webimage

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	"golang.org/x/crypto/sha3"
)

const (
	proofTokenPrefix        = "gAAAAAB"
	requirementsTokenPrefix = "gAAAAAC"
	defaultPoWMaxAttempts   = 500000
	legacyPoWHashHexLength  = 128
)

// SolveProof reproduces the proof generator embedded in the captured Sentinel SDK.
func SolveProof(ctx context.Context, challenge PoWChallenge, vector BrowserVector, opts PoWOptions) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !challenge.Required {
		return BuildRequirementsToken(vector, elapsedMillis(opts, time.Now()))
	}
	seed := strings.TrimSpace(challenge.Seed)
	difficulty := strings.ToLower(strings.TrimSpace(challenge.Difficulty))
	if seed == "" || !validDifficulty(difficulty) || len(vector) <= 9 {
		return "", ErrInvalidChallenge
	}

	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultPoWMaxAttempts
	}
	startedAt := time.Now()
	for nonce := 0; nonce < maxAttempts; nonce++ {
		if errContext := ctx.Err(); errContext != nil {
			return "", errContext
		}
		candidate := cloneBrowserVector(vector)
		candidate[3] = nonce
		candidate[9] = elapsedMillis(opts, startedAt)
		encoded, errEncode := encodeBrowserVector(candidate)
		if errEncode != nil {
			return "", fmt.Errorf("encode Sentinel browser vector: %w", errEncode)
		}
		hash := sentinelHash(seed + encoded)
		if hash[:len(difficulty)] <= difficulty {
			return proofTokenPrefix + encoded + "~S", nil
		}
	}
	return "", ErrPoWExhausted
}

// BuildLegacyRequirementsToken generates the Sentinel token shared by V2 prepare and legacy fallback.
func BuildLegacyRequirementsToken(ctx context.Context, userAgent string, opts PoWOptions) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultPoWMaxAttempts
	}
	config := []any{
		legacyRandomChoice([]int{16, 24, 32}) + legacyRandomChoice([]int{3000, 4000, 6000}),
		time.Now().UTC().Format("Mon Jan 02 2006 15:04:05 GMT+0000 (UTC)"),
		nil,
		0,
		userAgent,
		nil,
		"dpl=1440a687921de39ff5ee56b92807faaadce73f13",
		"en-US",
		"en-US,zh-CN",
		0,
		legacyRandomChoice([]string{
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
		legacyRandomChoice([]string{"innerWidth", "innerHeight", "devicePixelRatio", "screen", "chrome", "location", "history", "navigator"}),
		legacyRandomFloat64(),
		uuid.NewString(),
		"",
		8,
		time.Now().Unix(),
	}
	seed := strconv.FormatFloat(legacyRandomFloat64(), 'f', -1, 64)
	target := []byte{0x0f, 0xff, 0xff}
	for nonce := 0; nonce < maxAttempts; nonce++ {
		if errContext := ctx.Err(); errContext != nil {
			return "", errContext
		}
		config[3] = nonce
		config[9] = nonce >> 1
		raw, errMarshal := json.Marshal(config)
		if errMarshal != nil {
			return "", fmt.Errorf("encode legacy Sentinel requirements vector: %w", errMarshal)
		}
		encoded := base64.StdEncoding.EncodeToString(raw)
		digest := sha3.Sum512([]byte(seed + encoded))
		if bytes.Compare(digest[:len(target)], target) <= 0 {
			return requirementsTokenPrefix + encoded, nil
		}
	}
	return "", ErrPoWExhausted
}

// SolveLegacyProof reproduces the SHA3-512 proof used by the legacy Sentinel endpoint.
func SolveLegacyProof(ctx context.Context, challenge PoWChallenge, userAgent string, opts PoWOptions) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !challenge.Required {
		return "", nil
	}
	seed := strings.TrimSpace(challenge.Seed)
	difficulty := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(challenge.Difficulty)), "0x")
	if seed == "" || !validLegacyDifficulty(difficulty) {
		return "", ErrInvalidChallenge
	}

	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultPoWMaxAttempts
	}
	config := legacyPoWConfig(userAgent)
	for nonce := 0; nonce < maxAttempts; nonce++ {
		if errContext := ctx.Err(); errContext != nil {
			return "", errContext
		}
		config[3] = nonce
		raw, errMarshal := json.Marshal(config)
		if errMarshal != nil {
			return "", fmt.Errorf("encode legacy Sentinel browser vector: %w", errMarshal)
		}
		encoded := base64.StdEncoding.EncodeToString(raw)
		digest := sha3.Sum512([]byte(seed + encoded))
		hexDigest := hex.EncodeToString(digest[:])
		if hexDigest[:len(difficulty)] <= difficulty {
			return proofTokenPrefix + encoded, nil
		}
	}
	return "", ErrPoWExhausted
}

func legacyPoWConfig(userAgent string) []any {
	return []any{
		legacyRandomChoice([]int{3000, 4000, 6000}) * legacyRandomChoice([]int{1, 2, 4}),
		time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"),
		nil,
		0,
		userAgent,
		"https://tcr9i.chat.openai.com/v2/35536E1E-65B4-4D96-9D97-6ADB7EFF8147/api.js",
		"dpl=1440a687921de39ff5ee56b92807faaadce73f13",
		"en",
		"en-US",
		nil,
		"plugins−[object PluginArray]",
		legacyRandomChoice([]string{"_reactListeningcfilawjnerp", "_reactListening9ne2dfo1i47"}),
		legacyRandomChoice([]string{"alert", "ontransitionend", "onprogress"}),
	}
}

func legacyRandomChoice[T any](values []T) T {
	index := 0
	if len(values) > 1 {
		limit := big.NewInt(int64(len(values)))
		if value, errRandom := cryptorand.Int(cryptorand.Reader, limit); errRandom == nil {
			index = int(value.Int64())
		}
	}
	return values[index]
}

func legacyRandomFloat64() float64 {
	var raw [8]byte
	if _, errRead := cryptorand.Read(raw[:]); errRead != nil {
		return float64(time.Now().UnixNano()&((1<<53)-1)) / float64(1<<53)
	}
	return float64(binary.BigEndian.Uint64(raw[:])>>11) / float64(1<<53)
}

func validLegacyDifficulty(difficulty string) bool {
	if difficulty == "" || len(difficulty) > legacyPoWHashHexLength {
		return false
	}
	for _, char := range difficulty {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// BuildRequirementsToken encodes the non-PoW Sentinel requirements token.
func BuildRequirementsToken(vector BrowserVector, elapsed int64) (string, error) {
	if len(vector) <= 9 {
		return "", ErrInvalidChallenge
	}
	candidate := cloneBrowserVector(vector)
	candidate[3] = 1
	candidate[9] = elapsed
	encoded, errEncode := encodeBrowserVector(candidate)
	if errEncode != nil {
		return "", fmt.Errorf("encode Sentinel requirements vector: %w", errEncode)
	}
	return requirementsTokenPrefix + encoded, nil
}

func cloneBrowserVector(vector BrowserVector) BrowserVector {
	cloned := make(BrowserVector, len(vector))
	copy(cloned, vector)
	return cloned
}

func elapsedMillis(opts PoWOptions, startedAt time.Time) int64 {
	if opts.ElapsedMillis != nil {
		return opts.ElapsedMillis()
	}
	return time.Since(startedAt).Milliseconds()
}

func encodeBrowserVector(vector BrowserVector) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if errEncode := encoder.Encode(vector); errEncode != nil {
		return "", errEncode
	}
	data := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	return base64.StdEncoding.EncodeToString(data), nil
}

func validDifficulty(difficulty string) bool {
	if difficulty == "" || len(difficulty) > 8 {
		return false
	}
	for _, char := range difficulty {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func sentinelHash(value string) string {
	var hash uint32 = 2166136261
	for _, codeUnit := range utf16.Encode([]rune(value)) {
		hash ^= uint32(codeUnit)
		hash *= 16777619
	}
	hash ^= hash >> 16
	hash *= 2246822507
	hash ^= hash >> 13
	hash *= 3266489909
	hash ^= hash >> 16
	return fmt.Sprintf("%08x", hash)
}
