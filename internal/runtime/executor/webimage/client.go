package webimage

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func buildSession(cfg *config.Config, credentials Credentials, signature string, now time.Time) (*Session, error) {
	stdClient := helps.NewUtlsHTTPClient(context.Background(), cfg, &cliproxyauth.Auth{ProxyURL: credentials.ProxyURL}, 0)
	jar, errJar := cookiejar.New(nil)
	if errJar != nil {
		return nil, fmt.Errorf("build web image cookie jar: %w", errJar)
	}
	stdClient.Jar = jar
	stdClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	authID := strings.TrimSpace(credentials.AuthID)
	identitySeed := authID
	if identitySeed == "" {
		identitySeed = "unidentified"
	}
	userAgent := config.DefaultWebImageUserAgent
	clientVersion := config.DefaultWebImageClientVersion
	clientBuild := config.DefaultWebImageClientBuild
	if cfg != nil {
		if configured := strings.TrimSpace(cfg.WebImageUserAgent); configured != "" {
			userAgent = configured
		}
		clientVersion = strings.TrimSpace(cfg.WebImageClientVersion)
		clientBuild = strings.TrimSpace(cfg.WebImageClientBuild)
	}

	return &Session{
		Client: stdClient,
		Identity: Identity{
			DeviceID:  uuid.NewSHA1(uuid.NameSpaceOID, []byte(identitySeed)).String(),
			SessionID: uuid.NewString(),
		},
		UserAgent:     userAgent,
		ClientVersion: clientVersion,
		ClientBuild:   clientBuild,
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
	clientBuild := config.DefaultWebImageClientBuild
	if cfg != nil {
		if configured := strings.TrimSpace(cfg.WebImageUserAgent); configured != "" {
			userAgent = configured
		}
		clientVersion = strings.TrimSpace(cfg.WebImageClientVersion)
		clientBuild = strings.TrimSpace(cfg.WebImageClientBuild)
	}
	return strings.Join([]string{resolveProxyURL(cfg, credentials), userAgent, clientVersion, clientBuild}, "\x00")
}
