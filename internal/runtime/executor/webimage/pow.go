package webimage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	proofTokenPrefix        = "gAAAAAB"
	requirementsTokenPrefix = "gAAAAAC"
	defaultPoWMaxAttempts   = 500000
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
