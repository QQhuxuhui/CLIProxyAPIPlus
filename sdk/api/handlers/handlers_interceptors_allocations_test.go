package handlers

import (
	"context"
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type readonlyInterceptorHost struct{ *handlerInterceptorTestHost }

func (*readonlyInterceptorHost) RequestInterceptorsReadOnly() bool { return true }

func TestHandlerNoopInterceptionSharesImmutablePayload(t *testing.T) {
	body := make([]byte, 2<<20)
	host := &readonlyInterceptorHost{&handlerInterceptorTestHost{
		interceptRequestBeforeAuth: func(_ context.Context, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
			if &req.Body[0] != &body[0] {
				t.Fatal("read-only host received a body clone")
			}
			return pluginapi.RequestInterceptResponse{}
		},
	}}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	handler.SetPluginHost(host)
	req := coreexecutor.Request{Payload: body}
	opts := coreexecutor.Options{OriginalRequest: body}
	got, gotOpts, err := handler.applyRequestInterceptorsBeforeAuth(context.Background(), "openai", "model", "request", req, opts, "")
	if err != nil || &got.Payload[0] != &body[0] || &gotOpts.OriginalRequest[0] != &body[0] {
		t.Fatal("no-op interception replaced body")
	}
}

func TestAfterAuthCaptureDoesNotCloneUnchangedBody(t *testing.T) {
	body := make([]byte, 2<<20)
	capture := &requestAfterAuthCapture{}
	capture.record(coreexecutor.RequestAfterAuthInterceptRequest{Body: body}, coreexecutor.RequestAfterAuthInterceptResponse{})
	req, opts := capture.apply(coreexecutor.Request{Payload: body}, coreexecutor.Options{OriginalRequest: body})
	if &req.Payload[0] != &body[0] || &opts.OriginalRequest[0] != &body[0] {
		t.Fatal("capture copied unchanged payload")
	}
	changed := []byte("changed")
	capture.record(coreexecutor.RequestAfterAuthInterceptRequest{Body: body}, coreexecutor.RequestAfterAuthInterceptResponse{Body: changed})
	changed[0] = 'X'
	req, opts = capture.apply(req, opts)
	if string(req.Payload) != "changed" || string(opts.OriginalRequest) != "changed" {
		t.Fatal("capture did not isolate modified plugin output")
	}
}

func BenchmarkHandlerNoopInterceptors2MiB(b *testing.B) {
	body := make([]byte, 2<<20)
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	handler.SetPluginHost(&readonlyInterceptorHost{&handlerInterceptorTestHost{
		interceptRequestBeforeAuth: func(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
			return pluginapi.RequestInterceptResponse{}
		},
	}})
	req := coreexecutor.Request{Payload: body}
	opts := coreexecutor.Options{OriginalRequest: body}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		handler.applyRequestInterceptorsBeforeAuth(context.Background(), "openai", "model", "request", req, opts, "")
	}
}

type providerResolvedInterceptorHost struct {
	handlerDirectExecutorInterceptorHost
}

func (*providerResolvedInterceptorHost) PluginExecutorProvider(string) string {
	return "actual-provider"
}

func TestDirectPluginInterceptionUsesResolvedProvider(t *testing.T) {
	host := &providerResolvedInterceptorHost{}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	handler.SetPluginHost(host)
	opts := coreexecutor.Options{}
	setPluginExecutorProviders(host, "different-plugin-id", &opts)
	providers, _ := opts.Metadata[coreexecutor.RequestProvidersMetadataKey].([]string)
	if len(providers) != 1 || providers[0] != "actual-provider" {
		t.Fatalf("pre-auth providers=%v", providers)
	}
	_, _, err := handler.applyRequestInterceptorsAfterPluginExecutorRoute(context.Background(), host, "different-plugin-id", "openai", "model", "request", coreexecutor.Request{Payload: []byte(`{}`)}, opts, "")
	if err != nil || !host.afterAuthCalled || host.afterAuthReq.Provider != "actual-provider" {
		t.Fatalf("after-auth provider=%q, err=%v", host.afterAuthReq.Provider, err)
	}
}
