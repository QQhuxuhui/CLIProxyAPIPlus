package webimage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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
