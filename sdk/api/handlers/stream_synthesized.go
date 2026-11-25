package handlers

import (
	"bytes"
	"context"
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
	// Stream selects stream chunk interceptors: Build returns SSE data payloads and the
	// execution returns them framed as an SSE body terminated by [DONE]. Otherwise Build
	// returns exactly one body and response interceptors run on it.
	Stream bool
	// OriginalRequest is the client request reported to interceptors. Empty keeps the
	// executor's original request.
	OriginalRequest []byte
	// Build converts the executor payload into the client payloads.
	Build func(body []byte) ([][]byte, *interfaces.ErrorMessage)
}

// finalizeResponse runs a ResponseFinalizer and the interceptors for its protocol.
func (h *BaseAPIHandler) finalizeResponse(ctx context.Context, requestID string, fin *ResponseFinalizer, model, requestedModel string, opts coreexecutor.Options, requestBody []byte, rawHeaders, responseHeaders http.Header, payload []byte, skipPluginID string) ([]byte, http.Header, *interfaces.ErrorMessage) {
	bodies, errMsg := fin.Build(payload)
	if errMsg != nil {
		return nil, nil, errMsg
	}
	originalRequest := opts.OriginalRequest
	if len(fin.OriginalRequest) > 0 {
		originalRequest = fin.OriginalRequest
	}
	if !fin.Stream {
		if len(bodies) != 1 {
			return nil, nil, &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errFinalizerBodyCount}
		}
		body, headers := h.applyResponseInterceptors(ctx, requestID, fin.Protocol, model, requestedModel, opts, rawHeaders, responseHeaders, originalRequest, requestBody, bodies[0], http.StatusOK, skipPluginID)
		return body, headers, nil
	}
	interceptor := h.newSynthesizedStreamInterceptor(ctx, requestID, fin.Protocol, model, requestedModel, opts.Headers, rawHeaders, originalRequest, requestBody, opts.Metadata, skipPluginID)
	var out bytes.Buffer
	for _, body := range bodies {
		if kept, keep := interceptor.chunk(body); keep {
			out.WriteString("data: ")
			out.Write(kept)
			out.WriteString("\n\n")
		}
	}
	out.WriteString("data: [DONE]\n\n")
	return out.Bytes(), interceptor.downstreamHeaders(responseHeaders), nil
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
