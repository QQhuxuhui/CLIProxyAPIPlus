package pluginhost

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRequestInterceptorFilterProviderAndProtocol(t *testing.T) {
	for _, tc := range []struct {
		name      string
		request   pluginapi.RequestInterceptRequest
		wantCalls int
	}{
		{"other-provider-before", pluginapi.RequestInterceptRequest{Providers: []string{"antigravity"}, SourceFormat: "openai"}, 0},
		{"mixed-before", pluginapi.RequestInterceptRequest{Providers: []string{"antigravity", "codex"}, SourceFormat: "openai"}, 1},
		{"unknown-before", pluginapi.RequestInterceptRequest{SourceFormat: "openai"}, 1},
		{"other-source", pluginapi.RequestInterceptRequest{Provider: "codex", SourceFormat: "gemini", ToFormat: "codex"}, 0},
		{"other-target", pluginapi.RequestInterceptRequest{Provider: "codex", SourceFormat: "openai", ToFormat: "openai"}, 0},
		{"other-provider-after", pluginapi.RequestInterceptRequest{Provider: "antigravity", Providers: []string{"codex"}, SourceFormat: "openai", ToFormat: "codex"}, 0},
		{"selected-after", pluginapi.RequestInterceptRequest{Provider: "codex", SourceFormat: "openai", ToFormat: "codex"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			host := newHostWithRecords(capabilityRecord{id: "codex-only", requestInterceptors: config.RequestInterceptorFilter{Providers: []string{"codex"}, SourceFormats: []string{"openai"}, TargetFormats: []string{"codex"}}, plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
				RequestInterceptor: requestInterceptorFunc(func(context.Context, pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
					calls++
					return pluginapi.RequestInterceptResponse{}, nil
				}),
			}}})
			tc.request.Body = make([]byte, 2<<20)
			host.InterceptRequestBeforeAuth(context.Background(), tc.request)
			if calls != tc.wantCalls {
				t.Fatalf("calls=%d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestRuntimeInterceptorFilterIsHostOnly(t *testing.T) {
	cfg, err := config.ParseConfigBytes([]byte("plugins:\n  enabled: true\n  configs:\n    fixture:\n      enabled: true\n      request-interceptors:\n        providers: [' CODEX ']\n      plugin-owned: unchanged\n"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := runtimeConfigFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	item := runtime.Items["fixture"]
	if len(item.RequestInterceptors.Providers) != 1 || item.RequestInterceptors.Providers[0] != "codex" {
		t.Fatalf("bad runtime filter: %+v", item.RequestInterceptors)
	}
	if strings.Contains(string(item.ConfigYAML), "request-interceptors") || !strings.Contains(string(item.ConfigYAML), "plugin-owned: unchanged") {
		t.Fatalf("host filter crossed plugin config boundary: %s", item.ConfigYAML)
	}
}

func TestNativeSchedulerCannotMutateSharedProjection(t *testing.T) {
	host := newHostWithRecords(capabilityRecord{id: "scheduler", plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
		Scheduler: schedulerFunc(func(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
			if req.Candidates[0].Attributes["project_id"] != "original" {
				t.Fatal("earlier invocation corrupted projection")
			}
			req.Candidates[0].Attributes["project_id"] = "mutated"
			req.Candidates[0].ID = "mutated"
			return pluginapi.SchedulerPickResponse{}, nil
		}),
	}}})
	req := pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "fixture", Attributes: map[string]string{"project_id": "original"}}}}
	for range 2 {
		host.PickAuth(context.Background(), req)
	}
	if req.Candidates[0].ID != "fixture" || req.Candidates[0].Attributes["project_id"] != "original" {
		t.Fatal("input projection changed")
	}
}

func TestPluginExecutorProviderUsesProviderIdentifier(t *testing.T) {
	host := newHostWithRecords(capabilityRecord{id: "plugin-id", plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{Executor: &fakeExecutor{identifier: "actual-provider"}}}})
	if got := host.PluginExecutorProvider("plugin-id"); got != "actual-provider" {
		t.Fatalf("resolved provider=%q", got)
	}
}

func TestRequestInterceptorFiltersReloadWithoutPluginChanges(t *testing.T) {
	calls := 0
	plugin := validTestPlugin("alpha")
	plugin.Capabilities.RequestInterceptor = requestInterceptorFunc(func(context.Context, pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
		calls++
		return pluginapi.RequestInterceptResponse{}, nil
	})
	loader := newTestSymbolLoader()
	loader.lookups["alpha"] = newTestSymbolLookup(&testPlugin{registerResult: plugin, reconfigureResult: plugin})
	host := NewForTest(loader)
	cfg := &config.Config{Plugins: config.PluginsConfig{Enabled: true, Dir: makePluginDir(t, "alpha"), Configs: enabledPluginConfigs("alpha")}}
	item := cfg.Plugins.Configs["alpha"]
	item.RequestInterceptors.Providers = []string{" CODEX "}
	cfg.Plugins.Configs["alpha"] = item
	host.ApplyConfig(context.Background(), cfg)
	req := pluginapi.RequestInterceptRequest{Provider: "antigravity", SourceFormat: "openai", Body: []byte(`{}`)}
	host.InterceptRequestAfterAuth(context.Background(), req)
	if calls != 0 {
		t.Fatal("initial filter did not apply")
	}
	item.RequestInterceptors.Providers = []string{"antigravity"}
	cfg.Plugins.Configs["alpha"] = item
	host.ApplyConfig(context.Background(), cfg)
	host.InterceptRequestAfterAuth(context.Background(), req)
	if calls != 1 {
		t.Fatalf("updated filter did not apply: calls=%d", calls)
	}
}
