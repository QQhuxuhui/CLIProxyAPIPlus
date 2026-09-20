package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPluginRequestInterceptorFilterRoundTrip(t *testing.T) {
	input := []byte("plugins:\n  configs:\n    fixture:\n      enabled: true\n      request-interceptors:\n        providers: [codex]\n        source-formats: [openai, openai-response]\n        target-formats: [codex]\n      plugin-owned: unchanged\n")
	var first Config
	if err := yaml.Unmarshal(input, &first); err != nil {
		t.Fatal(err)
	}
	filter := first.Plugins.Configs["fixture"].RequestInterceptors
	if !reflect.DeepEqual(filter.Providers, []string{"codex"}) || len(filter.SourceFormats) != 2 || len(filter.TargetFormats) != 1 {
		t.Fatalf("bad filter: %+v", filter)
	}
	raw, err := yaml.Marshal(first.Plugins.Configs["fixture"])
	if err != nil {
		t.Fatal(err)
	}
	var second PluginInstanceConfig
	if err := yaml.Unmarshal(raw, &second); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(filter, second.RequestInterceptors) {
		t.Fatalf("round trip changed filter: %+v", second.RequestInterceptors)
	}
	if err := yaml.Unmarshal([]byte("enabled: true\n"), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.RequestInterceptors.Providers) != 0 {
		t.Fatal("reloading kept stale filters")
	}
}
