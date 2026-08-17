package webimage

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestResolveProxyURLPrefersAuthProxy(t *testing.T) {
	cfg := &config.Config{}
	cfg.ProxyURL = "http://global-proxy.example"

	if got := resolveProxyURL(cfg, Credentials{ProxyURL: "socks5://auth-proxy.example"}); got != "socks5://auth-proxy.example" {
		t.Fatalf("resolveProxyURL() = %q", got)
	}
	if got := resolveProxyURL(cfg, Credentials{}); got != "http://global-proxy.example" {
		t.Fatalf("resolveProxyURL() fallback = %q", got)
	}
}

func TestSessionPoolIsolatesAccountsAndKeepsStableIDs(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	pool := NewSessionPool(cfg)
	t.Cleanup(pool.Close)

	first, errFirst := pool.Get(Credentials{AuthID: "auth-a"})
	if errFirst != nil {
		t.Fatalf("Get(auth-a) error = %v", errFirst)
	}
	again, errAgain := pool.Get(Credentials{AuthID: "auth-a"})
	if errAgain != nil {
		t.Fatalf("Get(auth-a again) error = %v", errAgain)
	}
	second, errSecond := pool.Get(Credentials{AuthID: "auth-b"})
	if errSecond != nil {
		t.Fatalf("Get(auth-b) error = %v", errSecond)
	}

	if first != again {
		t.Fatal("same auth ID did not reuse its session")
	}
	if first == second || first.Client == second.Client {
		t.Fatal("different auth IDs shared a client session")
	}
	if first.Identity.DeviceID == "" || first.Identity.SessionID == "" || first.Identity.DeviceID == second.Identity.DeviceID || first.Identity.SessionID == second.Identity.SessionID {
		t.Fatalf("session IDs are not stable and isolated: first=%+v second=%+v", first.Identity, second.Identity)
	}
	if first.Client.Timeout != 0 {
		t.Fatalf("client timeout = %v, want zero", first.Client.Timeout)
	}
}

func TestSessionPoolCookieJarsAreIsolated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if value := r.URL.Query().Get("set"); value != "" {
			http.SetCookie(w, &http.Cookie{Name: "account", Value: value, Path: "/"})
		}
		cookie, errCookie := r.Cookie("account")
		if errCookie == nil {
			_, _ = fmt.Fprint(w, cookie.Value)
		}
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	pool := NewSessionPool(cfg)
	t.Cleanup(pool.Close)
	first, errFirst := pool.Get(Credentials{AuthID: "auth-a"})
	if errFirst != nil {
		t.Fatalf("Get(auth-a) error = %v", errFirst)
	}
	second, errSecond := pool.Get(Credentials{AuthID: "auth-b"})
	if errSecond != nil {
		t.Fatalf("Get(auth-b) error = %v", errSecond)
	}

	respSet, errSet := first.Client.Get(server.URL + "?set=a")
	if errSet != nil {
		t.Fatalf("first client set cookie: %v", errSet)
	}
	_ = respSet.Body.Close()
	respFirst, errFirstGet := first.Client.Get(server.URL)
	if errFirstGet != nil {
		t.Fatalf("first client get cookie: %v", errFirstGet)
	}
	defer respFirst.Body.Close()
	if cookie := first.Client.Jar.Cookies(respFirst.Request.URL); len(cookie) != 1 || cookie[0].Value != "a" {
		t.Fatalf("first client cookies = %+v", cookie)
	}
	if cookie := second.Client.Jar.Cookies(respFirst.Request.URL); len(cookie) != 0 {
		t.Fatalf("second client inherited cookies: %+v", cookie)
	}
}

func TestSessionPoolRebuildsWhenFingerprintConfigChanges(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	pool := NewSessionPool(cfg)
	t.Cleanup(pool.Close)

	first, errFirst := pool.Get(Credentials{AuthID: "auth-a"})
	if errFirst != nil {
		t.Fatalf("Get() error = %v", errFirst)
	}
	cfg.WebImageUserAgent = "changed-agent"
	second, errSecond := pool.Get(Credentials{AuthID: "auth-a"})
	if errSecond != nil {
		t.Fatalf("Get() after config change error = %v", errSecond)
	}
	if first == second {
		t.Fatal("session was not rebuilt after fingerprint config changed")
	}
	cfg.WebImageClientBuild = "changed-build"
	third, errThird := pool.Get(Credentials{AuthID: "auth-a"})
	if errThird != nil {
		t.Fatalf("Get() after client build change error = %v", errThird)
	}
	if second == third {
		t.Fatal("session was not rebuilt after client build config changed")
	}
}

func TestSessionClientVerifiesTLSCertificates(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	pool := NewSessionPool(cfg)
	t.Cleanup(pool.Close)
	session, errGet := pool.Get(Credentials{AuthID: "auth-a"})
	if errGet != nil {
		t.Fatalf("Get() error = %v", errGet)
	}

	if _, errRequest := session.Client.Get(server.URL); errRequest == nil {
		t.Fatal("self-signed TLS certificate was accepted")
	}
}
