package helps

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// NewCodexEgressHTTPClient returns the HTTP client for one Codex /responses attempt. It is the
// regular utls client, except that the egress pool may route the attempt through one of its
// proxies: retries after an upstream overload, and a share of first attempts while the direct
// route is overloaded. Credentials with their own proxy_url always keep it.
func NewCodexEgressHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, reporter *UsageReporter) *http.Client {
	client := NewUtlsHTTPClient(ctx, cfg, auth, 0)
	if cfg == nil || auth == nil || strings.TrimSpace(auth.ProxyURL) != "" {
		return client
	}
	pool := CodexEgressPools()
	pool.ApplyUnmanagedConfig(cfg.Codex.EgressPool)
	if !pool.Active() {
		return client
	}
	client.Transport = &codexEgressRoundTripper{
		pool:     pool,
		direct:   client.Transport,
		authKey:  auth.ID,
		retry:    cliproxyauth.CodexEgressRetryPending(ctx),
		reporter: reporter,
	}
	return client
}

type codexEgressRoundTripper struct {
	pool     *CodexEgressPool
	direct   http.RoundTripper
	authKey  string
	retry    bool
	reporter *UsageReporter
}

func (t *codexEgressRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || !strings.EqualFold(req.URL.Hostname(), "chatgpt.com") {
		return t.direct.RoundTrip(req)
	}
	d := t.pool.selectRoute(t.authKey, t.retry)
	for d.proxy != nil {
		px := d.proxy
		t.reporter.SetCodexEgress(d.group, px.rawURL)
		resp, err := px.transport.RoundTrip(req)
		if err == nil {
			t.pool.markConnectSuccess(px)
			resp.Body = &codexEgressBody{ReadCloser: resp.Body, release: func() { t.pool.release(px) }}
			return resp, nil
		}
		if !isCodexEgressConnectError(err) || req.Context().Err() != nil {
			t.pool.release(px)
			return nil, err
		}
		// The proxy failed before the request reached the upstream: cool the proxy, not the
		// credential, and try another proxy or the direct route with the same credential.
		d = t.pool.reselect(d, t.authKey, err)
		if req.GetBody != nil {
			body, errBody := req.GetBody()
			if errBody != nil {
				if d.proxy != nil {
					t.pool.release(d.proxy)
				}
				return nil, errBody
			}
			req = req.Clone(req.Context())
			req.Body = body
		}
	}
	switch d.group {
	case "":
		return t.direct.RoundTrip(req)
	case CodexEgressGroupShiftPool:
		d.group = CodexEgressGroupFirstDirect
	case CodexEgressGroupRetryPool:
		d.group = CodexEgressGroupRetryFallback
	}
	t.reporter.SetCodexEgress(d.group, CodexEgressRouteDirect)
	return t.direct.RoundTrip(req)
}

// codexEgressBody releases the proxy slot once the response body is closed.
type codexEgressBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *codexEgressBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
