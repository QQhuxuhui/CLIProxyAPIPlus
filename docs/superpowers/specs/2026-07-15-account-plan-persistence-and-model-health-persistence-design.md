# Account 套餐 Persistence + Account-List UX + Model-Health Persistence — Design

**Status:** Revised after code review; pending user re-review (2026-07-16).
**Date:** 2026-07-15 (revised 2026-07-16)
**Branch:** dev

**Goal:** On the self-built `/quota-monitor.html` 账号统计 page:
1. Stop the page-open "全量加载" burst that force-fetches every antigravity account's 套餐 upstream. Instead, rely on the plan already captured during normal API usage, **persist** it so it survives restart, and keep the manual 刷新额度 button.
2. Make the account list paginated and filterable (name fuzzy, 套餐, 创建时间, 状态), and show the error message on hover when status is `error`.
3. Persist the 模型健康 (model-health) data so it survives a restart.

---

## 1. Feasibility (verified against code)

| Concern | Finding |
|---|---|
| Can we get 套餐 during normal API calls? | Not from the proxied response body (no provider exposes plan there). But the **antigravity** executor already captures `paidTier.id` opportunistically during normal usage via a throttled side-call to `loadCodeAssist` (`maybeRefreshAntigravityCreditsHint` → `updateAntigravityCreditsBalance`, `internal/runtime/executor/antigravity_executor.go:1896/2140`). **Codex** plan is decoded locally from the JWT (`Attributes["plan_type"]`, already persisted). Gemini/Claude/others have no plan concept. |
| Why is 套餐 lost on restart? | The captured hint lives only in `antigravityCreditsHintByAuth sync.Map` (`sdk/cliproxy/auth/antigravity_credits.go:39`); file mode never persists it. That is exactly why `autoFillCredits()` (`quota_monitor.html:614`) force-fires one `loadCodeAssist` per un-cached antigravity account on page open. |
| Error message for hover | Already exposed: `buildAuthFileEntry` returns `status_message` from `Auth.StatusMessage` (`internal/api/handlers/management/auth_files.go`). Frontend-only change. |
| Model-health persistence | A purpose-built `CooldownStateStore` (`.cds` files) + `RestoreCooldownStates` already reconstruct the health snapshot on restart (`sdk/cliproxy/auth/cooldown_state.go`, `conductor.go:584`). It is gated off by `save-cooldown-status: false` (default) and also disabled in Home mode. User runs **file mode**, so enabling the flag is sufficient. |

**User decisions (2026-07-15):** client-side pagination; file-mode deployment; never-used accounts show `—` until first call or manual refresh; decouple plan-display from credit-scheduling; default `save-cooldown-status` on for this build.

---

## 2. Non-Goals

- No scraping of plan from proxied response bodies (the data is not there).
- No change to the antigravity credit-scheduling semantics (`Known`/`Available` in-memory lifecycle stays as-is; the scheduler keeps re-verifying live credit availability on first use).
- No server-side pagination/filtering (client-side chosen).
- No new health store — reuse the existing `.cds` cooldown persistence.
- No Home-mode plan persistence path (out of scope; Home mode keeps current in-memory behavior).
- No change to Codex plan handling (already local + persisted).

---

## 3. Workstream ① — 套餐 capture + persistence (kill the page-open burst)

### 3.1 Principle: decouple display-plan from credit-scheduling

- **Display plan** (`paid_tier`, i.e. `PaidTierID`) is stable and is what the page shows. We persist it.
- **Credit-scheduling flags** (`Known`, `Available`, `CreditAmount`, `MinCreditAmount`) are volatile and drive routing (`antigravityCreditsAvailableForModel`, `conductor.go:5310`). We do **not** persist/restore these, so on restart the executor still re-verifies live credit availability on the account's first request. This keeps the scheduler correct while the page shows the plan immediately.

### 3.2 New injectable plan store (file mode)

Mirror the existing `CooldownStateStore` architecture (injectable interface + file impl + startup wiring), kept minimal:

- New interface in `sdk/cliproxy/auth` (e.g. `AntigravityPlanStore`) with `Load(ctx) (map[string]AntigravityPlanRecord, error)` and `Save(ctx, snapshot map[string]AntigravityPlanRecord) error`.
- `AntigravityPlanRecord`: `{ PaidTierID string; UpdatedAt time.Time }`.
- File impl: one versioned JSON envelope stored as `antigravity-plans.aps` in the resolved auth dir. The non-`.json` extension is mandatory: the filestore and watcher treat every `.json` below `auth-dir` as an auth credential. A single map file is sufficient because plan data is tiny.
- Write with a `0600` temp file, close it, atomically replace the target with `os.Rename`, and remove the temp file on every failure path. If the auth directory is missing, create it with `0700`; if it already exists, preserve its permissions because it is shared with the configured auth store.
- A process-level plan registry holds the injected store and `map[authID]AntigravityPlanRecord`. This matches the existing process-level credits-hint scope, but its lifecycle is explicit: configuring a store **replaces** the map from that store, configuring nil clears both store and map, and service shutdown clears it so sequential SDK services cannot leak state.
- Registry mutation and persistence are serialized. Each write updates the map, then the persistence path takes a fresh full snapshot and calls `Save`; it never performs concurrent per-key read-modify-write operations against the single file. Repeating the same non-empty tier is a no-op, avoiding unnecessary writes.
- Public operations are `SetAntigravityDisplayPlan(authID, paidTierID, updatedAt)`, `GetAntigravityDisplayPlan(authID)`, and `DeleteAntigravityDisplayPlan(authID)`. Empty IDs/tiers are ignored because an absent tier is ambiguous and must not erase a previously confirmed tier.
- `Manager.Remove` calls `DeleteAntigravityDisplayPlan` for antigravity auths. Startup load drops malformed/empty records; account removal prevents stale plans from surviving deletion and re-import under the same ID.

### 3.3 Capture independently from credit scheduling

`SetAntigravityCreditsHint` is not a complete capture point: both the executor and the management handler can parse a valid `paidTier.id` but intentionally skip the hint when the credits array has no valid `GOOGLE_ONE_AI` entry. Display-plan capture therefore has its own API and call sites:

- In `updateAntigravityCreditsBalance`, call `SetAntigravityDisplayPlan` immediately after parsing a non-empty `paidTier.id`, before inspecting `availableCredits`.
- In `RefreshAntigravityCredits`, call it after a successful upstream response whenever the parsed `PaidTierID` is non-empty, regardless of the separate `cacheable` decision for the live credits hint.
- `SetAntigravityCreditsHint` may also delegate a non-empty `PaidTierID` to `SetAntigravityDisplayPlan` as a defensive capture path. Registry de-duplication prevents duplicate disk writes.
- Store errors are best-effort and logged with auth ID only; no token, response body, or other credential material is logged. Plan persistence failure does not fail an API request or alter credit routing.

### 3.4 Startup restore (display only)

In file mode, `sdk/cliproxy/service.go` resolves the auth dir and configures/loads `antigravity-plans.aps` after the core auth store is loaded and before serving requests. **Do not** write restored records into `antigravityCreditsHintByAuth` as `Known` hints: restore is display-only, so `maybeRefreshAntigravityCreditsHint` still fires on first use.

The same configure function runs on config hot reload so an `auth-dir` change replaces the store and in-memory map rather than continuing to write the old directory. Home mode and service shutdown configure nil and clear state. A malformed plan file logs a warning and starts with an empty display-plan map; it must not prevent service startup or mutate credits scheduling.

### 3.5 Read path

`buildAuthFileEntry` resolves `paid_tier` as: live credits hint's `PaidTierID` if present and non-empty, otherwise `GetAntigravityDisplayPlan(auth.ID)`, otherwise omit (page renders `—`). This means after restart, before any request, the page shows the persisted plan with zero upstream calls.

### 3.6 Frontend

- **Remove `autoFillCredits()`** and its `creditsAutoAttempted` bookkeeping (`quota_monitor.html:335/614-624`, called at `render()` line 597). No upstream calls on page open.
- **Keep** the manual 刷新额度 button and `refreshCredits()` (`quota_monitor.html:626-649`) — it still POSTs `/v0/management/antigravity-credits/refresh`, which updates display-plan persistence independently from the live hint's cacheability.
- Never-used accounts show `—` until first call or manual refresh.

---

## 4. Workstream ② — Account list UX (frontend-only, client-side)

All in `internal/managementasset/quota_monitor.html`, operating on the single existing `GET /v0/management/auth-files` fetch (local, in-memory — cheap even with many accounts). No backend change.

### 4.1 Filters (applied to the loaded array)

| Filter | Behavior |
|---|---|
| 账号名称 | Case-insensitive substring match on `entry.name`. |
| 套餐 | Dropdown populated from distinct `accountPlan(entry)` values, not only `paid_tier`; this keeps Codex `id_token.plan_type` display and filtering consistent. Include a "—/未知" option and an "全部" default. |
| 创建时间 | Two date inputs (from/to). Convert `entry.created_at` to a browser-local `YYYY-MM-DD` calendar value and compare those strings inclusively, so the end date includes the entire local day. Entries with no/invalid `created_at` are excluded when a range is set. |
| 状态 | Dropdown populated from the distinct `status` values present (e.g. active/disabled/error/…), plus "全部". |

Filters combine with AND. Changing any filter re-filters and resets to page 1. Refreshing data rebuilds dynamic options while preserving a selected value only if it still exists; otherwise that filter returns to "全部".

### 4.2 Pagination

- Page-size selector (e.g. 20/50/100) + prev/next + a "第 X / Y 页 · 共 N 条" indicator.
- Implement pure helpers for filtering and pagination, then route every account-table update through one `renderAccountView({ resetPage, rebuildOptions })` entry point.
- Applied to the filtered result set. Manual 刷新 re-fetches and re-applies current filters/page (clamped to valid range).
- Successful account deletion rebuilds filter options, reapplies filters, and clamps the page; the page never remains empty merely because the last item on the final page was deleted.
- Successful manual credits refresh updates the matching account, rebuilds plan options, and reapplies the active plan filter. If the refreshed account no longer matches, it disappears from the current result as expected.
- Empty source data renders "暂无账号"; a non-empty source with zero filtered matches renders a distinct "无匹配账号" state.

### 4.3 Error hover

When `entry.status === 'error'`, render the 状态 cell with a `data-tip` attribute (and a subtle visual affordance) containing `entry.status_message`, shown via a fixed-position tooltip driven by a delegated `mouseover` listener. Native `title` tooltips are not used: the 15s auto-refresh rebuilds the row DOM, which cancels the browser tooltip before it ever appears. `status_message` is already in the API response; escape it for HTML. When status is not error, render as today.

---

## 5. Workstream ③ — Model-health persistence

- Enable the existing cooldown-state persistence by **defaulting `save-cooldown-status` to `true`** for this build in both config initialization paths (`internal/config/config.go` and `internal/config/parse.go`) plus `config.example.yaml`. Explicit `false` still overrides the default. File mode only; Home mode remains forced off as today.
- On restart, `RestoreCooldownStates` rehydrates cooling/unavailable model states with a future `NextRetryAfter`; `disabled` rides in the auth file; healthy is the implicit default. Together these reconstruct exactly what the 模型健康 tab renders (it recomputes `by_model` from live auth state each poll).
- No new store, no new endpoint, no frontend change.

This default flip is intentional and accepted for this build. Compatibility is provided by the existing explicit `false` setting and the unchanged Home-mode override; tests must lock down both opt-out paths.

---

## 6. Testing (TDD)

**Plan store (`sdk/cliproxy/auth`):**
- Save → load round-trip; versioned envelope; `0600` final file; atomic temp+rename cleanup.
- Two concurrent auth updates survive in the final snapshot under `go test -race`; repeated identical tiers do not write again.
- The `.aps` file is ignored by `FileTokenStore.List`, watcher auth synthesis, and auth-file counts/events.
- Dedicated capture persists a non-empty tier even when the live hint is not cacheable; empty tiers do not erase an existing plan.
- Configure/load replaces rather than merges registry state; configure nil clears it; malformed files recover as empty with a warning.
- Removing an antigravity auth deletes its plan and persists the new snapshot; re-import under the same ID starts with no stale plan.
- Startup restore populates display-plan state but does **not** create a `Known` credits hint, so the executor still refreshes live credits on first use.

**Read path (`auth_files_test.go`):**
- `paid_tier` present from live hint; present from persisted map when live hint absent; omitted when neither.

**Frontend:**
- Rendered page **no longer contains** `autoFillCredits`/`creditsAutoAttempted`.
- Contains the filter controls (name input, 套餐 select, date inputs, 状态 select), pagination controls, and the error-tooltip markup (`data-tip=`/`#hover-tip`/status-message wiring).
- Still contains the 刷新额度 button + `/v0/management/antigravity-credits/refresh` call.
- Put filter/pagination derivation in a DOM-free `AccountViewLogic` object bounded by stable marker comments in the inline script. Add `internal/managementasset/quota_monitor_logic_test.mjs`, using only built-in `node:test` and `node:vm`, to extract/evaluate that marked block and cover AND filtering, Codex plan fallback, local-calendar inclusive dates, invalid dates, page clamping, deletion of the final-page item, and a refreshed plan leaving the active filter. This adds no npm dependency; Node 20+ is a test-only prerequisite.
- Perform and document a manual browser smoke check for filter controls, prev/next disabled states, error tooltip, deletion, and manual plan refresh. HTML substring tests remain structural guards, not the sole automated behavioral proof.

**Model-health:**
- Extend/confirm the cooldown save→restore test; add an assertion that a simulated restart reconstructs the health-relevant model states (cooling with future retry restored; expired skipped).
- Config tests cover absent → true, explicit false → false, Home mode → false, and hot-reload enable/disable for the cooldown store in both config parsing paths.

**Gate:** `gofmt -w .`, `go test ./...`, race tests for touched packages, `node --test internal/managementasset/quota_monitor_logic_test.mjs`, the documented browser smoke check, and `go build -o test-output ./cmd/server && rm test-output`. Run `go vet` on every touched package. Full-repo `go vet ./...` currently has unrelated baseline warnings; record those separately and do not attribute them to this change unless the baseline is fixed first.

---

## 7. Files touched

- `sdk/cliproxy/auth/antigravity_credits.go` — defensive forwarding of non-empty hint tiers to the display-plan registry.
- `sdk/cliproxy/auth/antigravity_plan_store.go` (new) — display-plan registry, `AntigravityPlanStore`, snapshot serialization, and `FileAntigravityPlanStore` using `antigravity-plans.aps`.
- `sdk/cliproxy/auth/conductor.go` — remove persisted display plan when an antigravity auth is removed.
- `internal/runtime/executor/antigravity_executor.go` — capture `paidTier.id` independently from credits-array parsing.
- `internal/api/handlers/management/antigravity_credits.go` — persist a parsed display plan independently from live-hint cacheability.
- `sdk/cliproxy/service.go` — configure/replace/clear the plan store on startup, hot reload, Home mode, and shutdown; enable cooldown save.
- `internal/api/handlers/management/auth_files.go` — `paid_tier` fallback read from the persisted-plan map.
- `internal/managementasset/quota_monitor.html` — remove auto-fill; add filters, pagination, error tooltip.
- `internal/managementasset/quota_monitor_logic_test.mjs` (new) — dependency-free behavioral tests for the DOM-free account-view logic embedded in the HTML.
- `internal/config/config.go`, `internal/config/parse.go`, `config.example.yaml` — default `save-cooldown-status: true` while preserving explicit false and Home override.
- Corresponding `_test.go` files.

---

## 8. Open risks

- **Credit-availability freshness across restart:** by design we only persist the display plan, so the scheduler re-verifies live credits on first use — no staleness introduced beyond today's behavior.
- **`paid_tier` for never-used accounts:** shows `—` until first call/manual refresh (accepted).
- **Plan store is file-mode only:** Home-mode deployments keep the current in-memory behavior (out of scope).
- **Process-level registry:** the existing credits hints and the new display-plan registry assume one active CLIProxy service per process. Configure/clear semantics prevent sequential-service leakage; simultaneous independent services in one process remain outside the current runtime architecture.
- **Missing tier ambiguity:** an empty/missing `paidTier.id` does not clear a confirmed plan. A non-empty changed tier replaces it; account deletion removes it. This favors the last confirmed provider value over interpreting a partial response as a downgrade.
- **`save-cooldown-status` default flip:** accepted for this build. It only affects file-mode deployments and writes small `.cds` files beside auth files; explicit `false` remains supported.
- **`AuthIndex`/`ID` stability:** persisted plan is keyed by `auth.ID` (stable across restarts), not the runtime index — safe.
