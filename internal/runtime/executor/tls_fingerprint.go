package executor

import (
	tls "github.com/refraction-networking/utls"
)

// Node.js v24.13.0 TLS fingerprint configuration
//
// IMPORTANT: The following values are based on Node.js v22 and serve as initial placeholders.
// They need to be verified and updated through packet capture analysis:
//
// Research steps to obtain precise v24.13.0 fingerprint:
// 1. Install Node.js v24.13.0: nvm install 24.13.0 && nvm use 24.13.0
// 2. Create test script:
//    const https = require('https');
//    https.get('https://api.anthropic.com', (res) => console.log('Status:', res.statusCode));
// 3. Capture with Wireshark: tcp.port == 443 and ssl.handshake.type == 1
// 4. Extract cipher suites and extension order from ClientHello
// 5. Verify JA3 fingerprint at https://ja3er.com/json
// 6. Update the constants below
//
// TODO(security): Verify and update these values for Node.js v24.13.0

// nodeJS24CipherSuites defines the TLS cipher suite order for Node.js v24.
// Based on OpenSSL 3.x defaults used by Node.js.
var nodeJS24CipherSuites = []uint16{
	// TLS 1.3 cipher suites (AEAD only)
	tls.TLS_AES_128_GCM_SHA256,        // 0x1301
	tls.TLS_AES_256_GCM_SHA384,        // 0x1302
	tls.TLS_CHACHA20_POLY1305_SHA256,  // 0x1303

	// TLS 1.2 cipher suites
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, // 0xC02B
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,   // 0xC02F
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, // 0xC02C
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,   // 0xC030
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,  // 0xCCA9
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,    // 0xCCA8
}

// nodeJS24SupportedGroups defines the supported elliptic curve groups.
var nodeJS24SupportedGroups = []tls.CurveID{
	tls.X25519,    // Most preferred
	tls.CurveP256, // secp256r1
	tls.CurveP384, // secp384r1
}

// nodeJS24SignatureSchemes defines the supported signature algorithms.
var nodeJS24SignatureSchemes = []tls.SignatureScheme{
	tls.ECDSAWithP256AndSHA256,
	tls.ECDSAWithP384AndSHA384,
	tls.ECDSAWithP521AndSHA512,
	tls.PSSWithSHA256,
	tls.PSSWithSHA384,
	tls.PSSWithSHA512,
	tls.PKCS1WithSHA256,
	tls.PKCS1WithSHA384,
	tls.PKCS1WithSHA512,
}

// nodeJS24PointFormats defines the EC point formats (uncompressed only for modern TLS).
var nodeJS24PointFormats = []byte{0} // uncompressed

// NewNodeJS24ClientHelloSpec creates a ClientHelloSpec mimicking Node.js v24.13.0.
// This is used with utls to produce a TLS fingerprint matching Node.js.
func NewNodeJS24ClientHelloSpec(serverName string) *tls.ClientHelloSpec {
	return &tls.ClientHelloSpec{
		TLSVersMin: tls.VersionTLS12,
		TLSVersMax: tls.VersionTLS13,
		CipherSuites: append([]uint16(nil), nodeJS24CipherSuites...),
		CompressionMethods: []byte{0}, // null compression only
		Extensions: []tls.TLSExtension{
			// SNI
			&tls.SNIExtension{ServerName: serverName},

			// Extended master secret
			&tls.ExtendedMasterSecretExtension{},

			// Renegotiation info
			&tls.RenegotiationInfoExtension{Renegotiation: tls.RenegotiateOnceAsClient},

			// Supported groups (elliptic curves)
			&tls.SupportedCurvesExtension{Curves: append([]tls.CurveID(nil), nodeJS24SupportedGroups...)},

			// EC point formats
			&tls.SupportedPointsExtension{SupportedPoints: append([]byte(nil), nodeJS24PointFormats...)},

			// Session ticket
			&tls.SessionTicketExtension{},

			// ALPN (Application Layer Protocol Negotiation)
			&tls.ALPNExtension{AlpnProtocols: []string{"h2", "http/1.1"}},

			// Status request (OCSP stapling)
			&tls.StatusRequestExtension{},

			// Signature algorithms
			&tls.SignatureAlgorithmsExtension{
				SupportedSignatureAlgorithms: append([]tls.SignatureScheme(nil), nodeJS24SignatureSchemes...),
			},

			// Signed certificate timestamp
			&tls.SCTExtension{},

			// Key share (for TLS 1.3)
			&tls.KeyShareExtension{KeyShares: []tls.KeyShare{
				{Group: tls.X25519},
			}},

			// PSK key exchange modes (for TLS 1.3)
			&tls.PSKKeyExchangeModesExtension{Modes: []uint8{tls.PskModeDHE}},

			// Supported versions
			&tls.SupportedVersionsExtension{Versions: []uint16{
				tls.VersionTLS13,
				tls.VersionTLS12,
			}},

			// Compress certificate (RFC 8879)
			&tls.UtlsCompressCertExtension{Algorithms: []tls.CertCompressionAlgo{
				tls.CertCompressionBrotli,
			}},

			// Padding (to avoid fingerprinting based on ClientHello length)
			&tls.UtlsPaddingExtension{GetPaddingLen: tls.BoringPaddingStyle},
		},
	}
}

// GetNodeJS24HelloID returns a utls.ClientHelloID that can be used directly.
// This is an alternative to using ClientHelloSpec for simpler cases.
func GetNodeJS24HelloID() tls.ClientHelloID {
	// Use Chrome as a reasonable approximation until exact Node.js fingerprint is verified
	// Node.js uses OpenSSL which has similar characteristics to Chrome's BoringSSL
	return tls.HelloChrome_Auto
}

// TLSFingerprintConfig holds configuration for TLS fingerprinting.
type TLSFingerprintConfig struct {
	// Enabled controls whether TLS fingerprinting is active.
	Enabled bool

	// UseCustomSpec uses the custom ClientHelloSpec instead of built-in presets.
	UseCustomSpec bool

	// ServerName is the SNI server name to use.
	ServerName string
}

// DefaultTLSFingerprintConfig returns the default TLS fingerprint configuration.
func DefaultTLSFingerprintConfig() TLSFingerprintConfig {
	return TLSFingerprintConfig{
		Enabled:       false, // Disabled by default until fingerprint is verified
		UseCustomSpec: false,
		ServerName:    "api.anthropic.com",
	}
}
