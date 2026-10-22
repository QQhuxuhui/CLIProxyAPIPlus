package executor

import (
	"net/http"
	"testing"
	"time"
)

func TestCodexHTTPRateLimitReset(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	for _, tc := range []struct {
		name, header string
		want         time.Duration
	}{
		{"seconds", "7", 7 * time.Second},
		{"date", now.Add(9 * time.Second).Format(http.TimeFormat), 9 * time.Second},
		{"missing", "", time.Minute},
		{"invalid", "bogus", time.Minute},
		{"zero", "0", 0},
		{"past", now.Add(-time.Second).Format(http.TimeFormat), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := newCodexStatusErrWithHeadersAt(429, []byte(`{"detail":"Rate limit exceeded"}`), http.Header{"Retry-After": {tc.header}}, true, now)
			if err.quota == nil || err.quota.ReasonCode != "RATE_LIMIT_EXCEEDED" || !err.quota.ResetAt.Equal(now.Add(tc.want)) || !err.credentialScoped {
				t.Fatalf("unexpected rate limit error: %+v, quota %+v", err, err.quota)
			}
		})
	}
	err := newCodexStatusErrWithHeadersAt(429, []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`), http.Header{"Retry-After": {"7"}}, false, now)
	if err.quota != nil || err.retryAfter == nil || *err.retryAfter != time.Hour {
		t.Fatalf("quota behavior changed: %+v", err)
	}
}
