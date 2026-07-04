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

func TestQuotaMonitorHTMLHasDashboard(t *testing.T) {
	s := string(QuotaMonitorHTML())
	for _, marker := range []string{
		"/v0/management/quota-summary",
		`id="dash-strip"`,
		`id="model-health"`,
		`id="mh-rows"`,
		`id="mh-filter"`,
		"function healthOf",
		"var HEALTH",
		"可服务模型全部健康",
	} {
		if !strings.Contains(s, marker) {
			t.Errorf("embedded page missing model-health marker %q", marker)
		}
	}
	for _, gone := range []string{
		`id="dash-donut"`, `id="dash-dist"`, `id="dash-prov"`, `id="dash-cards"`,
	} {
		if strings.Contains(s, gone) {
			t.Errorf("embedded page still contains removed widget %q", gone)
		}
	}
}

func TestQuotaMonitorHTMLClearsRenderedData(t *testing.T) {
	s := string(QuotaMonitorHTML())
	for _, marker := range []string{
		"function clearRenderedData()",
		"clearRenderedData();",
		"document.getElementById('dashboard').hidden = true",
		"els.updated.textContent = ''",
		"document.getElementById('mh-rows').innerHTML = ''",
	} {
		if !strings.Contains(s, marker) {
			t.Errorf("embedded page missing stale data cleanup marker %q", marker)
		}
	}
}

func TestQuotaMonitorHTMLRendersPendingVerification(t *testing.T) {
	s := string(QuotaMonitorHTML())
	for _, marker := range []string{
		"pending_verification",
		"待验证",
		".badge.p",
	} {
		if !strings.Contains(s, marker) {
			t.Errorf("embedded page missing pending-verification marker %q", marker)
		}
	}
}

// TestQuotaMonitorHTMLBoundsRememberedKey guards S20: the full-privilege
// management key must not be persisted indefinitely in localStorage. The
// remembered key is stored as a {v, exp} envelope with a TTL and read back
// through getRememberedKey (which purges expired/legacy entries), so a bare
// unbounded localStorage.setItem of the raw key must not reappear.
func TestQuotaMonitorHTMLBoundsRememberedKey(t *testing.T) {
	s := string(QuotaMonitorHTML())
	for _, marker := range []string{
		"KEY_TTL_MS",
		"function getRememberedKey()",
		"localStorage.setItem(KEY_NAME, JSON.stringify({ v: k, exp:",
	} {
		if !strings.Contains(s, marker) {
			t.Errorf("embedded page missing bounded-key marker %q", marker)
		}
	}
	// The old unbounded persistence (raw key, never expires) must be gone.
	if strings.Contains(s, "localStorage.setItem(KEY_NAME, k)") {
		t.Error("embedded page still persists the raw management key to localStorage without a TTL")
	}
}

func TestQuotaMonitorHTMLSurfacesSummaryFetchFailure(t *testing.T) {
	s := string(QuotaMonitorHTML())
	if !strings.Contains(s, "模型健康汇总加载失败") {
		t.Error("embedded page missing quota-summary failure message")
	}
	if strings.Contains(s, ".catch(function () {});") {
		t.Error("fetchSummary still swallows errors silently")
	}
}
