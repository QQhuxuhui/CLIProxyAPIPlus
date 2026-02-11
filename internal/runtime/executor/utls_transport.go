package executor

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// UTLSTransport is an http.RoundTripper that uses utls for TLS connections
// to mimic specific client fingerprints (e.g., Node.js).
type UTLSTransport struct {
	// serverName is the SNI server name
	serverName string

	// proxyURL is the optional proxy URL
	proxyURL string

	// useCustomSpec controls whether to use custom ClientHelloSpec
	useCustomSpec bool

	// helloID is the utls ClientHelloID to use when not using custom spec
	helloID utls.ClientHelloID

	// dialer for TCP connections (may be through proxy)
	dialer proxy.Dialer

	// connections caches HTTP/2 connections for reuse
	connections map[string]*http2.ClientConn
	connMu      sync.Mutex

	// tlsConfig base TLS config
	tlsConfig *tls.Config
}

// UTLSTransportConfig holds configuration for creating a UTLSTransport.
type UTLSTransportConfig struct {
	ServerName    string
	ProxyURL      string
	UseCustomSpec bool
	TLSConfig     *tls.Config
}

// NewUTLSTransport creates a new UTLSTransport.
func NewUTLSTransport(cfg UTLSTransportConfig) (*UTLSTransport, error) {
	t := &UTLSTransport{
		serverName:    cfg.ServerName,
		proxyURL:      cfg.ProxyURL,
		useCustomSpec: cfg.UseCustomSpec,
		helloID:       GetNodeJS24HelloID(),
		connections:   make(map[string]*http2.ClientConn),
		tlsConfig:     cfg.TLSConfig,
	}

	// Set up dialer (direct or through proxy)
	if cfg.ProxyURL != "" {
		dialer, err := t.createProxyDialer(cfg.ProxyURL)
		if err != nil {
			return nil, err
		}
		t.dialer = dialer
	} else {
		t.dialer = proxy.Direct
	}

	return t, nil
}

// createProxyDialer creates a proxy.Dialer for the given proxy URL.
func (t *UTLSTransport) createProxyDialer(proxyURL string) (proxy.Dialer, error) {
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}

	switch parsed.Scheme {
	case "socks5":
		var auth *proxy.Auth
		if parsed.User != nil {
			auth = &proxy.Auth{
				User: parsed.User.Username(),
			}
			if password, ok := parsed.User.Password(); ok {
				auth.Password = password
			}
		}
		return proxy.SOCKS5("tcp", parsed.Host, auth, proxy.Direct)
	case "http", "https":
		// For HTTP proxies, we need to use CONNECT method
		// This is more complex; for now, fall back to direct + warning
		log.Warnf("HTTP proxy not fully supported with utls, using direct connection")
		return proxy.Direct, nil
	default:
		return nil, errors.New("unsupported proxy scheme: " + parsed.Scheme)
	}
}

// RoundTrip implements http.RoundTripper.
func (t *UTLSTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Only handle HTTPS requests
	if req.URL.Scheme != "https" {
		return nil, errors.New("utls transport only supports HTTPS")
	}

	host := req.URL.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "443")
	}

	// Try to reuse existing HTTP/2 connection
	t.connMu.Lock()
	conn, ok := t.connections[host]
	if ok && conn.CanTakeNewRequest() {
		t.connMu.Unlock()
		return conn.RoundTrip(req)
	}
	delete(t.connections, host)
	t.connMu.Unlock()

	// Create new connection
	tlsConn, err := t.dialTLS(req.Context(), host)
	if err != nil {
		return nil, err
	}

	// Check if HTTP/2 was negotiated
	if tlsConn.ConnectionState().NegotiatedProtocol == "h2" {
		// Set up HTTP/2 connection
		h2Transport := &http2.Transport{}
		h2Conn, err := h2Transport.NewClientConn(tlsConn)
		if err != nil {
			tlsConn.Close()
			return nil, err
		}

		// Cache the connection
		t.connMu.Lock()
		t.connections[host] = h2Conn
		t.connMu.Unlock()

		return h2Conn.RoundTrip(req)
	}

	// HTTP/1.1 fallback
	transport := &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return tlsConn, nil
		},
	}
	return transport.RoundTrip(req)
}

// dialTLS creates a TLS connection using utls.
func (t *UTLSTransport) dialTLS(ctx context.Context, addr string) (*utls.UConn, error) {
	// TCP connection (possibly through proxy)
	tcpConn, err := t.dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	// Set deadline from context
	if deadline, ok := ctx.Deadline(); ok {
		tcpConn.SetDeadline(deadline)
	}

	// Determine server name for SNI
	serverName := t.serverName
	if serverName == "" {
		host, _, _ := net.SplitHostPort(addr)
		serverName = host
	}

	// Create utls config
	utlsConfig := &utls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: false,
	}
	if t.tlsConfig != nil {
		utlsConfig.InsecureSkipVerify = t.tlsConfig.InsecureSkipVerify
		utlsConfig.RootCAs = t.tlsConfig.RootCAs
	}

	// Create utls connection
	tlsConn := utls.UClient(tcpConn, utlsConfig, t.helloID)

	// Apply custom spec if configured
	if t.useCustomSpec {
		spec := NewNodeJS24ClientHelloSpec(serverName)
		if err := tlsConn.ApplyPreset(spec); err != nil {
			tcpConn.Close()
			return nil, err
		}
	}

	// Perform handshake
	if err := tlsConn.Handshake(); err != nil {
		tcpConn.Close()
		return nil, err
	}

	// Clear deadline after handshake
	tcpConn.SetDeadline(time.Time{})

	return tlsConn, nil
}

// Close closes all cached connections.
func (t *UTLSTransport) Close() error {
	t.connMu.Lock()
	defer t.connMu.Unlock()

	for host, conn := range t.connections {
		conn.Close()
		delete(t.connections, host)
	}
	return nil
}

// CloseIdleConnections closes idle connections (for compatibility with http.Client).
func (t *UTLSTransport) CloseIdleConnections() {
	t.Close()
}
