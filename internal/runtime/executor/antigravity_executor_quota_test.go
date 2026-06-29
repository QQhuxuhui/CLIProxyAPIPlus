package executor

import (
	"net/http"
	"testing"
	"time"
)

func TestNewAntigravityStatusErr_PopulatesQuotaDetail(t *testing.T) {
	body := []byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[` +
		`{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED",` +
		`"metadata":{"model":"gemini-pro-agent","quotaResetTimeStamp":"2026-07-03T13:11:31Z"}},` +
		`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"351085s"}]}}`)
	err := newAntigravityStatusErr(http.StatusTooManyRequests, body)
	detail, ok := err.QuotaDetail()
	if !ok {
		t.Fatal("expected QuotaDetail ok=true")
	}
	if detail.Model != "gemini-pro-agent" || detail.ReasonCode != "QUOTA_EXHAUSTED" {
		t.Errorf("detail = %+v", detail)
	}
	want, _ := time.Parse(time.RFC3339, "2026-07-03T13:11:31Z")
	if !detail.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %v, want %v", detail.ResetAt, want)
	}
}

func TestStatusErr_NoQuotaDetailByDefault(t *testing.T) {
	err := statusErr{code: 500, msg: "boom"}
	if _, ok := err.QuotaDetail(); ok {
		t.Error("expected ok=false when quota unset")
	}
}
