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
		"导入时间",
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
	// Status and error detail must be the last two columns in the account table header.
	theadStart := strings.Index(s, `<table id="table"`)
	if theadStart < 0 {
		t.Fatal("embedded page missing account table")
	}
	theadEnd := strings.Index(s[theadStart:], "</tr>")
	if theadEnd < 0 {
		t.Fatal("account table header row not found")
	}
	headerRow := s[theadStart : theadStart+theadEnd]
	if !strings.HasSuffix(strings.TrimSpace(headerRow), "<th>状态</th><th>错误</th>") {
		t.Errorf("状态 followed by 错误 must be the last columns in the account table header, got header row: %s", headerRow)
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

func TestQuotaMonitorHTMLHasAccountFiltersAndPagination(t *testing.T) {
	s := string(QuotaMonitorHTML())
	for _, marker := range []string{
		`id="account-name-filter"`,
		`id="account-plan-filter"`,
		`id="account-created-from"`,
		`id="account-created-to"`,
		`id="account-status-filter"`,
		`id="account-page-size"`,
		`id="account-page-prev"`,
		`id="account-page-next"`,
		`id="account-page-info"`,
		"var AccountViewLogic",
		"function renderAccountView(options)",
		"status_message",
		`data-tip="`,
		`id="hover-tip"`,
		"/v0/management/antigravity-credits/refresh",
	} {
		if !strings.Contains(s, marker) {
			t.Errorf("embedded page missing account-view marker %q", marker)
		}
	}
	for _, removed := range []string{"autoFillCredits", "creditsAutoAttempted"} {
		if strings.Contains(s, removed) {
			t.Errorf("embedded page still contains page-open credits refresh marker %q", removed)
		}
	}
}

func TestQuotaMonitorHTMLHasErrorAndRequestColumns(t *testing.T) {
	s := string(QuotaMonitorHTML())
	for _, marker := range []string{
		"<th>请求数</th><th>成功率</th>",
		"<th>状态</th><th>错误</th>",
		"function errorCell(a)",
		"function requestCells(a)",
		"AccountViewLogic.statusError",
		"AccountViewLogic.requestStats",
		"AccountViewLogic.formatSuccessRate",
		"function parseErrorBody(message)",
		"error_description",
		`class="error-detail"`,
		`class="request-count"`,
		".error-detail {",
		".request-count {",
	} {
		if !strings.Contains(s, marker) {
			t.Errorf("embedded page missing account error/request marker %q", marker)
		}
	}
	// The error column reuses the delegated tooltip rather than a native title.
	if strings.Contains(s, `class="error-detail" title=`) {
		t.Error("error column must use data-tip, not a native title attribute")
	}
}

func TestQuotaMonitorHTMLFiltersByErrorAndBatchDeletes(t *testing.T) {
	s := string(QuotaMonitorHTML())
	for _, marker := range []string{
		`id="account-error-filter"`,
		`id="account-select-all"`,
		`id="account-bulk-bar"`,
		`id="account-bulk-count"`,
		`id="account-bulk-clear"`,
		`id="account-bulk-delete"`,
		`class="row-select"`,
		"function selectCell(a)",
		"function paintSelectionControls(filtered)",
		"function deleteSelectedAccounts(names)",
		"AccountViewLogic.pruneSelection",
		"AccountViewLogic.selectionState",
		"AccountViewLogic.toggleSelection",
		"JSON.stringify({ names: names })",
		"确定删除选中的 ",
		"此操作不可恢复",
		"个不在当前筛选结果中",
	} {
		if !strings.Contains(s, marker) {
			t.Errorf("embedded page missing account selection marker %q", marker)
		}
	}
	// The checkbox column must lead the row so the account name stays first among data columns.
	theadStart := strings.Index(s, `<table id="table"`)
	if theadStart < 0 {
		t.Fatal("embedded page missing account table")
	}
	headerRow := s[theadStart : theadStart+strings.Index(s[theadStart:], "</tr>")]
	selectCol := strings.Index(headerRow, `class="select-col"`)
	nameCol := strings.Index(headerRow, "<th>账号名称</th>")
	if selectCol < 0 || nameCol < 0 || selectCol > nameCol {
		t.Errorf("select-col must be the first column in the account table header, got header row: %s", headerRow)
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
		"function fetchUsage(range)",
		"preset: 'today'",
		"/v0/management/usage-stats?account=",
		"未开启用量统计，请在 config.yaml 设置 usage-stats-enabled: true 后重启/重载",
		"acct-name",
	} {
		if !strings.Contains(s, marker) {
			t.Errorf("embedded page missing usage-stats modal marker %q", marker)
		}
	}
}

func TestQuotaMonitorHTMLCancelsStaleUsageModalRequests(t *testing.T) {
	s := string(QuotaMonitorHTML())
	start := strings.Index(s, "// ---- Usage-stats modal ----")
	if start < 0 {
		t.Fatal("usage-stats modal section missing")
	}
	end := strings.Index(s[start:], "// ---- Wiring ----")
	if end < 0 {
		t.Fatal("usage-stats modal section end missing")
	}
	section := s[start : start+end]
	for _, marker := range []string{
		"var modalGeneration = 0",
		"var modalController = null",
		"function cancelModalRequest()",
		"modalGeneration = ModelStatsLogic.nextGeneration(modalGeneration)",
		"modalController.abort()",
		"ModelStatsLogic.isCurrentGeneration(generation, modalGeneration)",
		"options.signal = controller.signal",
	} {
		if !strings.Contains(section, marker) {
			t.Errorf("usage-stats modal missing stale-request guard %q", marker)
		}
	}
}

func TestQuotaMonitorHTMLHasModelStatsChart(t *testing.T) {
	s := string(QuotaMonitorHTML())
	for _, marker := range []string{
		`id="tabbtn-stats"`,
		`id="tab-stats"`,
		`id="stats-range-today"`,
		`id="stats-range-yesterday"`,
		`id="stats-range-7d"`,
		`id="stats-range-custom"`,
		`id="stats-range-apply"`,
		`id="stats-status"`,
		`id="stats-range-summary"`,
		`id="stats-chart-body"`,
		`id="stats-rows"`,
		"stats-track",
		"stats-segment-success",
		"stats-segment-fail",
		"function renderModelStats(data)",
		"escapeHtml(row.model)",
		"escapeHtml(tip)",
	} {
		if !strings.Contains(s, marker) {
			t.Errorf("embedded page missing model-stats marker %q", marker)
		}
	}
}

func TestQuotaMonitorHTMLHasRaceSafeModelStatsLifecycle(t *testing.T) {
	s := string(QuotaMonitorHTML())
	for _, marker := range []string{
		"var refreshMs = 60000",
		"function requestModelStats()",
		"function maybeAutoRefreshModelStats()",
		"function resetModelStatsState()",
		"ModelStatsLogic.nextGeneration",
		"ModelStatsLogic.isCurrentGeneration",
		"new AbortController()",
		"/v0/management/usage-stats?",
		"preset=",
		"仍显示 ",
		"statsState.lastSuccessRange",
		"reason === 'auto'",
		"reason === 'manual'",
		"reason === 'login'",
		"document.addEventListener('focusin'",
	} {
		if !strings.Contains(s, marker) {
			t.Errorf("embedded page missing model-stats lifecycle marker %q", marker)
		}
	}
	if strings.Contains(s, "function todayStr()") || strings.Contains(s, "function yesterdayStr()") {
		t.Error("usage presets must no longer use browser-calendar helper functions")
	}
}

func TestQuotaMonitorHTMLTracksSuccessfulModelStatsAgeSeparately(t *testing.T) {
	s := string(QuotaMonitorHTML())
	for _, marker := range []string{
		"lastSuccessAt: 0",
		"statsState.lastSuccessAt = 0",
		"statsState.lastSuccessAt = Date.now()",
		"ModelStatsLogic.shouldRefreshOnActivate(statsState.lastSuccessAt, Date.now())",
	} {
		if !strings.Contains(s, marker) {
			t.Errorf("embedded page missing successful-result age marker %q", marker)
		}
	}
	if strings.Contains(s, "ModelStatsLogic.shouldRefreshOnActivate(statsState.lastRequestAt, Date.now())") {
		t.Error("tab activation must use the last successful result time, not the last request time")
	}
}

func TestQuotaMonitorHTMLClearsModelStatsState(t *testing.T) {
	s := string(QuotaMonitorHTML())
	start := strings.Index(s, "function clearRenderedData()")
	if start < 0 {
		t.Fatal("clearRenderedData missing")
	}
	end := strings.Index(s[start:], "\n  }")
	if end < 0 || !strings.Contains(s[start:start+end], "resetModelStatsState();") {
		t.Error("clearRenderedData must reset model statistics state")
	}
}

func TestQuotaMonitorHTMLHidesUsageStatsModalOnLoad(t *testing.T) {
	s := string(QuotaMonitorHTML())
	if !strings.Contains(s, `#modal-backdrop[hidden] { display: none; }`) {
		t.Error("usage-stats modal backdrop must remain hidden when the hidden attribute is present")
	}
}

func TestQuotaMonitorHTMLHasNoRemoteScripts(t *testing.T) {
	s := string(QuotaMonitorHTML())
	if strings.Contains(s, `<script src=`) || strings.Contains(s, "cdn.tailwindcss.com") {
		t.Fatal("quota monitor must not execute remote scripts")
	}
}
