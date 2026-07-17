# Model Call Statistics Chart — Design

**Status:** Approved by user (2026-07-16). Ready for implementation planning.

## Goal

Add a "模型调用统计" section to the quota monitor page that shows per-model call
counts as a horizontal stacked bar chart, filterable by time range.

## Current State

- `internal/usagestats` already persists per-day, per-account, per-model
  success/fail counts as day JSON files under `usage-stats/` (gated by
  `usage-stats-enabled`, swept after `usage-stats-retention-days`, default 90).
- `/v0/management/usage-stats?from=&to=&account=` (management auth) returns
  `{from, to, accounts: [{account, models: [{model, success, fail, total}]}]}`.
  Omitting `account` returns all accounts. The per-account usage modal on the
  quota monitor page already consumes this endpoint.
- The page is a single embedded HTML file (`internal/managementasset/quota_monitor.html`)
  with no chart library; it already has a delegated `data-tip` hover tooltip
  (`#hover-tip`) and a DOM-free `AccountViewLogic` block covered by
  `quota_monitor_logic_test.mjs`.

**Backend changes: none.** This is a frontend-only feature.

## Design

### Data flow

On load and on time-filter change, fetch `/v0/management/usage-stats?from=&to=`
(no `account` param). Aggregate client-side: sum `success`/`fail` per model
name across all accounts, sort by total descending. The aggregation function is
DOM-free and lives in a marked logic block so it is testable under node.

### UI

A new white-card section between the account list card and the model health
card, matching the existing card style:

- Header: title 模型调用统计 + time presets 今天 / 昨天 / 近7天 (default) /
  自定义 (from/to date inputs + 应用 button), mirroring the usage modal's
  preset interaction.
- Chart: one row per model, rendered with plain CSS (no chart library):
  - Left: model name, fixed width, truncated with `data-tip` full name on hover.
  - Middle: horizontal stacked bar — green segment for success, red for fail —
    width proportional to the largest model total in the current result.
  - Right: total count text.
  - Hovering the bar shows `成功 x · 失败 y` via the existing `#hover-tip`
    delegated tooltip.
- Empty result: 该时间段无调用记录.
- `usage-stats-enabled` off (HTTP 503): show the same guidance text as the
  usage modal (enable `usage-stats-enabled: true` and restart/reload).

### Refresh behavior

- Fetch on first successful page load and whenever the time filter changes.
- Re-fetch with the currently selected range inside the existing 15s
  `fetchData` auto-refresh cycle (the endpoint reads memory + local files;
  cost is negligible).

### Error handling

- 503 → guidance text (feature disabled), section body hidden.
- Other non-OK / network errors → inline error text in the section, keep last
  rendered chart if any.

## Testing

- `quota_monitor_test.go`: structural markers — section element id, preset
  button ids, `/v0/management/usage-stats` fetch without `account` param,
  stacked-bar class names, aggregation function marker.
- `quota_monitor_logic_test.mjs`: unit tests for the DOM-free aggregation —
  multi-account merge for the same model, sort by total desc, empty input,
  fail-only counts, bar-width scaling against the max total.
- Manual browser smoke check: presets, custom range, tooltip detail, 503 path.

## Files to change

- `internal/managementasset/quota_monitor.html` — new section markup, CSS,
  fetch/aggregate/render logic.
- `internal/managementasset/quota_monitor_test.go` — structural markers.
- `internal/managementasset/quota_monitor_logic_test.mjs` — aggregation tests.

## Out of scope

- Per-day trend chart (data supports it; can be a follow-up).
- Backend changes of any kind.
- Deployment concern noted during review: `usage-stats/` is relative to the
  container working directory (`/CLIProxyAPI/usage-stats`) and must be
  volume-mounted to survive container recreation. Operational, not code.
