package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coresession "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPluginMetadataExcludesSessionCache(t *testing.T) {
	metadata := coresession.WithInfoCache(map[string]any{"public": "value"})
	for name, got := range map[string]map[string]any{
		"interceptor": cloneInterceptorMetadata(metadata),
		"executor":    mergeExecutorMetadata(metadata, metadata),
		"rpc":         sanitizePluginMetadata(metadata),
	} {
		if _, ok := got[coreexecutor.SessionInfoCacheMetadataKey]; ok {
			t.Fatalf("%s leaked session cache", name)
		}
		if _, err := json.Marshal(got); err != nil {
			t.Fatalf("%s metadata not serializable: %v", name, err)
		}
		if got["public"] != "value" {
			t.Fatalf("%s lost public metadata", name)
		}
	}
}

func TestRequestInterceptorNoopReturnsNoBodyReplacement(t *testing.T) {
	host := newHostWithRecords(capabilityRecord{id: "noop", plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
		RequestInterceptor: requestInterceptorFunc(func(_ context.Context, req pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
			req.Body[0] = 'X'
			return pluginapi.RequestInterceptResponse{}, nil
		}),
	}}})
	body := []byte(`{"model":"fixture"}`)
	resp := host.InterceptRequestBeforeAuth(context.Background(), pluginapi.RequestInterceptRequest{Body: body})
	if len(resp.Body) != 0 || body[0] != '{' {
		t.Fatalf("no-op changed request or returned replacement: original=%q response=%q", body, resp.Body)
	}
}

func TestRequestInterceptorPreservesHeaderClearing(t *testing.T) {
	host := newHostWithRecords(capabilityRecord{id: "clear", plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
		RequestInterceptor: requestInterceptorFunc(func(context.Context, pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
			return pluginapi.RequestInterceptResponse{ClearHeaders: []string{"X-Session-Id"}}, nil
		}),
	}}})
	for _, key := range []string{"X-Session-Id", "X-Session-ID", "x-session-id"} {
		resp := host.InterceptRequestAfterAuth(context.Background(), pluginapi.RequestInterceptRequest{Headers: http.Header{key: {"old"}}})
		if len(resp.ClearHeaders) != 1 || len(resp.Headers) != 0 {
			t.Fatalf("header deletion lost: %+v", resp)
		}
	}
}

func BenchmarkHostNoopInterceptors2MiB(b *testing.B) {
	records := make([]capabilityRecord, 3)
	for i := range records {
		records[i] = capabilityRecord{id: fmt.Sprint(i), plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			RequestInterceptor: requestInterceptorFunc(func(context.Context, pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
				return pluginapi.RequestInterceptResponse{}, nil
			}),
		}}}
	}
	host := newHostWithRecords(records...)
	req := pluginapi.RequestInterceptRequest{Body: make([]byte, 2<<20)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		host.InterceptRequestBeforeAuth(context.Background(), req)
	}
}
