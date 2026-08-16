package helps

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

const chromeFingerprintDialTimeout = 30 * time.Second

type chromeFingerprintDial struct {
	done chan struct{}
	conn *http2.ClientConn
	err  error
}

type chromeFingerprintRoundTripper struct {
	dialer proxy.Dialer
	http   *http.Transport

	mu      sync.Mutex
	conns   map[string]*http2.ClientConn
	dialing map[string]*chromeFingerprintDial
}

// NewChromeFingerprintHTTPClient creates an isolated HTTP client that uses the
// existing uTLS Chrome profile for chatgpt.com and standard protocol negotiation
// for other hosts. Certificates are verified before requests send credentials,
// and network deadlines are limited to connection setup.
func NewChromeFingerprintHTTPClient(proxyURL string) (*http.Client, error) {
	dialer, mode, errDialer := proxyutil.BuildDialer(strings.TrimSpace(proxyURL))
	if errDialer != nil {
		return nil, fmt.Errorf("build Chrome fingerprint proxy dialer: %w", errDialer)
	}
	if mode == proxyutil.ModeInherit || dialer == nil {
		dialer = proxy.Direct
	}

	transport := &chromeFingerprintRoundTripper{
		dialer:  dialer,
		http:    proxyutil.NewDirectTransport(),
		conns:   make(map[string]*http2.ClientConn),
		dialing: make(map[string]*chromeFingerprintDial),
	}
	transport.http.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialChromeFingerprintConnection(ctx, dialer, network, address)
	}
	return &http.Client{Transport: transport}, nil
}

func (t *chromeFingerprintRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, fmt.Errorf("Chrome fingerprint request URL is missing")
	}
	if !strings.EqualFold(request.URL.Scheme, "https") || !strings.EqualFold(request.URL.Hostname(), "chatgpt.com") {
		return t.http.RoundTrip(request)
	}

	conn, errConn := t.getOrCreateConnection(request.Context(), request.URL.Hostname(), request.URL.Port())
	if errConn != nil {
		return nil, errConn
	}
	response, errRoundTrip := conn.RoundTrip(request)
	if errRoundTrip == nil {
		return response, nil
	}

	key := connectionKey(request.URL.Hostname(), request.URL.Port())
	t.mu.Lock()
	if current := t.conns[key]; current == conn {
		delete(t.conns, key)
	}
	t.mu.Unlock()
	_ = conn.Close()
	return nil, errRoundTrip
}

func (t *chromeFingerprintRoundTripper) getOrCreateConnection(ctx context.Context, host, port string) (*http2.ClientConn, error) {
	key := connectionKey(host, port)
	for {
		t.mu.Lock()
		if current := t.conns[key]; current != nil {
			if current.CanTakeNewRequest() {
				t.mu.Unlock()
				return current, nil
			}
			delete(t.conns, key)
			retireChromeFingerprintConnection(current)
		}
		if pending := t.dialing[key]; pending != nil {
			t.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-pending.done:
				if pending.err != nil {
					return nil, pending.err
				}
				continue
			}
		}

		pending := &chromeFingerprintDial{done: make(chan struct{})}
		t.dialing[key] = pending
		t.mu.Unlock()

		pending.conn, pending.err = t.createConnection(ctx, host, port)
		t.mu.Lock()
		delete(t.dialing, key)
		if pending.err == nil {
			t.conns[key] = pending.conn
		}
		close(pending.done)
		t.mu.Unlock()
		return pending.conn, pending.err
	}
}

func (t *chromeFingerprintRoundTripper) createConnection(ctx context.Context, host, port string) (*http2.ClientConn, error) {
	if strings.TrimSpace(host) == "" {
		return nil, fmt.Errorf("Chrome fingerprint host is empty")
	}
	if port == "" {
		port = "443"
	}
	address := net.JoinHostPort(host, port)

	setupCtx, cancelSetup := chromeFingerprintSetupContext(ctx)
	defer cancelSetup()
	rawConn, errDial := dialChromeFingerprintConnection(setupCtx, t.dialer, "tcp", address)
	if errDial != nil {
		return nil, fmt.Errorf("Chrome fingerprint dial failed: %w", errDial)
	}

	tlsConn := tls.UClient(rawConn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}, tls.HelloChrome_Auto)
	if errHandshake := tlsConn.HandshakeContext(setupCtx); errHandshake != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("Chrome fingerprint TLS handshake failed: %w", errHandshake)
	}
	if tlsConn.ConnectionState().NegotiatedProtocol != http2.NextProtoTLS {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("Chrome fingerprint upstream did not negotiate HTTP/2")
	}

	h2Transport := &http2.Transport{}
	h2Conn, errHTTP2 := h2Transport.NewClientConn(tlsConn)
	if errHTTP2 != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("create Chrome fingerprint HTTP/2 connection: %w", errHTTP2)
	}
	return h2Conn, nil
}

func chromeFingerprintSetupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, chromeFingerprintDialTimeout)
}

func dialChromeFingerprintConnection(ctx context.Context, dialer proxy.Dialer, network, address string) (net.Conn, error) {
	setupCtx, cancelSetup := chromeFingerprintSetupContext(ctx)
	defer cancelSetup()
	if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
		return contextDialer.DialContext(setupCtx, network, address)
	}
	return dialer.Dial(network, address)
}

func (t *chromeFingerprintRoundTripper) CloseIdleConnections() {
	if t == nil {
		return
	}
	t.http.CloseIdleConnections()
	t.mu.Lock()
	connections := make([]*http2.ClientConn, 0, len(t.conns))
	for key, connection := range t.conns {
		connections = append(connections, connection)
		delete(t.conns, key)
	}
	t.mu.Unlock()
	for _, connection := range connections {
		retireChromeFingerprintConnection(connection)
	}
}

func retireChromeFingerprintConnection(connection *http2.ClientConn) {
	if connection == nil {
		return
	}
	connection.SetDoNotReuse()
	go func() {
		_ = connection.Shutdown(context.Background())
	}()
}

func connectionKey(host, port string) string {
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(strings.ToLower(strings.TrimSpace(host)), port)
}
