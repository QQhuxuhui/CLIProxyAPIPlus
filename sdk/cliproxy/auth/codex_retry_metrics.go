package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/tidwall/gjson"
	"sort"
	"sync/atomic"
)

type codexOverloadMetricKey struct{ authID, exit, kind string }

// CodexOverloadPoint separates upstream capacity failures from credential health.
// Exit is an opaque proxy-route fingerprint; it never exposes proxy credentials.
type CodexOverloadPoint struct {
	AuthID string `json:"auth_id"`
	Exit   string `json:"exit"`
	Kind   string `json:"kind"`
	Count  uint64 `json:"count"`
}

func (m *Manager) recordCodexOverload(result Result) {
	m.mu.RLock()
	a := m.auths[result.AuthID]
	proxy := ""
	if a != nil {
		proxy = a.ProxyURL
	}
	m.mu.RUnlock()
	if a == nil {
		return
	}
	if proxy == "" {
		if cfg := m.runtimeConfigSnapshot(); cfg != nil {
			proxy = cfg.ProxyURL
		}
	}
	exit := "direct"
	if proxy != "" {
		sum := sha256.Sum256([]byte(proxy))
		exit = hex.EncodeToString(sum[:8])
	}
	kind := "overload"
	if result.Error != nil {
		code := gjson.Get(result.Error.Message, "error.code").String()
		typ := gjson.Get(result.Error.Message, "error.type").String()
		switch {
		case code == "server_is_overloaded":
			kind = code
		case typ == "service_unavailable_error":
			kind = typ
		case result.Error.HTTPStatus == 503:
			kind = "http_503"
		case result.Error.HTTPStatus == 529:
			kind = "http_529"
		default:
			kind = "stream_overload"
		}
	}
	key := codexOverloadMetricKey{result.AuthID, exit, kind}
	value, ok := m.codexOverloads.Load(key)
	if !ok {
		value, _ = m.codexOverloads.LoadOrStore(key, &atomic.Uint64{})
	}
	value.(*atomic.Uint64).Add(1)
}
func (m *Manager) codexOverloadSnapshot() []CodexOverloadPoint {
	out := make([]CodexOverloadPoint, 0)
	m.codexOverloads.Range(func(k, v any) bool {
		key := k.(codexOverloadMetricKey)
		out = append(out, CodexOverloadPoint{key.authID, key.exit, key.kind, v.(*atomic.Uint64).Load()})
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.AuthID != b.AuthID {
			return a.AuthID < b.AuthID
		}
		if a.Exit != b.Exit {
			return a.Exit < b.Exit
		}
		return a.Kind < b.Kind
	})
	return out
}
