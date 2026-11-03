import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const html = readFileSync(new URL('./quota_monitor.html', import.meta.url), 'utf8');
function logicBlock(name) {
  const startMarker = '/* ' + name + '_START */';
  const endMarker = '/* ' + name + '_END */';
  const start = html.indexOf(startMarker);
  const end = html.indexOf(endMarker);
  assert.notEqual(start, -1, name + ' start marker is missing');
  assert.notEqual(end, -1, name + ' end marker is missing');
  assert.ok(end > start, name + ' markers are out of order');
  return html.slice(start + startMarker.length, end);
}

const context = vm.createContext({ Date, Math, String, Number, Array, Object, JSON, isNaN, isFinite });
// Values built inside the vm realm carry its prototypes, which strict deepEqual
// rejects; round-tripping through JSON compares them structurally instead.
const plain = (value) => JSON.parse(JSON.stringify(value));
vm.runInContext(logicBlock('ACCOUNT_VIEW_LOGIC'), context);
const logic = context.AccountViewLogic;

test('accountPlan uses paid tier then Codex token plan then unknown', () => {
  assert.equal(logic.accountPlan({ paid_tier: 'pro', id_token: { plan_type: 'plus' } }), 'pro');
  assert.equal(logic.accountPlan({ id_token: { plan_type: 'plus' } }), 'plus');
  assert.equal(logic.accountPlan({}), '—');
});

test('filterAccounts combines name, plan, local date, and status filters', () => {
  const fixtures = [
    { name: 'Alpha Codex', provider: 'codex', id_token: { plan_type: 'plus' }, created_at: '2026-07-15T12:00:00', status: 'active' },
    { name: 'Beta', provider: 'antigravity', paid_tier: 'pro', created_at: '2026-07-15T12:00:00', status: 'error' },
  ];
  const rows = logic.filterAccounts(fixtures, {
    name: 'alpha', plan: 'plus', status: 'active', from: '2026-07-15', to: '2026-07-15',
  });
  assert.deepEqual(Array.from(rows, (row) => row.name), ['Alpha Codex']);
});

test('filterAccounts excludes invalid dates when a date bound is active', () => {
  const rows = logic.filterAccounts([{ name: 'Unknown date', created_at: 'invalid' }], {
    name: '', plan: '', status: '', from: '2026-07-15', to: '',
  });
  assert.equal(rows.length, 0);
});

test('localDateKey uses the browser local calendar day', () => {
  const value = '2026-07-15T23:30:00-07:00';
  const date = new Date(value);
  const expected = [
    date.getFullYear(),
    String(date.getMonth() + 1).padStart(2, '0'),
    String(date.getDate()).padStart(2, '0'),
  ].join('-');
  assert.equal(logic.localDateKey(value), expected);
});

test('paginate clamps a stale final page', () => {
  const result = logic.paginate([{ id: 1 }, { id: 2 }], 2, 2);
  assert.equal(result.page, 1);
  assert.equal(result.totalPages, 1);
  assert.deepEqual(Array.from(result.items, (row) => row.id), [1, 2]);
});

test('manual plan refresh preserves a selected plan that now has zero matches', () => {
  const account = { name: 'Plan changed', paid_tier: 'pro' };
  account.paid_tier = 'ultra';

  const rows = logic.filterAccounts([account], { plan: 'pro' });
  const options = logic.preserveSelectedOption(['ultra'], 'pro', true);

  assert.equal(rows.length, 0);
  assert.deepEqual(Array.from(options), ['pro', 'ultra']);
});

test('statusError shows the error code for structured upstream bodies', () => {
  const body = JSON.stringify({
    error: {
      code: 429,
      message: 'Individual quota reached. Resets in 3h2m14s.',
      status: 'RESOURCE_EXHAUSTED',
      details: [{ '@type': 'type.googleapis.com/google.rpc.ErrorInfo', reason: 'QUOTA_EXHAUSTED' }],
    },
  });
  assert.deepEqual(plain(logic.statusError({ status_message: body })), {
    text: '429',
    tip: 'Individual quota reached. Resets in 3h2m14s.',
  });
});

test('statusError shows the reason for flat OAuth bodies', () => {
  const body = JSON.stringify({ error: 'invalid_grant', error_description: 'Bad Request' });
  assert.deepEqual(plain(logic.statusError({ status_message: body })), {
    text: 'invalid_grant',
    tip: 'invalid_grant: Bad Request',
  });
  assert.deepEqual(plain(logic.statusError({ status_message: JSON.stringify({ error: 'invalid_grant' }) })), {
    text: 'invalid_grant',
    tip: 'invalid_grant',
  });
});

test('statusError parses a body carrying an upstream prefix', () => {
  const body = 'upstream returned status 429: ' + JSON.stringify({ error: { code: 429, status: 'RESOURCE_EXHAUSTED' } });
  assert.deepEqual(plain(logic.statusError({ status_message: body })), {
    text: '429',
    tip: 'RESOURCE_EXHAUSTED',
  });
});

test('statusError falls back to the raw text for plain conductor labels', () => {
  assert.deepEqual(plain(logic.statusError({ status_message: 'quota exhausted' })), {
    text: 'quota exhausted',
    tip: 'quota exhausted',
  });
  assert.deepEqual(plain(logic.statusError({ status_message: '  ' })), { text: '', tip: '' });
  assert.deepEqual(plain(logic.statusError({})), { text: '', tip: '' });
  assert.deepEqual(plain(logic.statusError({ status_message: '[1,2]' })), { text: '[1,2]', tip: '[1,2]' });
});

test('statusError collapses the codex invalidated-credential message to one label', () => {
  const message = 'credential invalidated: 3 consecutive 401 responses, re-login required (web image credential was rejected (token_invalidated))';
  assert.deepEqual(plain(logic.statusError({ status_message: message })), { text: 'credential invalidated', tip: message });
  assert.deepEqual(plain(logic.statusError({ status_message: message.replace('3 consecutive', '7 consecutive') })).text, 'credential invalidated');
  // Plain web-image labels keep the upstream code so the filter can tell them apart.
  assert.deepEqual(plain(logic.statusError({ status_message: 'web image credential was rejected (token_invalidated)' })), {
    text: 'web image credential was rejected (token_invalidated)',
    tip: 'web image credential was rejected (token_invalidated)',
  });
});

test('authFailures reads the consecutive 401 counter defensively', () => {
  assert.equal(logic.authFailures({ auth_failures: 3 }), 3);
  assert.equal(logic.authFailures({ auth_failures: '2' }), 2);
  assert.equal(logic.authFailures({ auth_failures: -1 }), 0);
  assert.equal(logic.authFailures({}), 0);
});

test('statusError falls back to status when the structured body carries no code', () => {
  const body = JSON.stringify({ error: { status: 'UNAVAILABLE' } });
  assert.deepEqual(plain(logic.statusError({ status_message: body })), { text: 'UNAVAILABLE', tip: 'UNAVAILABLE' });
  assert.deepEqual(plain(logic.statusError({ status_message: JSON.stringify({ error: {} }) })), {
    text: 'error',
    tip: JSON.stringify({ error: {} }),
  });
});

test('accountProvider normalizes case, whitespace and missing values', () => {
  assert.equal(logic.accountProvider({ provider: ' Codex ' }), 'codex');
  for (const entry of [{ provider: '   ' }, { provider: null }, {}, null]) assert.equal(logic.accountProvider(entry), '—');
});

test('filterAccounts narrows by provider and combines with other filters', () => {
  const fixtures = [
    { name: 'alpha', provider: 'codex', status: 'active' },
    { name: 'beta', provider: 'CODEX', status: 'error' },
    { name: 'gamma', provider: 'gemini', status: 'active' },
    { name: 'empty', provider: ' ', status: 'active' },
  ];
  const names = (filters) => Array.from(logic.filterAccounts(fixtures, filters), (row) => row.name);
  assert.deepEqual(names({ provider: 'codex' }), ['alpha', 'beta']);
  assert.deepEqual(names({ provider: '—' }), ['empty']);
  assert.deepEqual(names({ provider: 'codex', status: 'error' }), ['beta']);
  assert.deepEqual(names({ provider: 'claude' }), []);
  assert.equal(names({}).length, 4);
});

test('filterAccounts narrows to one parsed error label', () => {
  const quota = JSON.stringify({ error: { code: 429, status: 'RESOURCE_EXHAUSTED' } });
  const fixtures = [
    { name: 'quota-a', status: 'error', status_message: quota },
    { name: 'quota-b', status: 'error', status_message: quota },
    { name: 'grant', status: 'error', status_message: JSON.stringify({ error: 'invalid_grant' }) },
    { name: 'healthy', status: 'active' },
  ];
  assert.deepEqual(Array.from(logic.filterAccounts(fixtures, { error: '429' }), (row) => row.name), ['quota-a', 'quota-b']);
  assert.deepEqual(Array.from(logic.filterAccounts(fixtures, { error: 'invalid_grant' }), (row) => row.name), ['grant']);
  assert.equal(logic.filterAccounts(fixtures, { error: '' }).length, 4);
});

test('filterAccounts combines the error filter with the other filters', () => {
  const fixtures = [
    { name: 'alpha', status: 'error', status_message: JSON.stringify({ error: { code: 429 } }) },
    { name: 'beta', status: 'error', status_message: JSON.stringify({ error: { code: 429 } }) },
  ];
  assert.deepEqual(
    Array.from(logic.filterAccounts(fixtures, { name: 'alp', status: 'error', error: '429' }), (row) => row.name),
    ['alpha'],
  );
  assert.equal(logic.filterAccounts(fixtures, { name: 'alp', error: '401' }).length, 0);
});

test('pruneSelection drops names that no longer exist upstream', () => {
  const entries = [{ name: 'keep' }, { name: 'also-keep' }];
  assert.deepEqual(plain(logic.pruneSelection(['keep', 'gone', 'also-keep'], entries)), ['keep', 'also-keep']);
  assert.deepEqual(plain(logic.pruneSelection(['keep'], [])), []);
  assert.deepEqual(plain(logic.pruneSelection(null, entries)), []);
});

test('selectionState reports the header checkbox tri-state over filtered rows', () => {
  const rows = [{ name: 'a' }, { name: 'b' }, { name: 'c' }];
  assert.deepEqual(plain(logic.selectionState([], rows)), { total: 3, selected: 0, all: false, some: false });
  assert.deepEqual(plain(logic.selectionState(['a'], rows)), { total: 3, selected: 1, all: false, some: true });
  assert.deepEqual(plain(logic.selectionState(['a', 'b', 'c'], rows)), { total: 3, selected: 3, all: true, some: false });
  // A selection held outside the current filter must not tick the header box.
  assert.deepEqual(plain(logic.selectionState(['hidden'], rows)), { total: 3, selected: 0, all: false, some: false });
  assert.deepEqual(plain(logic.selectionState(['a'], [])), { total: 0, selected: 0, all: false, some: false });
});

test('toggleSelection only adds or removes the rows it was given', () => {
  const rows = [{ name: 'a' }, { name: 'b' }];
  // Selecting all keeps a name chosen under a previous filter.
  assert.deepEqual(plain(logic.toggleSelection(['other'], rows, true)), ['other', 'a', 'b']);
  assert.deepEqual(plain(logic.toggleSelection(['a', 'other'], rows, true)), ['a', 'other', 'b']);
  // Clearing all leaves names outside the current filter untouched.
  assert.deepEqual(plain(logic.toggleSelection(['a', 'b', 'other'], rows, false)), ['other']);
  assert.deepEqual(plain(logic.toggleSelection(['other'], [], true)), ['other']);
});

test('requestStats sums the in-process counters and ignores junk', () => {
  assert.deepEqual(plain(logic.requestStats({ success: 97, failed: 3 })), { success: 97, failed: 3, total: 100 });
  assert.deepEqual(plain(logic.requestStats({ success: -5, failed: 'x' })), { success: 0, failed: 0, total: 0 });
  assert.deepEqual(plain(logic.requestStats(null)), { success: 0, failed: 0, total: 0 });
});

test('formatSuccessRate never rounds a partial result to a clean 100% or 0%', () => {
  assert.equal(logic.formatSuccessRate({ success: 0, failed: 0, total: 0 }), '—');
  assert.equal(logic.formatSuccessRate({ success: 100, failed: 0, total: 100 }), '100%');
  assert.equal(logic.formatSuccessRate({ success: 0, failed: 4, total: 4 }), '0%');
  assert.equal(logic.formatSuccessRate({ success: 197, failed: 3, total: 200 }), '98.5%');
  assert.equal(logic.formatSuccessRate({ success: 9999, failed: 1, total: 10000 }), '99.9%');
  assert.equal(logic.formatSuccessRate({ success: 1, failed: 9999, total: 10000 }), '0.1%');
});

vm.runInContext(logicBlock('MODEL_STATS_LOGIC'), context);
const modelLogic = context.ModelStatsLogic;

test('aggregate merges exact model names across accounts and sorts stable ties', () => {
  const rows = modelLogic.aggregate([
    { account: 'a', models: [
      { model: 'z-model', success: 2, fail: 1 },
      { model: 'a-model', success: 3, fail: 0 },
      { model: 'shared', success: 4, fail: 1 },
    ] },
    { account: 'b', models: [
      { model: 'shared', success: 2, fail: 3 },
      { model: 'shared(high)', success: 1, fail: 0 },
    ] },
  ]);
  assert.deepEqual(plain(rows), [
    { model: 'shared', success: 6, fail: 4, total: 10 },
    { model: 'a-model', success: 3, fail: 0, total: 3 },
    { model: 'z-model', success: 2, fail: 1, total: 3 },
    { model: 'shared(high)', success: 1, fail: 0, total: 1 },
  ]);
});

test('aggregate removes zero rows and retains fail-only counts', () => {
  const rows = modelLogic.aggregate([{ models: [
    { model: 'zero', success: 0, fail: 0 },
    { model: 'failed', success: 0, fail: 7 },
  ] }]);
  assert.deepEqual(plain(rows), [{ model: 'failed', success: 0, fail: 7, total: 7 }]);
  assert.deepEqual(plain(modelLogic.aggregate([])), []);
});

test('scale uses global totals and row-local segment proportions', () => {
  const rows = modelLogic.scale([
    { model: 'large', success: 75, fail: 25, total: 100 },
    { model: 'small', success: 10, fail: 10, total: 20 },
  ]);
  assert.equal(rows[0].outerPct, 100);
  assert.equal(rows[0].successPct, 75);
  assert.equal(rows[0].failPct, 25);
  assert.equal(rows[1].outerPct, 20);
  assert.equal(rows[1].successPct, 50);
  assert.equal(rows[1].failPct, 50);
});

test('generation gate rejects stale responses', () => {
  const first = modelLogic.nextGeneration(0);
  const second = modelLogic.nextGeneration(first);
  assert.equal(modelLogic.isCurrentGeneration(first, second), false);
  assert.equal(modelLogic.isCurrentGeneration(second, second), true);
});

test('loaded state requires a successful result and resets when disabled', () => {
  assert.equal(typeof modelLogic.loadedAfterResult, 'function', 'loadedAfterResult must be exposed');
  assert.equal(modelLogic.loadedAfterResult(false, 'error'), false);
  assert.equal(modelLogic.loadedAfterResult(true, 'error'), true);
  assert.equal(modelLogic.loadedAfterResult(false, 'ok'), true);
  assert.equal(modelLogic.loadedAfterResult(true, 'disabled'), false);
});

test('refresh policy requires active current range and sixty seconds', () => {
  const base = {
    autoEnabled: true,
    tabActive: true,
    range: { preset: '7d', from: '', to: '' },
    serverToday: '2026-07-17',
    lastRequestAt: 1000,
    nowMs: 61000,
  };
  assert.equal(modelLogic.shouldAutoRefresh(base), true);
  assert.equal(modelLogic.shouldAutoRefresh({ ...base, tabActive: false }), false);
  assert.equal(modelLogic.shouldAutoRefresh({ ...base, range: { preset: 'yesterday', from: '', to: '' } }), false);
  assert.equal(modelLogic.shouldAutoRefresh({ ...base, lastRequestAt: 2000 }), false);
  assert.equal(modelLogic.rangeIncludesToday({ preset: 'custom', from: '2026-07-10', to: '2026-07-17' }, '2026-07-17'), true);
  assert.equal(modelLogic.rangeIncludesToday({ preset: 'custom', from: '2026-07-10', to: '2026-07-16' }, '2026-07-17'), false);
  assert.equal(modelLogic.shouldRefreshOnActivate(1000, 61000), true);
  assert.equal(modelLogic.shouldRefreshOnActivate(2000, 61000), false);
});

test('selectedEmails keeps list order, drops duplicates, and counts accounts without email', () => {
  const entries = [
    { name: 'a.json', email: 'a@x.com' },
    { name: 'b.json' },
    { name: 'c.json', email: ' a@x.com ' },
    { name: 'd.json', email: 'd@x.com' },
    { name: 'e.json', email: 'e@x.com' },
  ];
  assert.deepEqual(plain(logic.selectedEmails(['d.json', 'a.json', 'b.json', 'c.json'], entries)), {
    emails: ['a@x.com', 'd@x.com'],
    missing: 1,
  });
  assert.deepEqual(plain(logic.selectedEmails([], entries)), { emails: [], missing: 0 });
});

vm.runInContext(logicBlock('CODEX_POOL_LOGIC'), context);
test('Codex pool chart options keep TTFT gaps and the learned limit histogram', () => {
  const charts = plain(context.CodexPoolLogic.chartOptions({
    points: [{time:'2026-10-22T12:00:00Z',attempts:40,ttft_p50:7,ttft_p90:10},{time:'2026-10-22T12:01:00Z',attempts:0,ttft_p50:0,ttft_p90:0}],
    limits: [{limit:20,accounts:8}],
  }));
  assert.equal(charts.length, 2);
  assert.deepEqual(charts[0].series.map(s=>s.data), [[7,null],[10,null]]);
  assert.deepEqual(charts[1].xAxis.data, [20]);
  assert.deepEqual(charts[1].series[0].data, [8]);
  assert.equal(context.CodexPoolLogic.concurrencyCapacity({ concurrency_capacity: 10, unlimited_accounts: 3, concurrency_enabled: true }), '10 + ∞×3');
  assert.equal(context.CodexPoolLogic.concurrencyCapacity({ concurrency_capacity: 10 }), '10（未启用）');
});

function poolLifecycleFixture() {
  const elements = new Map(), scripts = [];
  const element = id => {
    if (!elements.has(id)) elements.set(id, {value:'',checked:false,disabled:false,textContent:'',className:'',appendChild(){},addEventListener(){},reportValidity(){return true;},reset(){}});
    return elements.get(id);
  };
  element('pool-config').elements = ['enabled','fail-fast','initial','min','max','probe','headroom','save'].map(id=>element('pool-'+id));
  let resolveSave;
  const saveResponse = new Promise(resolve=>{resolveSave=resolve;});
  const sandbox = vm.createContext({
    Date,Math,String,Number,Array,Object,JSON,AbortController,
    currentTab:'pool',firstSuccess:true,getStoredKey:()=> 'test-key',
    document:{getElementById:element,addEventListener(){},createElement:()=>({remove(){},appendChild(){}}),head:{appendChild:script=>scripts.push(script)}},
    escapeHtml:s=>String(s),
    window:{addEventListener(){}},setInterval:()=>1,clearInterval(){},fetch:()=>saveResponse,
    readError:()=>Promise.resolve('failed'),clearKey(){},setStatus(){}
  });
  vm.runInContext(logicBlock('CODEX_POOL_LOGIC'), sandbox);
  vm.runInContext(logicBlock('SPEED_VIEW_LOGIC'), sandbox);
  vm.runInContext(logicBlock('POOL_VIEW_LOGIC'), sandbox);
  const start=html.indexOf('  var poolState ='),end=html.indexOf('  // ---- Proxy settings ----',start);
  assert.ok(start>=0 && end>start);
  vm.runInContext(html.slice(start,end),sandbox);
  const data={config:{enabled:false,'fail-fast':false,'initial-per-minute':20,'min-per-minute':5,'max-per-minute':60,'probe-interval':'5m',headroom:1},home_mode:false,success_rate:0,points:[],limits:[]};
  sandbox.renderPool(data);sandbox.loadPoolECharts();
  element('pool-enabled').checked=true;element('pool-fail-fast').checked=true;element('pool-headroom').value='0.75';sandbox.poolState.dirty=true;
  const finishCharts = success => {
    if(success){sandbox.window.echarts={init:()=>({setOption(){},resize(){},dispose(){}})};scripts[0].onload();}
    else scripts[0].onerror();
  };
  return {sandbox,element,resolveSave,finishCharts};
}

for (const success of [true,false]) {
  test(`late chart ${success?'load':'failure'} preserves pending-save form locks`,async()=>{
    const {sandbox,element,resolveSave,finishCharts}=poolLifecycleFixture();
    sandbox.savePool({preventDefault(){}});
    assert.equal(element('pool-enabled').disabled,true);
    finishCharts(success);
    assert.equal(element('pool-enabled').disabled,true);
    assert.equal(element('pool-fail-fast').disabled,true);
    assert.equal(element('pool-headroom').value,'0.75');
    assert.equal(element('pool-status').textContent,'保存中…');
    resolveSave({ok:true});await new Promise(setImmediate);
    assert.equal(element('pool-enabled').disabled,false);
    assert.equal(element('pool-enabled').checked,true);
  });
  test(`late chart ${success?'load':'failure'} after save keeps committed form values`,async()=>{
    const {sandbox,element,resolveSave,finishCharts}=poolLifecycleFixture();
    sandbox.savePool({preventDefault(){}});resolveSave({ok:true});await new Promise(setImmediate);
    assert.equal(sandbox.poolState.dirty,false);
    finishCharts(success);
    assert.equal(element('pool-enabled').checked,true);
    assert.equal(element('pool-fail-fast').checked,true);
    assert.equal(element('pool-headroom').value,'0.75');
    assert.equal(element('pool-status').textContent,'已保存，等待配置热更新');
  });
}

vm.runInContext(logicBlock('CODEX_QUOTA_LOGIC'), context);
const quotaLogic = context.CodexQuotaLogic;
const identity = (value) => String(value);

test('windowLabel follows the actual window length', () => {
  assert.equal(quotaLogic.windowLabel(300), '5h');
  assert.equal(quotaLogic.windowLabel(10080), '周');
  assert.equal(quotaLogic.windowLabel(43200), '月');
  assert.equal(quotaLogic.windowLabel(1440), '1天');
  assert.equal(quotaLogic.windowLabel(90), '90分');
});

test('applyView filters only Codex rows and sorts unknown quota last', () => {
  const entries = [
    { name: 'gem', provider: 'gemini' },
    { name: 'empty', provider: 'codex' },
    { name: 'high', provider: 'codex' },
    { name: 'low', provider: 'codex' },
  ];
  const rows = { high: { remaining: 80 }, low: { remaining: 2 } };
  assert.deepEqual(Array.from(quotaLogic.applyView(entries, rows, 'low', ''), (row) => row.name), ['low']);
  assert.deepEqual(Array.from(quotaLogic.applyView(entries, rows, 'unknown', ''), (row) => row.name), ['empty']);
  assert.deepEqual(Array.from(quotaLogic.applyView(entries, rows, '', 'asc'), (row) => row.name), ['low', 'high', 'gem', 'empty']);
  assert.deepEqual(Array.from(quotaLogic.applyView(entries, rows, '', 'desc'), (row) => row.name), ['high', 'low', 'gem', 'empty']);
});

test('cellMarkup shows bars, reset, pending, duplicate and staleness', () => {
  const now = Date.parse('2026-10-31T00:00:00Z');
  const html = quotaLogic.cellMarkup({
    remaining: 5, duplicate: true, observed_at: '2026-10-30T22:00:00Z',
    windows: [
      { minutes: 300, used_percent: 95, reset_at: '2026-10-31T01:00:00Z' },
      { minutes: 10080, used_percent: 0, pending: true },
    ],
  }, now, identity);
  assert.match(html, /5h 窗口：已用 95%，1 小时后恢复/);
  assert.match(html, /width:5%/);
  assert.match(html, /待确认/);
  assert.match(html, /重复/);
  assert.match(html, /2 小时前/);
  assert.match(quotaLogic.cellMarkup(undefined, now, identity), /未知/);
  assert.match(quotaLogic.cellMarkup({ invalid: true, windows: [] }, now, identity), /失效/);
});

const quotaSummary = (overrides = {}) => ({
  files: 860, unique_accounts: 646, duplicate_files: 201, invalid: 21, disabled: 0, usable: 94, unknown: 66,
  settings: { headroom_percent: 20, horizon_hours: 24 },
  plans: [
    { plan: 'plus', accounts: 633, usable: 94, recover_24h: 111, windows: [{ minutes: 300, accounts: 573, avg_remaining: 70.7 }, { minutes: 10080, accounts: 579, avg_remaining: 11.6 }] },
    { plan: 'unknown', accounts: 8, usable: 0, recover_24h: 0, windows: [] },
  ],
  advice: { recommended_accounts: 0, rpm_shortfall: 0, peak_rpm: 410, target_rpm: 492, usable_capacity: 6400, runway_hours: null, warming_up: true, low_confidence: false },
  ...overrides,
});

test('overview flags a low window even when no accounts are needed', () => {
  const view = quotaLogic.overview(quotaSummary());
  assert.equal(view.tone, 'warn');
  assert.equal(view.count, 0);
  assert.equal(view.headline, '暂时不用加号，但周额度偏低');
  assert.match(view.detail, /多数账号已用完额度/);
  assert.deepEqual(Array.from(view.tiles, (tile) => tile.value), ['94', '收集中', '+111']);
  assert.match(view.tiles[2].note, /约 205 个可用/);
  assert.deepEqual(Array.from(view.states, (state) => state.count), [94, 486, 66]);
  assert.match(view.stateExtra, /21 个文件已失效/);
  assert.deepEqual(Array.from(view.meters, (meter) => `${meter.label}|${meter.value}|${meter.status}`), ['5 小时额度|剩 71%|充足', '周额度|剩 12%|偏低', '请求容量|峰值 410 / 分钟|够用']);
  assert.equal(view.plans[0].remaining, '5 小时 71% · 周 12%');
  assert.equal(view.plans[1].plan, '未识别');
  assert.equal(view.plans[1].remaining, '暂无数据');
});

test('overview reports the add-account recommendation and its cause', () => {
  const quota = quotaLogic.overview(quotaSummary({ advice: { recommended_accounts: 120, usable_capacity: 1520, peak_rpm: 410, target_rpm: 492, runway_hours: 6 } }));
  assert.equal(quota.tone, 'danger');
  assert.equal(quota.headline, '建议今天加 120 个号');
  assert.match(quota.detail, /约 6 小时后可用账号撑不住高峰/);
  assert.equal(quota.tiles[1].value, '约 6 小时');
  const rough = quotaLogic.overview(quotaSummary({ advice: { recommended_accounts: 70, runway_hours: 12, warming_up: true } }));
  assert.match(rough.detail, /用量数据不足 24 小时，仅供参考/);
  const rpm = quotaLogic.overview(quotaSummary({ advice: { recommended_accounts: 5, rpm_shortfall: 5, usable_capacity: 400, peak_rpm: 410, target_rpm: 492, runway_hours: null } }));
  assert.match(rpm.detail, /高峰需要 492 \/ 分钟，现在只能承载 400 \/ 分钟/);
  assert.equal(rpm.meters.at(-1).status, '不够用');
});

test('overview weighs plan windows and covers the healthy states', () => {
  const plans = [
    { plan: 'free', accounts: 90, usable: 90, recover_24h: 0, windows: [{ minutes: 43200, accounts: 90, avg_remaining: 80 }] },
    { plan: 'plus', accounts: 10, usable: 10, recover_24h: 0, windows: [{ minutes: 43200, accounts: 10, avg_remaining: 60 }] },
  ];
  const base = { unique_accounts: 100, usable: 100, unknown: 0, invalid: 0, plans };
  const healthy = quotaLogic.overview(quotaSummary({ ...base, advice: { peak_rpm: 90, usable_capacity: 4000, warming_up: false } }));
  assert.equal(healthy.headline, '额度充足，不用加号');
  assert.equal(healthy.meters[0].value, '剩 78%');
  assert.equal(healthy.meters.length, 2);
  assert.equal(healthy.tiles[1].value, '24 小时以上');
  assert.equal(healthy.tiles[2].note, '暂无账号恢复');
  assert.equal(healthy.stateExtra, '');
  const warming = quotaLogic.overview(quotaSummary({ ...base, advice: { warming_up: true } }));
  assert.equal(warming.headline, '暂时不用加号');
  const unknown = quotaLogic.overview(quotaSummary({ ...base, unknown: 30, advice: { low_confidence: true } }));
  assert.equal(unknown.headline, '暂时不用加号，但有账号还没查到额度');
  assert.match(unknown.detail, /30 个账号暂无额度数据/);
});

test('overview folds a low success caveat into the verdict, the peak text and the capacity meter', () => {
  const plans = [{ plan: 'free', accounts: 100, usable: 100, recover_24h: 0, windows: [{ minutes: 43200, accounts: 100, avg_remaining: 80 }] }];
  const base = { unique_accounts: 100, usable: 100, unknown: 0, invalid: 0, plans };
  const caveat = { rate: 0.38, cause: '上游过载严重', effective: 300 };
  // ok -> warn, scaled capacity everywhere.
  const healthy = quotaLogic.overview(quotaSummary({ ...base, advice: { peak_rpm: 410, target_rpm: 492, usable_capacity: 28352, warming_up: false } }), caveat);
  assert.equal(healthy.tone, 'warn');
  assert.equal(healthy.label, '需要留意');
  assert.match(healthy.detail, /上游过载严重：近 5 分钟成功率 38\.0%，实际能承载约 300 次\/分。$/);
  assert.equal(healthy.peakText, '492 / 300 次/分（按成功率折算）');
  const meter = healthy.meters.at(-1);
  assert.equal(meter.status, '不够用');
  assert.equal(meter.percent, 100);
  assert.equal(meter.note, '实际能承载约 300 / 分钟（成功率 38.0%）');
  // Without a caveat the original texts return.
  const plain0 = quotaLogic.overview(quotaSummary({ ...base, advice: { peak_rpm: 410, target_rpm: 492, usable_capacity: 28352, warming_up: false } }));
  assert.equal(plain0.tone, 'ok');
  assert.equal(plain0.peakText, '492 / 28,352 次/分');
  assert.equal(plain0.meters.at(-1).note, '可承载 28,352 / 分钟');
  assert.equal(plain0.states[0].label, '额度够用');
  // warn and danger keep their tone; danger quotes the scaled capacity.
  const warn = quotaLogic.overview(quotaSummary(), caveat);
  assert.equal(warn.tone, 'warn');
  assert.equal(warn.headline, '暂时不用加号，但周额度偏低');
  const danger = quotaLogic.overview(quotaSummary({ advice: { recommended_accounts: 5, rpm_shortfall: 5, usable_capacity: 400, peak_rpm: 410, target_rpm: 492, runway_hours: null } }), caveat);
  assert.equal(danger.tone, 'danger');
  assert.match(danger.detail, /现在只能承载 300 \/ 分钟/);
  // Zero effective capacity is not enough, whatever the peak.
  const zero = quotaLogic.overview(quotaSummary({ ...base, advice: { peak_rpm: 0, usable_capacity: 100 } }), { ...caveat, effective: 0 });
  assert.equal(zero.meters.at(-1).status, '不够用');
  // A moderate peak against the scaled capacity reads tight, not full.
  const tight = quotaLogic.overview(quotaSummary({ ...base, advice: { peak_rpm: 250, usable_capacity: 28352 } }), caveat);
  assert.equal(tight.meters.at(-1).status, '偏紧');
});

const NOW = Date.parse('2026-10-31T00:00:00Z');
const resetRow = (overrides = {}) => ({ remaining: 0, reset_count: 1, account_key: 'a', windows: [], ...overrides });

test('resettable filter keeps credits and leads with the soonest expiry', () => {
  const entries = [
    { name: 'none', provider: 'codex' }, { name: 'later', provider: 'codex' }, { name: 'soon', provider: 'codex' },
    { name: 'open', provider: 'codex' }, { name: 'dead', provider: 'codex' }, { name: 'gem', provider: 'gemini' },
  ];
  const rows = {
    none: resetRow({ reset_count: 0 }), later: resetRow({ reset_expires_at: '2026-11-20T00:00:00Z' }), soon: resetRow({ reset_expires_at: '2026-11-02T00:00:00Z' }),
    open: resetRow(), dead: resetRow({ invalid: true }), gem: resetRow(),
  };
  assert.deepEqual(Array.from(quotaLogic.applyView(entries, rows, 'resettable', ''), (row) => row.name), ['soon', 'later', 'open']);
  assert.deepEqual(Array.from(quotaLogic.applyView(entries, { ...rows, soon: resetRow({ remaining: 50, reset_expires_at: '2026-11-02T00:00:00Z' }) }, 'resettable', 'desc'), (row) => row.name), ['soon', 'later', 'open']);
});

test('cellMarkup offers reset only when a credit exists', () => {
  const windows = [{ minutes: 10080, used_percent: 100 }];
  const html = quotaLogic.cellMarkup(resetRow({ windows, reset_expires_at: '2026-11-06T00:00:00Z' }), NOW, identity, 'a.json');
  assert.match(html, /1 次可重置 · 6 天后过期/);
  assert.match(html, /quota-reset-button" data-name="a\.json"/);
  assert.doesNotMatch(quotaLogic.cellMarkup(resetRow({ windows, reset_count: 0 }), NOW, identity, 'a.json'), /quota-reset/);
  assert.doesNotMatch(quotaLogic.cellMarkup(resetRow({ windows, reset_count: null }), NOW, identity, 'a.json'), /quota-reset/);
  assert.doesNotMatch(quotaLogic.cellMarkup(resetRow({ windows, invalid: true }), NOW, identity, 'a.json'), /quota-reset/);
  assert.match(quotaLogic.cellMarkup(resetRow(), NOW, identity, 'a.json'), /quota-reset-button/);
});

test('resetPlan spends one credit per exhausted account and reports skips', () => {
  const rows = {
    one: resetRow({ account_key: '1' }), oneDup: resetRow({ account_key: '1' }), live: resetRow({ account_key: '2', remaining: 40 }),
    none: resetRow({ account_key: '3', reset_count: 0 }), two: resetRow({ account_key: '4', remaining: null }),
  };
  const plan = quotaLogic.resetPlan(['one', 'oneDup', 'live', 'none', 'two', 'gone'], rows);
  assert.deepEqual(Array.from(plan.targets), ['one', 'two']);
  assert.deepEqual([plan.live, plan.none, plan.duplicates], [1, 2, 1]);
  const message = quotaLogic.resetPlanMessage(plan);
  assert.match(message, /将消耗 2 次主动重置/);
  assert.match(message, /1 个额度未用完，2 个没有重置次数，1 个重复文件/);
});

test('reset confirmation warns before wasting quota', () => {
  const wasteful = quotaLogic.resetConfirmMessage({ name: 'a.json', count: 2, remaining: 62.4, expires_at: '2026-11-06T00:00:00Z' }, NOW);
  assert.match(wasteful, /剩余次数：2/);
  assert.match(wasteful, /最早过期：6 天后过期/);
  assert.match(wasteful, /当前还剩 62%，现在重置会浪费/);
  assert.doesNotMatch(quotaLogic.resetConfirmMessage({ name: 'a.json', count: 1, remaining: 0 }, NOW), /浪费/);
});

test('overview links to resettable accounts only when some exist', () => {
  const later = new Date(Date.now() + 6 * 86400000).toISOString();
  assert.match(quotaLogic.overview(quotaSummary({ reset: { accounts: 5, exhausted: 4, expires_at: later } })).resetText, /5 个账号可重置，其中 4 个额度已用完，最早 .*天后过期/);
  assert.equal(quotaLogic.overview(quotaSummary({ reset: { accounts: 0, exhausted: 0 } })).resetText, '');
});

test('concurrency cell covers limited, unlimited, disabled and inapplicable accounts', () => {
  const limited = { in_flight: 1, concurrency_limit: 2 };
  assert.equal(logic.concurrencyText(limited, true), '1/2');
  assert.equal(logic.concurrencyText({ concurrency_limit: 0 }, true), '0/∞');
  assert.equal(logic.concurrencyText(limited, false), '1 · 未启用');
  assert.equal(logic.concurrencyText({ provider: 'claude' }, true), '—');
});

test('full filter only keeps accounts at an enforced limit', () => {
  const rows = [
    { name: 'full', in_flight: 2, concurrency_limit: 2 },
    { name: 'free', in_flight: 1, concurrency_limit: 2 },
    { name: 'unlimited', in_flight: 9, concurrency_limit: 0 },
    { name: 'other' },
  ];
  const full = logic.filterAccounts(rows, { concurrency: 'full', concurrencyEnabled: true });
  assert.deepEqual(Array.from(full, (row) => row.name), ['full']);
  assert.equal(logic.filterAccounts(rows, { concurrency: 'full', concurrencyEnabled: false }).length, 0);
});

test('concurrency limit input separates inherit, zero and invalid values', () => {
  assert.equal(logic.parseConcurrencyLimit(' '), null);
  assert.equal(logic.parseConcurrencyLimit('0'), 0);
  assert.equal(logic.parseConcurrencyLimit(' 8 '), 8);
  assert.equal(logic.parseConcurrencyLimit('-1'), undefined);
  assert.equal(logic.parseConcurrencyLimit('1.5'), undefined);
});

test('batch result reports success and failure counts', () => {
  const message = logic.concurrencyBatchMessage({ succeeded: ['a'], unchanged: [], failed: [{ name: 'b', error: 'auth file not found' }] });
  assert.equal(message, '成功 1 个，未变 0 个，失败 1 个：b（auth file not found）');
});

test('speed view summarises 24h labels in a fixed order', () => {
  const ctx = vm.createContext({ Date, Math, String, Number, Array, Object, JSON, isNaN, isFinite });
  vm.runInContext(logicBlock('SPEED_VIEW_LOGIC') + ';this.speed = SpeedViewLogic;', ctx);
  const s = ctx.speed;
  assert.equal(s.labelSummary({ slower: 1, effective: 3 }), '近 24 小时结论：有效 3 小时 · 更慢 1 小时');
  assert.equal(s.labelSummary({}), '近 24 小时还没有完整小时的数据');
  assert.equal(s.seconds(null), '—');
  assert.equal(s.seconds(1520), '1.52 s');
  assert.equal(s.failRate({ requests: 0, failures: 0 }), '—');
  assert.equal(s.failRate({ requests: 8, failures: 2 }), '25.0%');
});

test('speed view series appends the current hour and keeps gaps as null', () => {
  const ctx = vm.createContext({ Date, Math, String, Number, Array, Object, JSON, isNaN, isFinite });
  vm.runInContext(logicBlock('SPEED_VIEW_LOGIC') + ';this.speed = SpeedViewLogic;', ctx);
  const g = (p50, req, fail) => ({ p50_ms: p50, p90_ms: p50, requests: req, failures: fail });
  const series = plain(ctx.speed.fastSeries({
    hours: [{ hour: '2026-11-02T06:00:00Z', fast: g(1000, 10, 1), control: g(null, 0, 0) }],
    current: { hour: '2026-11-02T07:00:00Z', fast: g(2000, 4, 0), control: g(9000, 20, 5) },
  }));
  assert.equal(series.times.length, 2);
  assert.deepEqual(series.fastP50, [1, 2]);
  assert.deepEqual(series.controlP50, [null, 9]);
  assert.deepEqual(series.fastFail, [10, 0]);
  assert.deepEqual(series.controlFail, [null, 25]);
});

test('speed fast cards report current hour and shares', () => {
  const ctx = vm.createContext({ Date, Math, String, Number, Array, Object, JSON, isNaN, isFinite });
  vm.runInContext(logicBlock('SPEED_VIEW_LOGIC') + ';this.speed = SpeedViewLogic;', ctx);
  const cards = plain(ctx.speed.fastCards({ enable: true, percent: 97, effective_percent: 48, members: 1,
    current: { label: 'no_difference', fast: { p50_ms: 9670, ttft_samples: 30, requests: 50, failures: 5 }, control: { p50_ms: 10460, ttft_samples: 60, requests: 80, failures: 20 } } }));
  const map = Object.fromEntries(cards);
  assert.equal(map['配置占比 / 运行占比'], '97% / 48%');
  assert.equal(map['本小时结论'], '无差异');
  assert.equal(map['本小时样本（fast / 对照）'], '30 / 60');
});

test('speed headroom grades, snapping and status text', () => {
  const ctx = vm.createContext({ Date, Math, String, Number, Array, Object, JSON, isNaN, isFinite });
  vm.runInContext(logicBlock('SPEED_VIEW_LOGIC') + ';this.speed = SpeedViewLogic;', ctx);
  const s = ctx.speed;
  assert.equal(s.headroom(null).text, '—');
  assert.equal(s.headroom(0.049).text, '充足');
  assert.equal(s.headroom(0.05).text, '偏紧');
  assert.equal(s.headroom(0.2).text, '偏紧');
  assert.equal(s.headroom(0.21).cls, 'speed-fail');
  assert.equal(s.snapPercent(29.6), 30);
  assert.equal(s.snapPercent(0.2), 1);
  assert.equal(s.snapPercent(95), 90);
  assert.equal(s.snapPercent('x'), 30);
  const base = { pools_configured: true, pools_running: true, enable: true };
  assert.ok(s.statusText({ config: base, live: { ready: false, scored: 7, ready_min: 20 } }).includes('已有分数 7 个，需要 20 个'));
  assert.ok(s.statusText({ config: base, live: { ready: true, probe_behind: true, probe_cycle_minutes: 31.6 } }).includes('一轮约 32 分钟'));
  assert.ok(s.statusText({ config: { pools_configured: false }, live: {} }).startsWith('Codex 分池未开启'));
});

test('speed bins become chart points with null gaps and fast share', () => {
  const ctx = vm.createContext({ Date, Math, String, Number, Array, Object, JSON, isNaN, isFinite });
  vm.runInContext(logicBlock('SPEED_VIEW_LOGIC') + ';this.speed = SpeedViewLogic;', ctx);
  const b = plain(ctx.speed.binSeries([
    { accounts: 4, fast: 4, score_ms: 900, util: 0.5 },
    { accounts: 4, fast: 1, score_ms: null, util: null },
    { accounts: 0, fast: 0, score_ms: null, util: null }
  ]));
  assert.deepEqual(b.score, [[1, 0.9], [3, null], [5, null]]);
  assert.deepEqual(b.util, [[1, 50], [3, null], [5, null]]);
  assert.deepEqual(b.fast, [[1, 100], [3, 25], [5, null]]);
});

test('speed trend keeps legacy hours to the overall line', () => {
  const ctx = vm.createContext({ Date, Math, String, Number, Array, Object, JSON, isNaN, isFinite });
  vm.runInContext(logicBlock('SPEED_VIEW_LOGIC') + ';this.speed = SpeedViewLogic;', ctx);
  const now = Date.now();
  const iso = (h) => new Date(now - h * 3600 * 1000).toISOString();
  const g = (p50) => ({ p50_ms: p50 });
  const series = plain(ctx.speed.trendSeries({
    hours: [
      { hour: iso(30), schema: 'ranked', fast: g(1), slow: g(1), overall: g(1) },
      { hour: iso(3), schema: 'legacy_ab', fast: g(null), slow: g(null), overall: g(9000), fast_util: 0.5, overflow_share: 0.5, fast_percent: 0 },
      { hour: iso(2), schema: 'ranked', fast: g(1200), slow: g(8000), overall: g(2000), fast_util: 0.4, overflow_share: 0.031, fast_percent: 30 }
    ],
    live: { current: { hour: iso(0), schema: 'ranked', fast: g(null), slow: g(null), overall: g(null), fast_util: null, overflow_share: null, fast_percent: 30 } }
  }));
  assert.equal(series.times.length, 3, 'hours older than 24h are dropped');
  assert.ok(series.times[0].endsWith('（旧口径）'));
  assert.deepEqual(series.overallP50, [9, 2, null]);
  assert.deepEqual(series.fastP50, [null, 1.2, null]);
  assert.deepEqual(series.overflow, [null, 3.1, null]);
  assert.deepEqual(series.fastPercent, [null, 30, 30]);
});

function poolView() {
  const ctx = vm.createContext({ Date, Math, String, Number, Array, Object, JSON, isNaN, isFinite });
  vm.runInContext(logicBlock('SPEED_VIEW_LOGIC') + ';' + logicBlock('POOL_VIEW_LOGIC') + ';this.view = PoolViewLogic;', ctx);
  return ctx.view;
}
const bin = (score_ms, accounts = 10, fast = 0, util = 0.1) => ({ score_ms, accounts, fast, util });

test('pool view grades speed and counts true fast accounts', () => {
  const v = poolView();
  assert.equal(v.speedTone(null), null);
  assert.equal(v.speedTone(1499), 'ok');
  assert.equal(v.speedTone(1500), 'warn');
  assert.equal(v.speedTone(3999), 'warn');
  assert.equal(v.speedTone(4000), 'bad');
  assert.equal(v.trueFastCount([bin(300, 12), bin(1499, 5), bin(1500, 7), bin(null, 9)]), 17);
  assert.equal(v.trueFastCount(undefined), 0);
});

test('pool view finds the first cliff and explains when there is none', () => {
  const v = poolView();
  assert.deepEqual(plain(v.cliff([bin(400), bin(600), bin(5000), bin(9000)], true, 100, 100)), { pos: 4, percent: 4, fromMs: 600, toMs: 5000 });
  // A doubling below 1.5 seconds is not a cliff; neither is a big gap that does not double.
  assert.ok(v.cliff([bin(300), bin(700), bin(5000), bin(8000)], true, 100, 100).percent === 4);
  // The edge is a board position over all accounts; half of them scored doubles the fast percent.
  assert.equal(v.cliff([bin(400), bin(600), bin(5000)], true, 50, 100).percent, 8);
  assert.ok(v.cliff([bin(300), bin(700)], true).reason.includes('没有明显断崖'));
  assert.ok(v.cliff([bin(4000), bin(7000)], true).reason.includes('没有明显断崖'));
  // Unscored bins never form a cliff with their neighbours.
  assert.ok(v.cliff([bin(500), bin(null), bin(9000)], true).reason.includes('没有明显断崖'));
  assert.equal(v.cliff([bin(400), bin(5000)], false).reason, '测速还没就绪');
  const late = Array.from({ length: 50 }, (_, i) => bin(i < 48 ? 500 : 9000));
  // The true edge (96%) is kept and only the converted percent is snapped to 90.
  assert.equal(v.cliff(late, true, 100, 100).pos, 96);
  assert.equal(v.cliff(late, true, 100, 100).percent, 90);
  assert.equal(v.cliff(late, true, 0, 100).percent, null);
});

test('pool view cut readout counts slow accounts inside the fast group', () => {
  const v = poolView();
  const bins = [bin(500, 10), bin(1200, 10), bin(2000, 10), bin(null, 10)];
  const r = plain(v.cutReadout(bins, 6, 200, 4, 200));
  assert.deepEqual(r, { cut: 6, accounts: 12, edgeMs: 2000, slowInFast: 10, trueFast: 20, dirty: true });
  assert.equal(v.cutReadout(bins, 4, 200, 4, 200).slowInFast, 0);
  assert.equal(v.cutReadout(bins, 4, 200, 4, 200).dirty, false);
  assert.equal(v.cutReadout([], 30, 0, 30, 0).edgeMs, null);
  // The target counts scored accounts only, and the covered bins shrink with them.
  const half = plain(v.cutReadout(bins, 4, 200, 4, 100));
  assert.equal(half.accounts, 4);
  assert.equal(half.edgeMs, 500);
  // A cut covering less than half a bin has no data behind a verdict.
  assert.equal(v.cutReadout(bins, 28, 1620, 28, 20).edgeMs, null);
  assert.equal(v.cutReadout([bin(null, 10), bin(500, 10)], 2, 200, 2, 200).edgeMs, null);
});

test('pool view peak keeps the latest of equal maxima and ignores empty minutes', () => {
  const v = poolView();
  const pts = [{ time: 'a', attempts: 5 }, { time: 'b', attempts: 9 }, { time: 'c', attempts: 9 }, { time: 'd', attempts: 1 }];
  assert.equal(v.peak(pts, 'attempts').time, 'c');
  assert.equal(v.peak([{ time: 'a', attempts: 0 }], 'attempts'), null);
  assert.equal(v.peak(undefined, 'attempts'), null);
  assert.deepEqual(plain(v.gauge(25, 100, 120)), { used: 25, peak: 120, usedBar: 25, peakMark: 99.5 });
  assert.deepEqual(plain(v.gauge(5, 0, 3)), { used: 0, peak: 0, usedBar: 0, peakMark: 0 });
});

test('pool view capacity answer grades success and splits cooling reasons', () => {
  const v = poolView();
  const a = plain(v.capacityAnswer({ points: [{ time: '2026-11-03T08:36:00Z', success: 127, other_failures: 8 }], success_rate: 0.9406, rpm: 293, capacity: 33194, in_flight: 56, concurrency_enabled: true, concurrency_capacity: 3280, cooling: 658, cooling_reasons: { credential_quota: 657, cooldown: 1 } }));
  assert.equal(a.tone, 'warn');
  assert.equal(a.label, '成功率偏低');
  assert.equal(a.verdict, '近 5 分钟成功率 94.1%');
  assert.deepEqual(a.rows, [['在途并发', '56 / 3,280'], ['冷却中（额度用完 / 其他）', '658（657 / 1）']]);
  assert.equal(v.successTone(0.95).label, '稳定');
  assert.equal(v.successTone(0.8499).tone, 'danger');
  assert.equal(v.successTone(undefined).tone, 'off');
  assert.equal(v.capacityAnswer({ concurrency_enabled: true, concurrency_capacity: 10, unlimited_accounts: 2 }).rows[0][1], '0 / 10 + ∞×2');
});

test('pool view capacity answer hides an unenforced concurrency limit and lists invalid accounts', () => {
  const v = poolView();
  // Production 2026-11-03 17:03: limit disabled, default 1, 341 dead tokens split from cooling.
  const a = plain(v.capacityAnswer({ in_flight: 87, concurrency_enabled: false, concurrency_capacity: 1392, cooling: 563, cooling_reasons: { credential_quota: 563 }, invalid: 341 }));
  assert.deepEqual(a.rows, [['在途并发', '87（未限制）'], ['冷却中（额度用完 / 其他）', '563（563 / 0）'], ['失效或不可用（需处理）', '341', 'danger']]);
  assert.equal(plain(v.capacityAnswer({ invalid: 0 })).rows.length, 2);
});

test('pool view overload caveat needs recent attempts below 80% success and names the main failure', () => {
  const v = poolView();
  const pts = (o) => [{ time: '2026-11-03T08:36:00Z', success: 38, ...o }];
  assert.equal(v.overloadCaveat(null), null);
  assert.equal(v.overloadCaveat({ success_rate: 0.5, points: [] }), null);
  assert.equal(v.overloadCaveat({ success_rate: 0.8, capacity: 100, points: pts({ overload: 10 }) }), null);
  assert.equal(v.overloadCaveat({ home_mode: true, success_rate: 0.3, points: pts({ overload: 60 }) }), null);
  assert.equal(v.overloadCaveat({ success_rate: null, points: pts({ overload: 60 }) }), null);
  assert.deepEqual(plain(v.overloadCaveat({ success_rate: 0.38, capacity: 28393, points: pts({ overload: 60, rate_limits: 2 }) })), { rate: 0.38, cause: '上游过载严重', effective: 10789 });
  assert.equal(v.overloadCaveat({ success_rate: 0.5, capacity: 10, points: pts({ rate_limits: 9, overload: 1 }) }).cause, '上游限流');
  assert.equal(v.overloadCaveat({ success_rate: 0.5, capacity: 10, points: pts({ quota_exhausted: 9 }) }).cause, '额度用完');
  assert.equal(v.overloadCaveat({ success_rate: 0.5, capacity: 10, points: pts({ other_failures: 9 }) }).cause, '请求失败多');
  assert.equal(v.overloadCaveat({ success_rate: 0.5, points: pts({ overload: 9 }) }).effective, 0);
});

test('pool view speed answer follows the overflow grade only while ranking runs', () => {
  const v = poolView();
  const sp = { config: { enable: true }, live: { active: true, ready: true, fast: 328, slow: 1310, unscored: 269, overflow_share: 0.267, fast_p50_ms: 3052, slow_p50_ms: 3619, overall_p50_ms: 3378, bins: [bin(500, 30), bin(1600, 30)] } };
  const a = plain(v.speedAnswer(sp));
  assert.equal(a.tone, 'danger');
  assert.equal(a.label, '快号不够');
  assert.equal(a.verdict, '快组 3.1 秒，慢组 3.6 秒');
  assert.deepEqual(a.rows[0], ['本小时请求掉进慢组', '26.7%', 'danger']);
  assert.equal(a.rows[1][1], '328 / 1,310 / 269');
  assert.equal(a.rows[2][1], '约 30 个');
  const warming = plain(v.speedAnswer({ config: {}, live: { active: true, ready: false } }));
  assert.equal(warming.tone, 'off');
  assert.equal(warming.label, '测速中');
  assert.equal(warming.rows[2][1], '—');
  assert.equal(v.speedAnswer({ config: {}, live: { active: false } }).label, '未生效');
});

test('pool view converts the cut between scored percent and board position', () => {
  const v = poolView();
  assert.equal(v.cutPos(28, 961, 1620), 28 * 961 / 1620);
  assert.equal(v.cutFromPos(v.cutPos(28, 961, 1620), 961, 1620), 28);
  assert.equal(v.cutPos(30, 0, 1620), 0);
  assert.equal(v.cutFromPos(10, 0, 1620), null);
  assert.equal(v.cutFromPos(10, 5, 0), null);
  // One or 19 scored accounts still convert, so a cut can be preset before ranking is ready.
  assert.equal(v.cutFromPos(0.05, 1, 1620), 81);
  assert.equal(v.cutFromPos(50, 19, 1620), 90);
  assert.equal(v.cutFromPos(0.2, 19, 1620), 17);
});

test('pool view board shares one cut across stairs, members and tanks', () => {
  const v = poolView();
  const used = (b, share, n, q) => ({ ...b, used_share: share, used: n, requests: q });
  const sp = { live: { total: 1000, unscored: 10, bins: [used(bin(300, 20, 20), 0.5, 10, 31), used(bin(2000, 20, 5), null, 0, 0), used(bin(null, 20, 0), 0.002, 0, 1), used(bin(4000, 20, 0, 0.9), 0, 0, 0)] } };
  const o = plain(v.boardOption(sp, 20, 1000));
  assert.equal(o.series.length, 4);
  assert.deepEqual(o.series[0].data.map(d => d.value), [[1, 0.3], [3, 2], [5, 12], [7, 4]]);
  assert.deepEqual(o.series[0].data.map(d => d.itemStyle.color), ['#3ccf8e', '#f2a93b', '#2a3442', '#f2626a']);
  // Water is the live used share; the old capacity util (0.9 in the last bin) is ignored.
  assert.deepEqual(o.series[2].data, [[1, 50], [3, 0], [5, 0.6], [7, 0]]);
  assert.deepEqual(o.series[3].data, [[1, 50], [3, 100], [5, 99.4], [7, 100]]);
  assert.equal(o.series[2].name, '最近 1 分钟用过');
  for (const i of [0, 1, 2]) assert.equal(o.series[i].markLine.data[0].xAxis, 0);
  // fast percent 20 of 500 scored accounts out of 1000 sits at rank position 10.
  const scored = plain(v.boardOption({ live: { ...sp.live, scored: 500 } }, 20, 1000));
  for (const i of [0, 1, 2]) assert.equal(scored.series[i].markLine.data[0].xAxis, 10);
  assert.deepEqual(scored.series[0].markArea.data[0].map(p => p.xAxis), [0, 10]);
  assert.equal(scored.series[0].markLine.label.formatter, '快组目标 20%');
  assert.equal(o.xAxis[2].interval, 20);
  const narrow = plain(v.boardOption(sp, 20, 360));
  assert.equal(narrow.xAxis[2].interval, 50);
  assert.ok(plain(v.boardOption({ live: {} }, 30, 800)).graphic.some(g => g.style.text === '还没有测速数据'));
  const fmt = v.boardOption(sp, 20, 1000).tooltip.formatter;
  assert.ok(fmt({ value: [1, 50] }).endsWith('最近 1 分钟用过 10 / 20 个号（含测速），共 31 次请求'));
  assert.ok(fmt({ value: [3, 0] }).endsWith('最近 1 分钟使用：—'));
});

test('pool view request chart splits failures and the capacity sentence replaces the old chart', () => {
  const v = poolView();
  const t1 = '2026-11-03T08:35:00+08:00', t2 = '2026-11-03T08:36:00+08:00';
  const d = {
    capacity: 1000, schedulable: 50, concurrency_capacity: 100, success_rate: 0.95,
    points: [{ time: t1, attempts: 100, capacity: 1000, success: 90, overload: 4, rate_limits: 3, quota_exhausted: 2, other_failures: 1 }, { time: t2, attempts: 600, capacity: 1000, success: 600 }],
    concurrency_points: [{ time: t2, in_flight_peak: 70, concurrency_capacity: 100, concurrency_full_accounts: 4, concurrency_rejections: 2 }],
  };
  assert.equal(v.capacityOption, undefined);
  const req = plain(v.requestsOption(d));
  assert.deepEqual(req.series.map(s => s.name), ['成功', '过载 502/503', '429 限流', '额度用完', '其他']);
  assert.deepEqual(req.series.map(s => s.data[0]), [90, 4, 3, 2, 1]);
  assert.ok(req.series.every(s => s.stack === 'req'));
  assert.equal(v.rejections(d), 2);
  // Concurrency is mentioned only when its limit is enforced.
  assert.equal(v.capacityRead(d), '最近 60 分钟请求最多用到 RPM 上限的 60%，已经接近上限，考虑加号。');
  assert.equal(v.capacityRead({ ...d, concurrency_enabled: true }), '最近 60 分钟请求最多用到 RPM 上限的 60%，并发最多用到 70%，已经接近上限，考虑加号。');
  assert.equal(v.capacityRead({ ...d, points: [{ time: t1, attempts: 10, capacity: 1000, success: 10 }] }), '最近 60 分钟请求最多用到 RPM 上限的 1%，号池容量还很空。');
  // R uses each minute's own capacity: 300 of 400 then, even though capacity is 1000 now.
  assert.ok(v.capacityRead({ ...d, points: [{ time: t1, attempts: 300, capacity: 400, success: 300 }] }).includes('RPM 上限的 75%'));
  assert.equal(v.capacityRead({ ...d, capacity: 0 }), '现在没有可调度的容量（可调度账号 50 个）。');
  assert.equal(v.capacityRead({ capacity: 1000, points: [{ time: t1, attempts: 0, capacity: 1000 }] }), '');
  assert.equal(v.capacityRead({}), '');
  assert.equal(v.minuteKey('not a time'), null);
  assert.deepEqual(plain(v.commonLimit({ limits: [{ limit: 20, accounts: 5 }, { limit: 21, accounts: 9 }] })), { limit: 21, accounts: 9 });
});

test('pool view capacity sentence blames the main failure when success is low', () => {
  const v = poolView();
  // Production 2026-11-03 17:03: 2% of RPM used, 38% success, mostly overloads.
  const d = { capacity: 28393, success_rate: 0.38, points: [{ time: '2026-11-03T08:36:00Z', attempts: 528, capacity: 28393, success: 41, overload: 55 }] };
  assert.equal(v.capacityRead(d), '最近 60 分钟请求最多用到 RPM 上限的 2%。限制用户的不是容量，是上游过载严重：近 5 分钟成功率只有 38.0%。');
});

test('pool view keeps local counters out of the Home mode capacity answer', () => {
  const v = poolView();
  const a = plain(v.capacityAnswer({ home_mode: true, success_rate: 0.5, rpm: 10, capacity: 20 }));
  assert.equal(a.tone, 'off');
  assert.equal(a.verdict, '容量由 Home 调度端管理');
  assert.equal(a.big, '—');
  assert.equal(v.capacityRead({ home_mode: true, capacity: 10, points: [{ time: 'x', attempts: 9 }] }), '');
});

test('pool view does not grade success when the last five minutes had no requests', () => {
  const v = poolView();
  const idle = plain(v.capacityAnswer({ success_rate: 0, points: [{ time: '2026-11-03T08:00:00Z', success: 0 }, { time: '2026-11-03T08:01:00Z' }] }));
  assert.equal(idle.tone, 'off');
  assert.equal(idle.label, '暂无请求');
  assert.equal(idle.verdict, '近 5 分钟没有请求');
  // Only the five newest minutes count, whatever order the points arrive in.
  const pts = [6, 5, 4, 3, 2, 1].map((m, i) => ({ time: '2026-11-03T08:0' + m + ':00Z', success: i === 5 ? 50 : 0 }));
  assert.equal(v.recentAttempts({ points: pts }), 0);
  assert.equal(v.recentAttempts({ points: pts.slice().reverse() }), 0);
  const busy = plain(v.capacityAnswer({ success_rate: 0, points: [{ time: '2026-11-03T08:01:00Z', other_failures: 3 }] }));
  assert.equal(busy.tone, 'danger');
});
