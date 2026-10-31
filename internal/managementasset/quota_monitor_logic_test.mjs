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
test('Codex pool charts distinguish rate limits and exhausted quota', () => {
  const charts = plain(context.CodexPoolLogic.chartOptions({
    points: [{time:'2026-10-22T12:00:00Z',attempts:40,capacity:100,ttft_p50:7,ttft_p90:10,overload:3,rate_limits:2,quota_exhausted:1,other_failures:4}],
    limits: [{limit:20,accounts:8}],
  }));
  assert.equal(charts.length, 4);
  assert.deepEqual(charts[0].series.map(s=>s.data), [[40],[100]]);
  assert.deepEqual(charts[2].series.map(s=>s.data), [[3],[2],[1],[4]]);
  assert.equal(charts[2].series.every(s=>s.stack==='fail'), true);
  assert.deepEqual(charts[3].xAxis.data, [20]);
  assert.deepEqual(charts[3].series[0].data, [8]);
});
test('Codex pool cards preserve negative remaining capacity and missing TTFT', () => {
  const cards=plain(context.CodexPoolLogic.cards({remaining:-3,success_rate:.75,ttft_p50:0,ttft_p90:0}));
  assert.equal(cards.find(c=>c[0]==='剩余容量 / RPM')[1], -3);
  assert.equal(cards.find(c=>c[0]==='近 5 分钟成功率')[1], '75.0%');
  assert.equal(cards.find(c=>c[0]==='首字 p50')[1], '—');
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
    document:{getElementById:element,createElement:()=>({remove(){},appendChild(){}}),head:{appendChild:script=>scripts.push(script)}},
    window:{addEventListener(){}},setInterval:()=>1,clearInterval(){},fetch:()=>saveResponse,
    readError:()=>Promise.resolve('failed'),clearKey(){},setStatus(){}
  });
  vm.runInContext(logicBlock('CODEX_POOL_LOGIC'), sandbox);
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
