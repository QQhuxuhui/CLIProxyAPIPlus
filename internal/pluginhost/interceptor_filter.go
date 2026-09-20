package pluginhost

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func normalizeRequestInterceptorFilter(filter config.RequestInterceptorFilter) config.RequestInterceptorFilter {
	normalize := func(values []string) []string {
		if len(values) == 0 {
			return nil
		}
		out := make([]string, len(values))
		for i, value := range values {
			out[i] = strings.ToLower(strings.TrimSpace(value))
		}
		return out
	}
	filter.Providers = normalize(filter.Providers)
	filter.SourceFormats = normalize(filter.SourceFormats)
	filter.TargetFormats = normalize(filter.TargetFormats)
	return filter
}

func interceptorAllows(values []string, value string) bool {
	if len(values) == 0 {
		return true
	}
	value = strings.ToLower(strings.TrimSpace(value))
	for _, allowed := range values {
		if allowed != "" && allowed == value {
			return true
		}
	}
	return false
}

func requestInterceptorMatches(filter config.RequestInterceptorFilter, req pluginapi.RequestInterceptRequest) bool {
	if !interceptorAllows(filter.SourceFormats, req.SourceFormat) {
		return false
	}
	if req.ToFormat != "" && !interceptorAllows(filter.TargetFormats, req.ToFormat) {
		return false
	}
	if req.Provider != "" {
		return interceptorAllows(filter.Providers, req.Provider)
	}
	if len(req.Providers) == 0 || len(filter.Providers) == 0 {
		return true
	}
	for _, provider := range req.Providers {
		if interceptorAllows(filter.Providers, provider) {
			return true
		}
	}
	return false
}

// HasRequestInterceptorsFor lets callers skip payload preparation when every
// interceptor is filtered out. Unknown routing dimensions remain eligible.
func (h *Host) HasRequestInterceptorsFor(req pluginapi.RequestInterceptRequest, skipPluginID string) bool {
	skipPluginID = strings.TrimSpace(skipPluginID)
	for _, record := range h.activeRecords() {
		if record.id != skipPluginID && !h.isPluginFused(record.id) && record.plugin.Capabilities.RequestInterceptor != nil && requestInterceptorMatches(record.requestInterceptors, req) {
			return true
		}
	}
	return false
}
