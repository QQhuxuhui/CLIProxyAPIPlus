package config

import "testing"

// TestWebsocketAuthEnabledDefaultsToTrue asserts the tri-state secure default:
// a nil pointer (ws-auth key omitted) means authentication is enabled, an
// explicit false disables it, and an explicit true keeps it enabled. The
// default-true case fails if WebsocketAuth is reverted to a plain bool that
// zero-defaults to false.
func TestWebsocketAuthEnabledDefaultsToTrue(t *testing.T) {
	falseVal := false
	trueVal := true

	cases := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{name: "nil config", cfg: nil, want: true},
		{name: "unset pointer defaults enabled", cfg: &Config{}, want: true},
		{name: "explicit false disables", cfg: &Config{WebsocketAuth: &falseVal}, want: false},
		{name: "explicit true enables", cfg: &Config{WebsocketAuth: &trueVal}, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WebsocketAuthEnabled(tc.cfg); got != tc.want {
				t.Fatalf("WebsocketAuthEnabled(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
