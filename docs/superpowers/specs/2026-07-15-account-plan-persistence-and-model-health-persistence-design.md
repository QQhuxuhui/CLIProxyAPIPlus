# Account 套餐 Persistence + Account-List UX + Model-Health Persistence — Design

**Status:** Approved by user (design walkthrough, 2026-07-15).
**Date:** 2026-07-15
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

- New interface in `sdk/cliproxy/auth` (e.g. `AntigravityPlanStore`) with `Load(ctx) (map[string]AntigravityPlanRecord, error)` and `Save(ctx, authID string, rec AntigravityPlanRecord) error`.
- `AntigravityPlanRecord`: `{ PaidTierID string; UpdatedAt time.Time }`.
- File impl: one atomic JSON file `antigravity-plans.json` in the **auth dir** (the same directory `.cds` files live in, resolved the same way as `FileCooldownStateStore`), temp-write + `os.Rename`, matching `.cds`/`usagestats` idioms. A single map file (not per-auth) is sufficient — plan data is tiny.
- A package-level injected store (`SetAntigravityPlanStore`, nil by default) plus an in-memory `map[authID]PaidTierID` populated on startup load, guarded by a dedicated `sync.RWMutex`.

### 3.3 Persist at the single choke point

Both capture paths (executor opportunistic refresh and the manual `RefreshAntigravityCredits` handler) already funnel through `SetAntigravityCreditsHint`. In that function, when the incoming hint has a non-empty `PaidTierID`:
- Update the in-memory persisted-plan map, and
- Best-effort persist the `PaidTierID` via the injected plan store (no error propagation; log on failure per logging conventions).

No change to the executor call sites is required — persistence happens transparently in `SetAntigravityCreditsHint`.

### 3.4 Startup restore (display only)

At startup (file mode, in `sdk/cliproxy/service.go`, alongside the cooldown-store wiring), load `antigravity-plans.json` into the persisted-plan map. **Do not** write these into `antigravityCreditsHintByAuth` as `Known` hints — restore is display-only, so `maybeRefreshAntigravityCreditsHint` still fires on first use.

### 3.5 Read path

`buildAuthFileEntry` resolves `paid_tier` as: live credits hint's `PaidTierID` if present and non-empty, otherwise the persisted-plan map value, otherwise omit (page renders `—`). This means after restart, before any request, the page shows the persisted plan with zero upstream calls.

### 3.6 Frontend

- **Remove `autoFillCredits()`** and its `creditsAutoAttempted` bookkeeping (`quota_monitor.html:335/614-624`, called at `render()` line 597). No upstream calls on page open.
- **Keep** the manual 刷新额度 button and `refreshCredits()` (`quota_monitor.html:626-649`) — it still POSTs `/v0/management/antigravity-credits/refresh`, which updates the live hint (and, via 3.3, the persisted plan).
- Never-used accounts show `—` until first call or manual refresh.

---

## 4. Workstream ② — Account list UX (frontend-only, client-side)

All in `internal/managementasset/quota_monitor.html`, operating on the single existing `GET /v0/management/auth-files` fetch (local, in-memory — cheap even with many accounts). No backend change.

### 4.1 Filters (applied to the loaded array)

| Filter | Behavior |
|---|---|
| 账号名称 | Case-insensitive substring match on `entry.name`. |
| 套餐 | Dropdown populated from the distinct `paid_tier` values present in the loaded data, plus a "—/未知" option and an "全部" default. |
| 创建时间 | Two date inputs (from/to); match `entry.created_at` within the inclusive range; entries with no `created_at` are excluded when a range is set. |
| 状态 | Dropdown populated from the distinct `status` values present (e.g. active/disabled/error/…), plus "全部". |

Filters combine with AND. Changing any filter re-filters and resets to page 1.

### 4.2 Pagination

- Page-size selector (e.g. 20/50/100) + prev/next + a "第 X / Y 页 · 共 N 条" indicator.
- Applied to the filtered result set. Manual 刷新 re-fetches and re-applies current filters/page (clamped to valid range).

### 4.3 Error hover

When `entry.status === 'error'`, render the 状态 cell with a `title` attribute (and a subtle visual affordance) containing `entry.status_message`. `status_message` is already in the API response; escape it for HTML. When status is not error, render as today.

---

## 5. Workstream ③ — Model-health persistence

- Enable the existing cooldown-state persistence by **defaulting `save-cooldown-status` to `true`** for this build (`internal/config/config.go` default + `config.example.yaml`). File mode only; Home mode remains gated as today.
- On restart, `RestoreCooldownStates` rehydrates cooling/unavailable model states with a future `NextRetryAfter`; `disabled` rides in the auth file; healthy is the implicit default. Together these reconstruct exactly what the 模型健康 tab renders (it recomputes `by_model` from live auth state each poll).
- No new store, no new endpoint, no frontend change.

**Note / verify point:** confirm defaulting the flag on has no unintended interaction with existing users of this fork (it only writes `.cds` files next to auth files in file mode). If a silent global default change is undesirable, the fallback is to set it in the user's own `config.yaml`; the design’s intent is that model-health persists out of the box for this build.

---

## 6. Testing (TDD)

**Plan store (`sdk/cliproxy/auth`):**
- Save → load round-trip; atomic write (temp+rename) to a temp dir.
- `SetAntigravityCreditsHint` with a non-empty `PaidTierID` updates the persisted-plan map and calls the store; with an empty `PaidTierID` it does not.
- Startup restore populates the persisted-plan map but does **not** create `Known` hints (assert `GetAntigravityCreditsHint` still reports not-known, so `maybeRefreshAntigravityCreditsHint` would still fire).

**Read path (`auth_files_test.go`):**
- `paid_tier` present from live hint; present from persisted map when live hint absent; omitted when neither.

**Frontend (`quota_monitor_test.go`, HTML-substring convention):**
- Rendered page **no longer contains** `autoFillCredits`/`creditsAutoAttempted`.
- Contains the filter controls (name input, 套餐 select, date inputs, 状态 select), pagination controls, and the error-tooltip markup (`title=`/status-message wiring).
- Still contains the 刷新额度 button + `/v0/management/antigravity-credits/refresh` call.

**Model-health:**
- Extend/confirm the cooldown save→restore test; add an assertion that a simulated restart reconstructs the health-relevant model states (cooling with future retry restored; expired skipped).

**Gate:** `gofmt -w .`, `go test ./...`, `go vet ./...`, race test for touched packages, `go build -o test-output ./cmd/server && rm test-output`.

---

## 7. Files touched

- `sdk/cliproxy/auth/antigravity_credits.go` — persisted-plan map, `SetAntigravityPlanStore`, persist-on-`SetAntigravityCreditsHint`, startup-load helper.
- `sdk/cliproxy/auth/antigravity_plan_store.go` (new) — `AntigravityPlanStore` interface + `FileAntigravityPlanStore` impl.
- `sdk/cliproxy/service.go` — wire the plan store + load on startup (file mode); enable cooldown save.
- `internal/api/handlers/management/auth_files.go` — `paid_tier` fallback read from the persisted-plan map.
- `internal/managementasset/quota_monitor.html` — remove auto-fill; add filters, pagination, error tooltip.
- `internal/config/config.go`, `config.example.yaml` — default `save-cooldown-status: true`.
- Corresponding `_test.go` files.

---

## 8. Open risks

- **Credit-availability freshness across restart:** by design we only persist the display plan, so the scheduler re-verifies live credits on first use — no staleness introduced beyond today's behavior.
- **`paid_tier` for never-used accounts:** shows `—` until first call/manual refresh (accepted).
- **Plan store is file-mode only:** Home-mode deployments keep the current in-memory behavior (out of scope).
- **`save-cooldown-status` default flip:** only affects file-mode deployments of this fork; writes small `.cds` files beside auth files. Confirm acceptable during spec review.
- **`AuthIndex`/`ID` stability:** persisted plan is keyed by `auth.ID` (stable across restarts), not the runtime index — safe.
