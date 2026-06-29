package helps

import (
	"testing"
	"time"
)

const proAgentBody = `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[` +
	`{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED","domain":"cloudcode-pa.googleapis.com",` +
	`"metadata":{"model":"gemini-pro-agent","quotaResetDelay":"97h31m25.480993691s","quotaResetTimeStamp":"2026-07-03T13:11:31Z","uiMessage":"true"}},` +
	`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"351085.480993691s"}]}}`

const flashImageBody = `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[` +
	`{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED","domain":"cloudcode-pa.googleapis.com",` +
	`"metadata":{"model":"gemini-3.1-flash-image","quotaResetDelay":"2h4m57.376883148s","quotaResetTimeStamp":"2026-06-29T13:54:35Z"}},` +
	`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"7497.376883148s"}]}}`

func TestParseAntigravityQuota_ProAgent(t *testing.T) {
	detail, ok := ParseAntigravityQuota([]byte(proAgentBody))
	if !ok {
		t.Fatal("expected ok=true")
	}
	if detail.Model != "gemini-pro-agent" {
		t.Errorf("Model = %q, want gemini-pro-agent", detail.Model)
	}
	if detail.ReasonCode != "QUOTA_EXHAUSTED" {
		t.Errorf("ReasonCode = %q, want QUOTA_EXHAUSTED", detail.ReasonCode)
	}
	want, _ := time.Parse(time.RFC3339, "2026-07-03T13:11:31Z")
	if !detail.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %v, want %v", detail.ResetAt, want)
	}
	if detail.ResetDelay == nil {
		t.Fatal("ResetDelay = nil, want retryDelay duration")
	}
	// RecoverAt 应优先用绝对 ResetAt
	now := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	if got := detail.RecoverAt(now); !got.Equal(want) {
		t.Errorf("RecoverAt = %v, want %v", got, want)
	}
}

func TestParseAntigravityQuota_FlashImage(t *testing.T) {
	detail, ok := ParseAntigravityQuota([]byte(flashImageBody))
	if !ok {
		t.Fatal("expected ok=true")
	}
	if detail.Model != "gemini-3.1-flash-image" {
		t.Errorf("Model = %q", detail.Model)
	}
	want, _ := time.Parse(time.RFC3339, "2026-06-29T13:54:35Z")
	if !detail.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %v, want %v", detail.ResetAt, want)
	}
}

func TestParseAntigravityQuota_AbsoluteOnly_NoRetryInfo(t *testing.T) {
	body := `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[` +
		`{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED",` +
		`"metadata":{"model":"gemini-pro-agent","quotaResetTimeStamp":"2026-07-03T13:11:31Z"}}]}}`
	detail, ok := ParseAntigravityQuota([]byte(body))
	if !ok {
		t.Fatal("expected ok=true")
	}
	if detail.ResetDelay != nil {
		t.Errorf("ResetDelay = %v, want nil", detail.ResetDelay)
	}
	want, _ := time.Parse(time.RFC3339, "2026-07-03T13:11:31Z")
	now := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	// 无 RetryInfo 时 RecoverAt 仍应是上游绝对时间（不退化到 now+短退避）
	if got := detail.RecoverAt(now); !got.Equal(want) {
		t.Errorf("RecoverAt = %v, want %v", got, want)
	}
}

func TestParseAntigravityQuota_NonQuotaBody(t *testing.T) {
	if _, ok := ParseAntigravityQuota([]byte(`{"error":{"code":500,"message":"boom"}}`)); ok {
		t.Error("expected ok=false for non-quota body")
	}
}
