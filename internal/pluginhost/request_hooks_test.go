package pluginhost

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestHostHasRequestHooks(t *testing.T) {
	var missing *Host
	if missing.HasRequestHooks() {
		t.Fatal("nil host reported request hooks")
	}
	if newHostWithRecords(capabilityRecord{id: "plain", plugin: pluginapi.Plugin{}}).HasRequestHooks() {
		t.Fatal("plugin without request capabilities reported as a request hook")
	}
	host := newHostWithRecords(capabilityRecord{
		id: "normalizer",
		plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			RequestNormalizer: requestNormalizerFunc(func(_ context.Context, req pluginapi.RequestTransformRequest) (pluginapi.PayloadResponse, error) {
				return pluginapi.PayloadResponse{Body: req.Body}, nil
			}),
		}},
	})
	if !host.HasRequestHooks() {
		t.Fatal("request normalizer plugin not reported as a request hook")
	}
}
