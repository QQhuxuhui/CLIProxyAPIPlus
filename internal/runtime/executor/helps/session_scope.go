package helps

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// ScopeSessionKeyToCaller binds a client-derived session key to the authenticated
// caller so a guessed or reused session id from a different tenant maps to a
// different cache entry. It returns "" when there is no session key or no
// identifiable caller (fail closed: no cross-tenant cache, at worst a cache miss).
func ScopeSessionKeyToCaller(ctx context.Context, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	caller := strings.TrimSpace(APIKeyFromContext(ctx))
	if caller == "" {
		return ""
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:session-scope:"+caller+"\x00"+raw)).String()
}
