package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// Backport of upstream c33a33e1 + 516ec3a0 onto the fork's pre-split layout.

var (
	antigravityBaseTransport = defaultAntigravityBaseTransport()
	antigravityTransports    = helps.NewTransportCache[antigravityTransportKey](antigravityTransportCacheCapacity)
)

const (
	antigravityTransportCacheCapacity  = 8192
	antigravityMaxIdleConnsPerHost     = 100
	antigravityIdleConnTimeout         = 10 * time.Minute
	antigravityAnonymousTransportScope = "anonymous"
)

type antigravityTransportKey struct {
	credential string
	proxy      string
	base       *http.Transport
}

func defaultAntigravityBaseTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok && transport != nil {
		return transport
	}
	return &http.Transport{}
}

func applyAntigravityPoolLimits(transport *http.Transport) {
	if transport == nil {
		return
	}
	if transport.MaxIdleConnsPerHost >= 0 && transport.MaxIdleConnsPerHost < antigravityMaxIdleConnsPerHost {
		transport.MaxIdleConnsPerHost = antigravityMaxIdleConnsPerHost
	}
	if transport.MaxIdleConns > 0 && transport.MaxIdleConns < transport.MaxIdleConnsPerHost {
		transport.MaxIdleConns = transport.MaxIdleConnsPerHost
	}
	if transport.IdleConnTimeout > 0 && transport.IdleConnTimeout < antigravityIdleConnTimeout {
		transport.IdleConnTimeout = antigravityIdleConnTimeout
	}
}

func antigravityHTTP11Transport(auth *cliproxyauth.Auth, base *http.Transport) *http.Transport {
	if base == nil {
		return nil
	}
	key := antigravityTransportKey{
		credential: antigravityTransportScope(auth),
		base:       base,
	}
	transport, errGet := antigravityTransports.Get(key, func() (*http.Transport, error) {
		return cloneTransportWithHTTP11(base), nil
	})
	if errGet != nil {
		log.Debugf("antigravity executor: cache HTTP/1.1 transport failed: %v", errGet)
		return cloneTransportWithHTTP11(base)
	}
	return transport
}

func antigravityProxiedHTTP11Transport(auth *cliproxyauth.Auth, proxyURL string) *http.Transport {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil
	}
	key := antigravityTransportKey{
		credential: antigravityTransportScope(auth),
		proxy:      proxyURL,
	}
	transport, errGet := antigravityTransports.Get(key, func() (*http.Transport, error) {
		base, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
		if errBuild != nil {
			return nil, errBuild
		}
		if base == nil {
			return nil, fmt.Errorf("antigravity executor: proxy setting produced no transport")
		}
		return cloneTransportWithHTTP11(base), nil
	})
	if errGet != nil {
		return nil
	}
	return transport
}

func antigravityTransportScope(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return antigravityAnonymousTransportScope
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		return "id:" + id
	}
	if auth.Attributes != nil {
		if path := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributePath]); path != "" {
			return "path:" + path
		}
		if source := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributeSource]); source != "" {
			return "source:" + source
		}
	}
	if refresh := strings.TrimSpace(metaStringValue(auth.Metadata, "refresh_token")); refresh != "" {
		return antigravityCredentialScope("refresh:", refresh)
	}
	if access := strings.TrimSpace(metaStringValue(auth.Metadata, "access_token")); access != "" {
		return antigravityCredentialScope("token:", access)
	}
	return antigravityAnonymousTransportScope
}

func antigravityCredentialScope(prefix, secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return prefix + hex.EncodeToString(digest[:8])
}

func antigravityProxyURL(cfg *config.Config, auth *cliproxyauth.Auth) string {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.ProxyURL)
	}
	return ""
}
