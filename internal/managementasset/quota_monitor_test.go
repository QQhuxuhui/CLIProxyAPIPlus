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
	if !strings.Contains(s, "/v0/management/auth-files") {
		t.Error("embedded page missing auth-files endpoint path")
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
		`id="mh-filter"`,
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

func TestQuotaMonitorHTMLSurfacesSummaryFetchFailure(t *testing.T) {
	s := string(QuotaMonitorHTML())
	if !strings.Contains(s, "模型健康汇总加载失败") {
		t.Error("embedded page missing quota-summary failure message")
	}
	if strings.Contains(s, ".catch(function () {});") {
		t.Error("fetchSummary still swallows errors silently")
	}
}

func TestQuotaMonitorHTMLHasAccountList(t *testing.T) {
	s := string(QuotaMonitorHTML())
	for _, marker := range []string{
		"账号列表",
		"账号名称",
		"套餐",
		"创建时间",
		"function render(files)",
		"data.files",
		"accountsData",
		"function accountPlan(a)",
		"a.id_token && a.id_token.plan_type",
		"td(accountPlan(a))",
	} {
		if !strings.Contains(s, marker) {
			t.Errorf("embedded page missing account-list marker %q", marker)
		}
	}
	// status must be the LAST <th> in the account table's header row.
	theadStart := strings.Index(s, `<table id="table"`)
	if theadStart < 0 {
		t.Fatal("embedded page missing account table")
	}
	theadEnd := strings.Index(s[theadStart:], "</tr>")
	if theadEnd < 0 {
		t.Fatal("account table header row not found")
	}
	headerRow := s[theadStart : theadStart+theadEnd]
	lastTh := strings.LastIndex(headerRow, "<th>")
	if lastTh < 0 || !strings.HasPrefix(headerRow[lastTh:], "<th>状态</th>") {
		t.Errorf("状态 column must be the last <th> in the account table header, got header row: %s", headerRow)
	}
	for _, gone := range []string{
		"冷却明细", "pending_verification", "待验证", ".badge.p", ".badge.q",
		"detailFilter", "applyDetailFilter", "mh-filter-clear",
	} {
		if strings.Contains(s, gone) {
			t.Errorf("embedded page still contains removed cooldown-detail marker %q", gone)
		}
	}
}

func TestQuotaMonitorHTMLDeletesAccount(t *testing.T) {
	s := string(QuotaMonitorHTML())
	for _, marker := range []string{
		"del-btn",
		"function deleteAccount(name)",
		"method: 'DELETE'",
		`ENDPOINT + '?name=' + encodeURIComponent(name)`,
		`确定删除认证文件`,
		`此操作不可恢复`,
		"window.confirm(",
	} {
		if !strings.Contains(s, marker) {
			t.Errorf("embedded page missing delete marker %q", marker)
		}
	}
}

func TestQuotaMonitorHTMLHasUsageStatsModal(t *testing.T) {
	s := string(QuotaMonitorHTML())
	for _, marker := range []string{
		`id="modal-backdrop"`,
		`id="range-today"`,
		`id="range-yesterday"`,
		`id="range-7d"`,
		`id="range-custom"`,
		`id="range-apply"`,
		"function openUsageModal(name, authIndex)",
		"+ authIndex + '）'", // modal title must actually use authIndex, not just accept it
		"function fetchUsage(from, to)",
		"/v0/management/usage-stats?account=",
		"未开启用量统计，请在 config.yaml 设置 usage-stats-enabled: true 后重启/重载",
		"acct-name",
	} {
		if !strings.Contains(s, marker) {
			t.Errorf("embedded page missing usage-stats modal marker %q", marker)
		}
	}
}

func TestQuotaMonitorHTMLHidesUsageStatsModalOnLoad(t *testing.T) {
	s := string(QuotaMonitorHTML())
	if !strings.Contains(s, `#modal-backdrop[hidden] { display: none; }`) {
		t.Error("usage-stats modal backdrop must remain hidden when the hidden attribute is present")
	}
}
