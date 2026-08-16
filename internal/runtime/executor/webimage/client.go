package webimage

import (
	"fmt"
	"net/http/cookiejar"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

var webImageIdentityNamespace = uuid.MustParse("b4d46bd5-f8b5-4a4f-a445-8957f348b277")

func buildSession(cfg *config.Config, credentials Credentials, signature string, now time.Time) (*Session, error) {
	stdClient, errClient := helps.NewChromeFingerprintHTTPClient(resolveProxyURL(cfg, credentials))
	if errClient != nil {
		return nil, fmt.Errorf("build web image HTTP client: %w", errClient)
	}
	jar, errJar := cookiejar.New(nil)
	if errJar != nil {
		return nil, fmt.Errorf("build web image cookie jar: %w", errJar)
	}
	stdClient.Jar = jar

	authID := strings.TrimSpace(credentials.AuthID)
	identitySeed := authID
	if identitySeed == "" {
		identitySeed = "unidentified"
	}
	userAgent := config.DefaultWebImageUserAgent
	clientVersion := config.DefaultWebImageClientVersion
	if cfg != nil {
		if configured := strings.TrimSpace(cfg.WebImageUserAgent); configured != "" {
			userAgent = configured
		}
		clientVersion = strings.TrimSpace(cfg.WebImageClientVersion)
	}

	return &Session{
		Client: stdClient,
		Identity: Identity{
			DeviceID:  uuid.NewSHA1(webImageIdentityNamespace, []byte("device:"+identitySeed)).String(),
			SessionID: uuid.NewSHA1(webImageIdentityNamespace, []byte("session:"+identitySeed)).String(),
		},
		UserAgent:     userAgent,
		ClientVersion: clientVersion,
		signature:     signature,
		createdAt:     now,
	}, nil
}

func resolveProxyURL(cfg *config.Config, credentials Credentials) string {
	if proxyURL := strings.TrimSpace(credentials.ProxyURL); proxyURL != "" {
		return proxyURL
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.ProxyURL)
	}
	return ""
}

func sessionSignature(cfg *config.Config, credentials Credentials) string {
	userAgent := config.DefaultWebImageUserAgent
	clientVersion := config.DefaultWebImageClientVersion
	if cfg != nil {
		if configured := strings.TrimSpace(cfg.WebImageUserAgent); configured != "" {
			userAgent = configured
		}
		clientVersion = strings.TrimSpace(cfg.WebImageClientVersion)
	}
	return strings.Join([]string{resolveProxyURL(cfg, credentials), userAgent, clientVersion}, "\x00")
}
