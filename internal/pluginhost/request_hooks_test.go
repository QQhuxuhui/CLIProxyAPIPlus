package pluginhost

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
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

func TestNormalizeRequestWithoutNormalizersReturnsBodyUncopied(t *testing.T) {
	host := newHostWithRecords(capabilityRecord{id: "plain", plugin: pluginapi.Plugin{}})
	body := []byte(`{"model":"m"}`)
	got := host.NormalizeRequest(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "m", body, false)
	if &got[0] != &body[0] {
		t.Fatal("body was copied although no normalizer plugin is loaded")
	}
}

func TestResponseHooksWithoutPluginsReturnBodyUncopied(t *testing.T) {
	host := newHostWithRecords(capabilityRecord{id: "plain", plugin: pluginapi.Plugin{}})
	if host.HasResponseHooks() {
		t.Fatal("plugin without response capabilities reported as a response hook")
	}
	body := []byte(`data: {"x":1}`)
	before := host.NormalizeResponseBefore(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "m", nil, nil, body, true)
	after := host.NormalizeResponseAfter(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "m", nil, nil, body, true)
	if &before[0] != &body[0] || &after[0] != &body[0] {
		t.Fatal("response body was copied although no response plugin is loaded")
	}
	if got, ok := host.TranslateResponse(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "m", nil, nil, body, true); ok || &got[0] != &body[0] {
		t.Fatal("unhandled TranslateResponse copied the body")
	}
	if got, ok := host.TranslateRequest(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "m", body, false); ok || &got[0] != &body[0] {
		t.Fatal("unhandled TranslateRequest copied the body")
	}
	withPlugin := newHostWithRecords(capabilityRecord{
		id: "after",
		plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			ResponseAfterTranslator: responseNormalizerFunc(func(_ context.Context, req pluginapi.ResponseTransformRequest) (pluginapi.PayloadResponse, error) {
				return pluginapi.PayloadResponse{Body: append(req.Body, '!')}, nil
			}),
		}},
	})
	if !withPlugin.HasResponseHooks() {
		t.Fatal("response normalizer plugin not reported as a response hook")
	}
	out := withPlugin.NormalizeResponseAfter(context.Background(), sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, "m", nil, nil, body, true)
	if string(out) != string(body)+"!" || string(body) != `data: {"x":1}` {
		t.Fatalf("plugin path changed input or lost output: out=%q body=%q", out, body)
	}
}
