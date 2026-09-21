package helps

import (
	"bytes"
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func translateCacheFixture() ([]byte, sdktranslator.Format, sdktranslator.Format, sdktranslator.RequestEnvelope) {
	body := []byte(`{"model":"gemini-3-pro","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	from := sdktranslator.FromString("claude")
	to := sdktranslator.FromString("antigravity")
	return body, from, to, sdktranslator.RequestEnvelope{Format: from, Model: "gemini-3-pro", Stream: true}
}

func TestTranslateRequestEnvelopePairForAttemptMatchesUncached(t *testing.T) {
	body, from, to, env := translateCacheFixture()
	ctx := context.Background()
	wantOriginal, wantWorking := TranslateRequestEnvelopePairWithCodexMultiAgentV2(ctx, nil, nil, from, to, env, body, body)

	meta := cliproxyexecutor.WithRequestScratch(nil)
	for attempt := 0; attempt < 3; attempt++ {
		original, working := TranslateRequestEnvelopePairForAttempt(ctx, nil, nil, from, to, env, body, body, meta)
		if !bytes.Equal(original, wantOriginal) || !bytes.Equal(working, wantWorking) {
			t.Fatalf("attempt %d: cached translation differs from uncached", attempt)
		}
		if len(working) > 0 && &working[0] == &original[0] {
			t.Fatalf("attempt %d: working copy shares the baseline array", attempt)
		}
		// Later stages modify the working copy in place; the baseline must survive.
		for i := range working {
			working[i] = 'x'
		}
	}
	original, _ := TranslateRequestEnvelopePairForAttempt(ctx, nil, nil, from, to, env, body, body, meta)
	if !bytes.Equal(original, wantOriginal) {
		t.Fatal("baseline was corrupted by working copy writes")
	}
}

func TestTranslateRequestEnvelopePairForAttemptReusesBaseline(t *testing.T) {
	body, from, to, env := translateCacheFixture()
	ctx := context.Background()
	meta := cliproxyexecutor.WithRequestScratch(nil)

	first, _ := TranslateRequestEnvelopePairForAttempt(ctx, nil, nil, from, to, env, body, body, meta)
	second, _ := TranslateRequestEnvelopePairForAttempt(ctx, nil, nil, from, to, env, body, body, meta)
	if &first[0] != &second[0] {
		t.Fatal("second attempt did not reuse the memoised baseline")
	}

	// A different source body, model, or stream flag must never hit the entry.
	other := append([]byte(nil), body...)
	third, _ := TranslateRequestEnvelopePairForAttempt(ctx, nil, nil, from, to, env, other, other, meta)
	if &third[0] == &first[0] {
		t.Fatal("different source body reused the baseline")
	}
	envModel := env
	envModel.Model = "gemini-3-flash"
	fourth, _ := TranslateRequestEnvelopePairForAttempt(ctx, nil, nil, from, to, envModel, body, body, meta)
	if &fourth[0] == &first[0] {
		t.Fatal("different model reused the baseline")
	}

	ForgetTranslatedRequests(meta)
	fifth, _ := TranslateRequestEnvelopePairForAttempt(ctx, nil, nil, from, to, env, body, body, meta)
	if &fifth[0] == &first[0] {
		t.Fatal("baseline survived ForgetTranslatedRequests")
	}
}

func TestTranslateRequestEnvelopePairForAttemptWithoutScratch(t *testing.T) {
	body, from, to, env := translateCacheFixture()
	ctx := context.Background()
	want, _ := TranslateRequestEnvelopePairWithCodexMultiAgentV2(ctx, nil, nil, from, to, env, body, body)
	for _, meta := range []map[string]any{nil, {}} {
		got, working := TranslateRequestEnvelopePairForAttempt(ctx, nil, nil, from, to, env, body, body, meta)
		if !bytes.Equal(got, want) || !bytes.Equal(working, want) {
			t.Fatal("translation without a scratch differs from the uncached path")
		}
	}
	ForgetTranslatedRequests(nil)
}
