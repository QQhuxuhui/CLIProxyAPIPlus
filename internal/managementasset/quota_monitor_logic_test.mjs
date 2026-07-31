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

test('statusError falls back to status when the structured body carries no code', () => {
  const body = JSON.stringify({ error: { status: 'UNAVAILABLE' } });
  assert.deepEqual(plain(logic.statusError({ status_message: body })), { text: 'UNAVAILABLE', tip: 'UNAVAILABLE' });
  assert.deepEqual(plain(logic.statusError({ status_message: JSON.stringify({ error: {} }) })), {
    text: 'error',
    tip: JSON.stringify({ error: {} }),
  });
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
