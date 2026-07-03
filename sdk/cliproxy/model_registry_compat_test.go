package cliproxy

import "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"

// Compile-time check: the concrete registry must satisfy the public SDK
// interface with its v7-stable 2-arg SetModelQuotaExceeded signature.
var _ ModelRegistry = (*registry.ModelRegistry)(nil)
