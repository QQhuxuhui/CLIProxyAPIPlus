package helps

import (
	"context"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

const translatedRequestScratchPrefix = "translated-request|"

// TranslateRequestEnvelopePairForAttempt behaves like
// TranslateRequestEnvelopePairWithCodexMultiAgentV2, but reuses the read-only
// baseline translation across credential attempts of one request.
//
// A request whose first credential fails is executed again with the next one, and
// every attempt used to translate the same multi-megabyte body from scratch. The
// built-in translation is deterministic, so the baseline is memoised in the
// request scratch, keyed by the identity of the untouched source body. The working
// copy is still a fresh clone for every attempt because later stages modify it.
//
// Anything that could make the result attempt-specific falls back to a fresh
// translation: no scratch, an empty body, translator plugin hooks, or Codex and
// Responses input whose rewriting depends on request headers.
func TranslateRequestEnvelopePairForAttempt(ctx context.Context, headers http.Header, cfg *config.Config, from, to sdktranslator.Format, req sdktranslator.RequestEnvelope, source, payload []byte, metadata map[string]any) (original, working []byte) {
	scratch := cliproxyexecutor.RequestScratchFrom(metadata)
	if scratch == nil || len(source) == 0 || len(payload) == 0 ||
		sdktranslator.HasRequestPluginHooks() ||
		from == sdktranslator.FormatOpenAIResponse || from == sdktranslator.FormatCodex {
		return TranslateRequestEnvelopePairWithCodexMultiAgentV2(ctx, headers, cfg, from, to, req, payload, payload)
	}

	key := fmt.Sprintf("%s%s|%s|%s|%t|%p|%p:%d", translatedRequestScratchPrefix, from.String(), to.String(), req.Model, req.Stream, req.ModelInfo, &source[0], len(source))
	if cached, ok := scratch.Load(key); ok {
		if baseline, okBaseline := cached.([]byte); okBaseline && len(baseline) > 0 {
			return baseline, append([]byte(nil), baseline...)
		}
	}
	original, working = TranslateRequestEnvelopePairWithCodexMultiAgentV2(ctx, headers, cfg, from, to, req, payload, payload)
	if len(original) > 0 {
		scratch.Store(key, original)
	}
	return original, working
}

// ForgetTranslatedRequests releases memoised translations once an attempt has been
// accepted upstream, so a long response does not pin an extra copy of the body.
func ForgetTranslatedRequests(metadata map[string]any) {
	cliproxyexecutor.RequestScratchFrom(metadata).DeletePrefix(translatedRequestScratchPrefix)
}
