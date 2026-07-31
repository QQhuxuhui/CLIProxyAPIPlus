# Model Call Statistics Chart - Design

**Status:** Approved and implemented on 2026-07-17.

## Goal

Add a dedicated "模型调用统计" tab to the quota monitor page. It shows call
counts aggregated by exact model name as an accessible horizontal stacked bar
chart, filterable by a server-calendar date range.

## Current State

- `internal/usagestats` persists per-day, per-account, per-model success/fail
  counts as JSON files under `usage-stats/`. Recording is gated by
  `usage-stats-enabled`; files are swept according to
  `usage-stats-retention-days`, which defaults to 90.
- `/v0/management/usage-stats?from=&to=&account=` is protected by management
  authentication and returns
  `{from, to, accounts: [{account, models: [{model, success, fail, total}]}]}`.
  Omitting `account` returns all accounts. The per-account usage modal already
  consumes this endpoint.
- The page is a single embedded HTML file,
  `internal/managementasset/quota_monitor.html`, with no chart library. It has a
  delegated `data-tip` tooltip (`#hover-tip`), an `escapeHtml` helper, and a
  DOM-free `AccountViewLogic` block covered by
  `quota_monitor_logic_test.mjs`.
- The account list and model-health table are separate tabs. There is no
  meaningful location "between" their cards in the current DOM.
- Usage day buckets and API ranges use the server's `time.Local`, while the
  existing modal constructs presets from the browser's calendar. Those dates
  can differ when the browser and server use different time zones.
- A range query loads each non-resident day from disk. Repeating an unrestricted
  custom range every 15 seconds would create avoidable disk work.

## Design

### Navigation and loading

Add a fourth top-level tab, ordered as:

1. 账号统计
2. 调用统计
3. 模型健康
4. 代理设置

The new `tab-stats` panel contains one white data card and follows the existing
page width, border, and shadow conventions. It is not nested inside another
card.

Statistics are lazy-loaded the first time the user opens the tab. Reopening the
tab fetches again when the current data is absent or at least 60 seconds old,
regardless of the selected range. This user-triggered activation rule is
separate from background auto-refresh and ensures server-relative presets such
as yesterday advance after midnight. It also avoids loading an all-account
dataset for users who never view the chart.

### Usage-stats API range contract

Extend the existing endpoint without breaking current `from`/`to` callers:

- Accept an optional `preset` query parameter with values `today`, `yesterday`,
  or `7d`. The server calculates these inclusive ranges using `time.Local`.
- `preset` is mutually exclusive with explicit `from` or `to`; mixed input
  returns HTTP 400.
- Existing requests without `preset` retain their current behavior. Empty dates
  default to the server's current day, and explicit dates are inclusive.
- Built-in presets are always accepted at their fixed lengths, including `7d`
  when retention is configured below seven days; missing older files simply
  contribute zero counts. Reject explicit `from`/`to` ranges whose inclusive
  length exceeds the configured `usage-stats-retention-days`. If the configured
  value is invalid or zero, use the existing default of 90 days for validation.
  This keeps the default preset usable while bounding arbitrary disk work.
- Add `server_today` (`YYYY-MM-DD`) and `server_utc_offset` (`+08:00`, `+00:00`,
  and so on) to successful responses. Existing response fields remain
  unchanged.

Range resolution lives in a small helper that accepts the preset/date strings,
a reference `time.Time`, and the maximum inclusive custom-range day count. The
handler passes the current server time; tests pass a fixed time so preset and
UTC-offset tests cannot fail at midnight.

Preset controls in both the new chart and the existing per-account modal use
`preset`, so "今天", "昨天", and "近7天" consistently mean the server's
calendar dates. Custom inputs remain explicit server-calendar dates and are
labelled `按服务端日期` in the UI.

No changes are required in `internal/usagestats`, its persisted JSON format, or
the aggregation architecture.

### Data aggregation

The chart requests `/v0/management/usage-stats?preset=7d` by default, without
an `account` parameter. For a custom range it sends encoded `from` and `to`
parameters.

A DOM-free `ModelStatsLogic` block:

- sums `success` and `fail` across accounts for each exact model string;
- calculates `total` from the summed values rather than trusting a separately
  summed total;
- removes zero-total rows;
- sorts by `total` descending and then by model name ascending for deterministic
  ties; and
- calculates the outer bar width against the largest model total and each
  success/fail segment against that row's total.

Grouping by exact model string is intentional. Provider separation, alias
normalization, and thinking-suffix normalization are not part of this feature.
The logic block has explicit start/end markers so Node tests can evaluate it
without a DOM.

### Time controls

The card header contains the title, a compact success/fail legend, and a
segmented range control:

- 今天
- 昨天
- 近7天, active by default
- 自定义

Preset buttons fetch immediately and expose their selected state with
`aria-pressed`. Selecting 自定义 reveals `from` and `to` date inputs plus an
`应用` button. Applying requires both dates and `from <= to`; the backend remains
authoritative for the retention limit. The response's `from` and `to` values
are displayed above the chart so users can see the server-resolved range.

### Chart

Render the chart with local HTML and CSS only:

- Each row contains the model name, a horizontal track, and a numeric summary.
- The model column is fixed-width on desktop and truncates long values. Its
  focusable `data-tip` target exposes the full escaped name.
- The filled portion of the track is proportional to the largest total in the
  result. Within it, green and red segments represent the row's success/fail
  proportions.
- The numeric summary visibly shows total, success, and fail counts. Color and
  hover are therefore not the only ways to obtain the values.
- The focusable bar has an `aria-label` and `data-tip` containing
  `成功 x · 失败 y · 合计 z`. Extend the delegated tooltip behavior to keyboard
  focus as well as pointer hover. Visible numbers preserve the information on
  touch devices where hover is unavailable.
- Format displayed counts with the browser locale, while keeping raw numbers in
  the aggregation logic.
- Keep all models available. When the list exceeds the card's 560px maximum
  chart height, scroll the row area vertically while leaving the header and
  range summary visible.

On narrow screens, each row becomes two lines: model name and total on the first
line, then a full-width bar with success/fail counts on the second. Fixed model
widths must not squeeze the bar or force horizontal page scrolling.

All model names and tooltip attributes must be written with DOM `textContent`
or the existing `escapeHtml` helper before insertion into `innerHTML`. Model
names originate outside the management page and must be treated as untrusted.

### Refresh and request ordering

The existing account/model-health refresh cadence remains 15 seconds when auto
refresh is enabled. Model statistics use a separate effective cadence of 60
seconds because call-count visualization does not need quota-level freshness.

An automatic model-statistics refresh runs only when all of these are true:

- the global 自动刷新 checkbox is enabled;
- the 调用统计 tab is visible;
- the selected preset or custom range includes the server's current day; and
- at least 60 seconds have elapsed since the last model-statistics request.

Yesterday and fully historical custom ranges do not auto-refresh. Presets are
sent back to the server on every refresh so their date windows advance at server
midnight. The top-level 刷新 button always refreshes statistics when the
statistics tab is visible, regardless of the range. Switching into a stale
statistics tab also refreshes regardless of the range, as described under
Navigation and loading.

Every statistics request receives an increasing generation number. Starting a
new request aborts the previous request when `AbortController` is available.
Only a response whose generation is still current may update status, range, or
chart DOM. This prevents a slow old-range response from overwriting a newer
selection.

Logging out, losing management authorization through the primary page request,
or clearing rendered data aborts the active statistics request and resets all
chart state.

### Loading, empty, disabled, and error states

Keep the status message outside the hideable chart body:

- Initial load: show `加载中...`; there is no stale chart yet.
- Refresh of an already rendered range: keep the chart visible and show a small
  loading status.
- Successful empty result: clear the old chart and show
  `该时间段无调用记录`.
- HTTP 503: clear and hide the chart body, then show
  `未开启用量统计，请在 config.yaml 设置 usage-stats-enabled: true 后重启/重载`.
- HTTP 400: show the API error inline and retain the last successful chart, if
  any.
- Other non-OK or network errors: retain the last successful chart and label it
  explicitly as `仍显示 <from> 至 <to> 的上次结果` so it cannot be mistaken for
  the failed range.

Controls stay usable after errors so the user can retry or select another
range.

## Testing

### Go API tests

Extend `internal/api/handlers/management/usage_stats_test.go` with:

- server-local `today`, `yesterday`, and inclusive `7d` preset resolution;
- rejection of unknown presets and preset/date combinations;
- acceptance of a range exactly equal to the configured retention limit;
- rejection of a range one day over the limit; and
- presence and format of `server_today` and `server_utc_offset`.

Retain the existing disabled, invalid-order, and empty-result coverage.

### Embedded-page structural tests

Extend `internal/managementasset/quota_monitor_test.go` to verify:

- the new tab button and panel IDs;
- the range controls, status, chart body, legend, and range-summary IDs;
- all-account usage requests that do not contain an `account` parameter;
- `MODEL_STATS_LOGIC_START` / `MODEL_STATS_LOGIC_END` markers;
- stacked-track and success/fail segment classes;
- request-generation and 60-second refresh markers; and
- escaped model-name rendering and the absence of remote scripts.

Structural tests are guards for the embedded asset, not substitutes for logic
tests.

### DOM-free JavaScript tests

Extend `internal/managementasset/quota_monitor_logic_test.mjs` with tests for:

- merging the same model across multiple accounts;
- keeping different exact model strings separate;
- total-descending and model-ascending tie sorting;
- empty input, zero-total removal, and fail-only counts;
- outer width scaling against the maximum total;
- success/fail segment proportions within a row;
- request-generation rejection of stale responses; and
- automatic-refresh eligibility for active/inactive tabs, current/historical
  ranges, and the 60-second minimum interval.

### Manual browser smoke check

Verify:

- lazy first-tab load and the default server-resolved 7-day range;
- preset and custom ranges, including a retention-limit error;
- manual refresh, active-tab auto-refresh, and no historical auto-refresh;
- a deliberately delayed old request cannot overwrite a newer range;
- empty, 503, 400, and network-error states;
- long and HTML-like model names render as text;
- pointer and keyboard tooltip access;
- readable desktop and narrow-mobile layouts; and
- a model list tall enough to trigger internal scrolling.

## Files to Change

- `internal/api/handlers/management/usage_stats.go` - preset calculation, range
  limit, and server-calendar metadata.
- `internal/api/handlers/management/usage_stats_test.go` - API contract tests.
- `internal/managementasset/quota_monitor.html` - tab markup, CSS, fetch state,
  aggregation, rendering, shared server-date presets, and tooltip focus support.
- `internal/managementasset/quota_monitor_test.go` - embedded-page structural
  guards.
- `internal/managementasset/quota_monitor_logic_test.mjs` - aggregation,
  scaling, request-generation, and refresh-policy tests.

## Out of Scope

- Per-day trend charts.
- Backend pre-aggregation or a new persistence format.
- Provider-separated model rows or model-name normalization.
- Changes to the quota and model-health refresh cadence.
- General redesign of the quota monitor page or usage modal.
- Deployment changes. `usage-stats/` remains relative to the container working
  directory (`/CLIProxyAPI/usage-stats`) and must be volume-mounted to survive
  container recreation.
