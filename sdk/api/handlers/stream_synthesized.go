package handlers

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// SynthesizedStreamInterceptor runs stream chunk interceptors over SSE payloads that a handler
// builds itself instead of receiving from an executor stream, so plugins can rewrite or drop
// them exactly as they would executor chunks.
type SynthesizedStreamInterceptor struct {
	ctx            context.Context
	host           PluginInterceptorHost
	base           pluginapi.StreamChunkInterceptRequest
	baseHeaders    http.Header
	rawHeaders     http.Header
	passthrough    bool
	includeBodies  bool
	includeHistory bool
	history        [][]byte
	chunkIndex     int
}

// NewSynthesizedStreamInterceptor prepares interception for a synthesized stream and runs the
// header-init hook. It returns nil when no stream interceptor is active; a nil interceptor passes
// chunks through unchanged.
func (h *BaseAPIHandler) NewSynthesizedStreamInterceptor(ctx context.Context, sourceFormat, model, requestedModel string, requestHeaders, responseHeaders http.Header, originalRequest []byte, metadata map[string]any) *SynthesizedStreamInterceptor {
	host := h.interceptorHost()
	if !streamInterceptorsEnabled(host) {
		return nil
	}
	s := &SynthesizedStreamInterceptor{
		ctx:  ctx,
		host: host,
		base: pluginapi.StreamChunkInterceptRequest{
			RequestID:       uuid.NewString(),
			SourceFormat:    sourceFormat,
			Model:           model,
			RequestedModel:  requestedModel,
			RequestHeaders:  cloneHeader(requestHeaders),
			OriginalRequest: cloneBytes(originalRequest),
			RequestBody:     cloneBytes(originalRequest),
			Metadata:        metadata,
		},
		baseHeaders:    cloneHeader(responseHeaders),
		rawHeaders:     cloneHeader(responseHeaders),
		passthrough:    PassthroughHeadersEnabled(h.Cfg),
		includeBodies:  streamChunkPayloadIncludesRequestBody(host),
		includeHistory: streamChunkPayloadIncludesHistory(host),
	}
	initReq := s.request(nil, pluginapi.StreamChunkHeaderInitIndex)
	initReq.OriginalRequest = cloneBytes(s.base.OriginalRequest)
	initReq.RequestBody = cloneBytes(s.base.RequestBody)
	intercepted := interceptStreamChunk(ctx, host, initReq, "")
	s.rawHeaders = finalInterceptorHeaders(s.rawHeaders, intercepted.Headers)
	return s
}

func (s *SynthesizedStreamInterceptor) request(payload []byte, index int) pluginapi.StreamChunkInterceptRequest {
	req := s.base
	req.RequestHeaders = cloneHeader(s.base.RequestHeaders)
	req.ResponseHeaders = cloneHeader(s.rawHeaders)
	req.OriginalRequest = nil
	req.RequestBody = nil
	req.Body = payload
	req.ChunkIndex = index
	return req
}

// DownstreamHeaders returns the response headers to send after interceptor changes.
func (s *SynthesizedStreamInterceptor) DownstreamHeaders(fallback http.Header) http.Header {
	if s == nil {
		return fallback
	}
	return downstreamHeadersAfterInterceptors(s.baseHeaders, s.rawHeaders, s.passthrough)
}

// Chunk passes one SSE data payload through the interceptors. It returns the payload to write
// and false when a plugin dropped the chunk.
func (s *SynthesizedStreamInterceptor) Chunk(payload []byte) ([]byte, bool) {
	if s == nil {
		return payload, true
	}
	req := s.request(cloneBytes(payload), s.chunkIndex)
	if s.includeHistory {
		req.HistoryChunks = cloneByteSlices(s.history)
	}
	if s.includeBodies {
		req.OriginalRequest = cloneBytes(s.base.OriginalRequest)
		req.RequestBody = cloneBytes(s.base.RequestBody)
	}
	intercepted := interceptStreamChunk(s.ctx, s.host, req, "")
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
