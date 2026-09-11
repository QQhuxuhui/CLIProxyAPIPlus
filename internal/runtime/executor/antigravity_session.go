package executor

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func antigravitySessionForRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (string, string, bool) {
	payload := opts.OriginalRequest
	if len(payload) == 0 {
		payload = req.Payload
	}
	rawStableKey := cliproxyauth.ExtractSessionID(opts.Headers, payload, opts.Metadata)
	if rawStableKey == "" || strings.HasPrefix(rawStableKey, "msg:") || strings.HasPrefix(rawStableKey, "clientreq:") || strings.HasPrefix(rawStableKey, "user:") {
		return "", generateSessionID(), false
	}
	stableKey := antigravityScopedStableSessionKey(rawStableKey, helps.APIKeyFromContext(ctx))
	return stableKey, antigravityStableUpstreamSessionID(stableKey), true
}

func antigravityScopedStableSessionKey(rawStableKey, caller string) string {
	sum := sha256.Sum256([]byte("cli-proxy-api:antigravity:stable-session:v1\x00" + strings.TrimSpace(caller) + "\x00" + rawStableKey))
	return "v1:" + hex.EncodeToString(sum[:])
}

func antigravityStableUpstreamSessionID(stableKey string) string {
	sum := sha256.Sum256([]byte("cli-proxy-api:antigravity:session:v1\x00" + stableKey))
	value := int64(binary.BigEndian.Uint64(sum[:8])) & 0x7FFFFFFFFFFFFFFF
	return "-" + strconv.FormatInt(value, 10)
}

func sessionLockKey(upstreamSessionID string, stable bool) string {
	if !stable {
		return ""
	}
	return upstreamSessionID
}
