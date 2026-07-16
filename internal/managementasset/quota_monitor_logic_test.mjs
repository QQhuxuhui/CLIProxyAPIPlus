import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const html = readFileSync(new URL('./quota_monitor.html', import.meta.url), 'utf8');
const startMarker = '/* ACCOUNT_VIEW_LOGIC_START */';
const endMarker = '/* ACCOUNT_VIEW_LOGIC_END */';
const start = html.indexOf(startMarker);
const end = html.indexOf(endMarker);

assert.notEqual(start, -1, 'account-view logic start marker is missing');
assert.notEqual(end, -1, 'account-view logic end marker is missing');
assert.ok(end > start, 'account-view logic markers are out of order');

const context = vm.createContext({ Date, Math, String, Number, Array, Object, isNaN });
vm.runInContext(html.slice(start + startMarker.length, end), context);
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
