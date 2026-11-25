package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ResponseFinalizer converts an executor response into the client protocol's response inside
// the execution, before response or stream chunk interceptors run and before the request
// lifecycle completes. Plugins then see the response the client actually receives, under the
// same request ID, request snapshot and metadata as the rest of the request.
type ResponseFinalizer struct {
	// Protocol is the client response protocol reported to interceptors.
	Protocol string
	// Stream selects stream chunk interceptors: Build emits SSE data payloads, each is
	// intercepted and handed to Emit, and the execution returns no body. Otherwise Build emits
	// exactly one body and response interceptors run on it.
	Stream bool
	// OriginalRequest is the client request reported to interceptors. Empty keeps the
	// executor's original request.
	OriginalRequest []byte
	// Build converts the executor payload into client payloads and passes each to emit,
	// stopping when emit returns false.
	Build func(body []byte, emit func(payload []byte) bool) *interfaces.ErrorMessage
	// Emit delivers one intercepted stream payload with the response headers known so far.
	// It is required when Stream is set; an error stops the stream and fails the request.
	Emit func(headers http.Header, payload []byte) error
}

var errFinalizerBodyCount = errors.New("response finalizer must emit exactly one body")

// finalizeResponse runs a ResponseFinalizer and the interceptors for its protocol.
func (h *BaseAPIHandler) finalizeResponse(ctx context.Context, requestID string, fin *ResponseFinalizer, model, requestedModel string, opts coreexecutor.Options, requestBody []byte, rawHeaders, responseHeaders http.Header, payload []byte, skipPluginID string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	originalRequest := opts.OriginalRequest
	if len(fin.OriginalRequest) > 0 {
		originalRequest = fin.OriginalRequest
	}
	if !fin.Stream {
		var bodies [][]byte
		if errMsg := fin.Build(payload, func(body []byte) bool {
			bodies = append(bodies, body)
			return true
		}); errMsg != nil {
			return nil, nil, errMsg
		}
		if len(bodies) != 1 {
			return nil, nil, &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errFinalizerBodyCount}
		}
		body, headers := h.applyResponseInterceptors(ctx, requestID, fin.Protocol, model, requestedModel, opts, rawHeaders, responseHeaders, originalRequest, requestBody, bodies[0], http.StatusOK, skipPluginID)
		return body, headers, nil
	}
	if ctx.Err() != nil {
		return nil, nil, &interfaces.ErrorMessage{StatusCode: http.StatusRequestTimeout, Error: ctx.Err()}
	}
	interceptor := h.newSynthesizedStreamInterceptor(ctx, requestID, fin.Protocol, model, requestedModel, opts.Headers, rawHeaders, originalRequest, requestBody, opts.Metadata, skipPluginID)
	var errStream error
	errMsg := fin.Build(payload, func(body []byte) bool {
		// Stop before the next interceptor call once the client is gone.
		if errStream = ctx.Err(); errStream != nil {
			return false
		}
		kept, keep := interceptor.chunk(body)
		if !keep {
			return true
		}
		if errStream = fin.Emit(interceptor.downstreamHeaders(responseHeaders), kept); errStream != nil {
			return false
		}
		return true
	})
	if errStream == nil {
		errStream = ctx.Err()
	}
	if errStream != nil {
		return nil, nil, &interfaces.ErrorMessage{StatusCode: http.StatusRequestTimeout, Error: errStream}
	}
	if errMsg != nil {
		return nil, nil, errMsg
	}
	return nil, interceptor.downstreamHeaders(responseHeaders), nil
}

// synthesizedStreamInterceptor runs stream chunk interceptors over SSE payloads that are built
// from a non-stream executor response, so plugins can rewrite or drop them exactly as they
// would executor stream chunks.
type synthesizedStreamInterceptor struct {
	ctx            context.Context
	host           PluginInterceptorHost
	skipPluginID   string
	base           pluginapi.StreamChunkInterceptRequest
	originalReq    []byte
	requestBody    []byte
	baseHeaders    http.Header
	rawHeaders     http.Header
	passthrough    bool
	includeBodies  bool
	includeHistory bool
	history        [][]byte
	chunkIndex     int
}

// newSynthesizedStreamInterceptor prepares interception and runs the header-init hook. It
// returns nil when no stream interceptor is active; a nil interceptor passes chunks through.
func (h *BaseAPIHandler) newSynthesizedStreamInterceptor(ctx context.Context, requestID, sourceFormat, model, requestedModel string, requestHeaders, rawResponseHeaders http.Header, originalRequest, requestBody []byte, metadata map[string]any, skipPluginID string) *synthesizedStreamInterceptor {
	host := h.interceptorHost()
	if !streamInterceptorsEnabled(host) {
		return nil
	}
	s := &synthesizedStreamInterceptor{
		ctx:          ctx,
		host:         host,
		skipPluginID: skipPluginID,
		base: pluginapi.StreamChunkInterceptRequest{
			RequestID:      requestID,
			SourceFormat:   sourceFormat,
			Model:          model,
			RequestedModel: requestedModel,
			RequestHeaders: cloneHeader(requestHeaders),
			Metadata:       metadata,
		},
		originalReq:    cloneBytes(originalRequest),
		requestBody:    cloneBytes(requestBody),
		baseHeaders:    cloneHeader(rawResponseHeaders),
		rawHeaders:     cloneHeader(rawResponseHeaders),
		passthrough:    PassthroughHeadersEnabled(h.Cfg),
		includeBodies:  streamChunkPayloadIncludesRequestBody(host),
		includeHistory: streamChunkPayloadIncludesHistory(host),
	}
	initReq := s.request(nil, pluginapi.StreamChunkHeaderInitIndex)
	initReq.OriginalRequest = cloneBytes(s.originalReq)
	initReq.RequestBody = cloneBytes(s.requestBody)
	intercepted := interceptStreamChunk(ctx, host, initReq, skipPluginID)
	s.rawHeaders = finalInterceptorHeaders(s.rawHeaders, intercepted.Headers)
	return s
}

func (s *synthesizedStreamInterceptor) request(payload []byte, index int) pluginapi.StreamChunkInterceptRequest {
	req := s.base
	req.RequestHeaders = cloneHeader(s.base.RequestHeaders)
	req.ResponseHeaders = cloneHeader(s.rawHeaders)
	req.Body = payload
	req.ChunkIndex = index
	return req
}

// downstreamHeaders returns the response headers to send after interceptor changes.
func (s *synthesizedStreamInterceptor) downstreamHeaders(fallback http.Header) http.Header {
	if s == nil {
		return fallback
	}
	return downstreamHeadersAfterInterceptors(s.baseHeaders, s.rawHeaders, s.passthrough)
}

// chunk passes one SSE data payload through the interceptors. It returns the payload to write
// and false when a plugin dropped the chunk.
func (s *synthesizedStreamInterceptor) chunk(payload []byte) ([]byte, bool) {
	if s == nil {
		return payload, true
	}
	req := s.request(cloneBytes(payload), s.chunkIndex)
	if s.includeHistory {
		req.HistoryChunks = cloneByteSlices(s.history)
	}
	if s.includeBodies {
		req.OriginalRequest = cloneBytes(s.originalReq)
		req.RequestBody = cloneBytes(s.requestBody)
	}
	intercepted := interceptStreamChunk(s.ctx, s.host, req, s.skipPluginID)
	s.rawHeaders = finalInterceptorHeaders(s.rawHeaders, intercepted.Headers)
	s.chunkIndex++
	if len(intercepted.Body) > 0 {
		payload = cloneBytes(intercepted.Body)
	}
	if intercepted.DropChunk {
		return nil, false
	}
	s.history = appendStreamInterceptorHistory(s.history, payload)
	return payload, true
}
