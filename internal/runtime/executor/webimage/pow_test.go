package webimage

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/sha3"
)

func fixedBrowserVector() BrowserVector {
	return BrowserVector{
		1920,
		"Sun Aug 16 2026 22:00:00 GMT+0800 (China Standard Time)",
		4294705152,
		0,
		"Mozilla/5.0 test",
		"https://chatgpt.com/sentinel/sdk.js",
		"build-test",
		"zh-CN",
		"zh-CN,zh",
		0,
		"vendor−function",
		"body",
		"window",
		1234.5,
		"00000000-0000-4000-8000-000000000001",
		"",
		8,
		1700000000000,
		0,
		0,
		0,
		0,
		0,
		0,
		0,
	}
}

func TestDefaultBrowserVectorUsesBrowserTimeSemantics(t *testing.T) {
	startedAt := time.Now().Add(-2 * time.Second).Truncate(time.Millisecond)
	session := &Session{createdAt: startedAt, UserAgent: "agent"}

	vector := defaultBrowserVector(session)

	if got := vector[1].(string); !strings.Contains(got, "GMT+0800 (China Standard Time)") {
		t.Fatalf("date = %q", got)
	}
	performanceNow, okPerformance := vector[13].(float64)
	if !okPerformance || performanceNow < 1900 || performanceNow > 5000 {
		t.Fatalf("performance.now = %#v", vector[13])
	}
	if got := int64(vector[17].(float64)); got != startedAt.UnixMilli() {
		t.Fatalf("timeOrigin = %d, want %d", got, startedAt.UnixMilli())
	}
}

func TestSolveProofMatchesCapturedSentinelAlgorithm(t *testing.T) {
	proof, errSolve := SolveProof(context.Background(), PoWChallenge{
		Required:   true,
		Seed:       "fixed-seed",
		Difficulty: "00ff",
	}, fixedBrowserVector(), PoWOptions{
		MaxAttempts: 500000,
		ElapsedMillis: func() int64 {
			return 17
		},
	})
	if errSolve != nil {
		t.Fatalf("SolveProof() error = %v", errSolve)
	}

	const want = "gAAAAABWzE5MjAsIlN1biBBdWcgMTYgMjAyNiAyMjowMDowMCBHTVQrMDgwMCAoQ2hpbmEgU3RhbmRhcmQgVGltZSkiLDQyOTQ3MDUxNTIsMzUsIk1vemlsbGEvNS4wIHRlc3QiLCJodHRwczovL2NoYXRncHQuY29tL3NlbnRpbmVsL3Nkay5qcyIsImJ1aWxkLXRlc3QiLCJ6aC1DTiIsInpoLUNOLHpoIiwxNywidmVuZG9y4oiSZnVuY3Rpb24iLCJib2R5Iiwid2luZG93IiwxMjM0LjUsIjAwMDAwMDAwLTAwMDAtNDAwMC04MDAwLTAwMDAwMDAwMDAwMSIsIiIsOCwxNzAwMDAwMDAwMDAwLDAsMCwwLDAsMCwwLDBd~S"
	if proof != want {
		t.Fatalf("SolveProof() = %q, want %q", proof, want)
	}
}

func TestBuildRequirementsTokenMatchesSentinelFraming(t *testing.T) {
	token, errBuild := BuildRequirementsToken(fixedBrowserVector(), 17)
	if errBuild != nil {
		t.Fatalf("BuildRequirementsToken() error = %v", errBuild)
	}
	if len(token) < len("gAAAAAC") || token[:len("gAAAAAC")] != "gAAAAAC" {
		t.Fatalf("BuildRequirementsToken() = %q", token)
	}
}

func TestBuildLegacyRequirementsTokenUsesLegacyVector(t *testing.T) {
	token, errBuild := BuildLegacyRequirementsToken(context.Background(), "test-agent", PoWOptions{})
	if errBuild != nil {
		t.Fatalf("BuildLegacyRequirementsToken() error = %v", errBuild)
	}
	encoded := strings.TrimPrefix(token, requirementsTokenPrefix)
	raw, errDecode := base64.StdEncoding.DecodeString(encoded)
	if errDecode != nil {
		t.Fatalf("decode legacy requirements token: %v", errDecode)
	}
	var vector []any
	if errJSON := json.Unmarshal(raw, &vector); errJSON != nil {
		t.Fatalf("decode legacy requirements vector: %v", errJSON)
	}
	if len(vector) != 18 || vector[4] != "test-agent" {
		t.Fatalf("legacy requirements vector = %#v", vector)
	}
}

func TestSolveLegacyProofUsesLegacySentinelFraming(t *testing.T) {
	proof, errSolve := SolveLegacyProof(context.Background(), PoWChallenge{
		Required:   true,
		Seed:       "legacy-seed",
		Difficulty: "000f",
	}, "test-agent", PoWOptions{})
	if errSolve != nil {
		t.Fatalf("SolveLegacyProof() error = %v", errSolve)
	}
	if !strings.HasPrefix(proof, proofTokenPrefix) || strings.HasSuffix(proof, "~S") {
		t.Fatalf("SolveLegacyProof() = %q", proof)
	}
	encoded := strings.TrimPrefix(proof, proofTokenPrefix)
	raw, errDecode := base64.StdEncoding.DecodeString(encoded)
	if errDecode != nil {
		t.Fatalf("decode legacy proof: %v", errDecode)
	}
	var vector []any
	if errJSON := json.Unmarshal(raw, &vector); errJSON != nil {
		t.Fatalf("decode legacy vector: %v", errJSON)
	}
	if len(vector) != 13 || vector[4] != "test-agent" {
		t.Fatalf("legacy vector = %#v", vector)
	}
	digest := sha3.Sum512([]byte("legacy-seed" + encoded))
	if got := hex.EncodeToString(digest[:])[:4]; got > "000f" {
		t.Fatalf("legacy digest prefix = %q", got)
	}
}

func TestSolveLegacyProofRejectsInvalidDifficulty(t *testing.T) {
	_, errSolve := SolveLegacyProof(context.Background(), PoWChallenge{Required: true, Seed: "seed", Difficulty: "not-hex"}, "test-agent", PoWOptions{})
	if !errors.Is(errSolve, ErrInvalidChallenge) {
		t.Fatalf("SolveLegacyProof() error = %v, want ErrInvalidChallenge", errSolve)
	}
}

func TestSolveLegacyProofAcceptsUppercaseHexPrefix(t *testing.T) {
	proof, errSolve := SolveLegacyProof(context.Background(), PoWChallenge{Required: true, Seed: "seed", Difficulty: "0Xffff"}, "test-agent", PoWOptions{MaxAttempts: 1})
	if errSolve != nil || !strings.HasPrefix(proof, proofTokenPrefix) {
		t.Fatalf("SolveLegacyProof() = %q, %v", proof, errSolve)
	}
}

func TestSolveProofHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, errSolve := SolveProof(ctx, PoWChallenge{Required: true, Seed: "seed", Difficulty: "00000000"}, fixedBrowserVector(), PoWOptions{MaxAttempts: 500000})
	if !errors.Is(errSolve, context.Canceled) {
		t.Fatalf("SolveProof() error = %v, want context canceled", errSolve)
	}
}

func TestSolveProofReturnsNoSolutionWithinBound(t *testing.T) {
	_, errSolve := SolveProof(context.Background(), PoWChallenge{Required: true, Seed: "seed", Difficulty: "00000000"}, fixedBrowserVector(), PoWOptions{MaxAttempts: 1})
	if !errors.Is(errSolve, ErrPoWExhausted) {
		t.Fatalf("SolveProof() error = %v, want ErrPoWExhausted", errSolve)
	}
}

func TestSolveProofRejectsInvalidDifficulty(t *testing.T) {
	_, errSolve := SolveProof(context.Background(), PoWChallenge{Required: true, Seed: "seed", Difficulty: "not-hex"}, fixedBrowserVector(), PoWOptions{})
	if !errors.Is(errSolve, ErrInvalidChallenge) {
		t.Fatalf("SolveProof() error = %v, want ErrInvalidChallenge", errSolve)
	}
}
