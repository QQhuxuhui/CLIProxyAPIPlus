package wsrelay

import (
	"net/http/httptest"
	"testing"
)

// TestCheckOriginPolicy asserts the CSWSH hardening: requests without an Origin
// header (non-browser provider clients) are allowed, an Origin whose host
// matches the request Host is allowed, and a cross-host Origin is rejected. It
// fails against the previous blanket "return true" behavior for the cross-host
// case.
func TestCheckOriginPolicy(t *testing.T) {
	cases := []struct {
		name   string
		host   string
		origin string
		want   bool
	}{
		{name: "no origin allowed", host: "proxy.local:8317", origin: "", want: true},
		{name: "same host allowed", host: "proxy.local:8317", origin: "http://proxy.local:8317", want: true},
		{name: "cross host rejected", host: "proxy.local:8317", origin: "http://evil.example:8317", want: false},
		{name: "malformed origin rejected", host: "proxy.local:8317", origin: "http://%zz", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://"+tc.host+"/v1/ws", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if got := checkOrigin(req); got != tc.want {
				t.Fatalf("checkOrigin(host=%q, origin=%q) = %v, want %v", tc.host, tc.origin, got, tc.want)
			}
		})
	}
}
