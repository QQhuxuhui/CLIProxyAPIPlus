package translator

import "context"

// PluginHooks defines optional translator extension hooks provided by plugins.
type PluginHooks interface {
	NormalizeRequest(ctx context.Context, from, to Format, model string, body []byte, stream bool) []byte
	TranslateRequest(ctx context.Context, from, to Format, model string, body []byte, stream bool) ([]byte, bool)
	NormalizeResponseBefore(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, body []byte, stream bool) []byte
	TranslateResponse(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, body []byte, stream bool) ([]byte, bool)
	NormalizeResponseAfter(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, body []byte, stream bool) []byte
}

// RequestHookReporter is an optional PluginHooks extension. HasRequestHooks reports
// whether any installed plugin can normalize or translate requests; when it returns
// false, NormalizeRequest and TranslateRequest are known to leave bodies unchanged.
type RequestHookReporter interface {
	HasRequestHooks() bool
}
