package executor

import "github.com/router-for-me/CLIProxyAPI/v6/internal/config"

// ApplyMasqueradeTraceConfig applies masquerade trace settings from the config to the global trace store.
func ApplyMasqueradeTraceConfig(cfg *config.Config) {
	store := GetGlobalTraceStore()
	if cfg == nil {
		store.SetEnabled(false)
		store.SetMaxSize(DefaultMaxTraceRecords)
		return
	}
	store.SetMaxSize(cfg.MasqueradeTrace.MaxRecords)
	store.SetEnabled(cfg.MasqueradeTrace.Enable)
}

