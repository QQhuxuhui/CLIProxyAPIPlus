package managementasset

import (
	"strings"
	"testing"
)

func TestQuotaMonitorHTML(t *testing.T) {
	b := QuotaMonitorHTML()
	if len(b) == 0 {
		t.Fatal("QuotaMonitorHTML() is empty")
	}
	s := string(b)
	if !strings.Contains(s, "/v0/management/model-quota") {
		t.Error("embedded page missing model-quota endpoint path")
	}
	if !strings.Contains(s, "cpa_quota_monitor_key") {
		t.Error("embedded page missing storage key name")
	}
	if !strings.Contains(s, `id="key"`) {
		t.Error("embedded page missing key input element")
	}
	// The returned slice must be a copy; mutating it must not affect later calls.
	b[0] = 0
	if QuotaMonitorHTML()[0] == 0 {
		t.Error("QuotaMonitorHTML() did not return a copy")
	}
}
