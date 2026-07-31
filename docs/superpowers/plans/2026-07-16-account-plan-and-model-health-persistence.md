# Account Plan and Model Health Persistence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist antigravity display plans without changing credit routing, remove quota-monitor page-open refresh bursts, add client-side account filtering/pagination, and enable existing model-health cooldown persistence by default.

**Architecture:** Add a process-scoped display-plan registry backed by one atomic `.aps` snapshot in file mode. Capture plans independently at executor and management refresh call sites, expose persisted plans through the existing auth-files response, and keep filtering/pagination in a DOM-free JavaScript object embedded in the quota monitor. Reuse the existing cooldown-state store and change only its configuration defaults.

**Tech Stack:** Go 1.26+, standard-library JSON/filesystem synchronization, Gin, embedded HTML/JavaScript, Node 20+ built-in `node:test` and `node:vm`.

## Global Constraints

- Follow `AGENTS.md`; format Go with `gofmt` and keep comments in English.
- Do not change antigravity `Known`/`Available` scheduling semantics or restore plans into credits hints.
- Store plan snapshots as `antigravity-plans.aps`, never `.json` under the auth directory.
- Keep Home mode plan persistence disabled and cooldown persistence forced off.
- Add no npm or Go dependencies and preserve unrelated `.docker-version` changes.

---

### Task 1: File-backed display-plan registry

**Files:**
- Create: `sdk/cliproxy/auth/antigravity_plan_store.go`
- Create: `sdk/cliproxy/auth/antigravity_plan_store_test.go`
- Modify: `sdk/cliproxy/auth/antigravity_credits.go`

**Interfaces:**
- Produces: `AntigravityPlanRecord`, `AntigravityPlanStore`, `NewFileAntigravityPlanStore(dir string)`, `ConfigureAntigravityPlanStore(ctx context.Context, store AntigravityPlanStore) error`, `SetAntigravityDisplayPlan(authID, paidTierID string, updatedAt time.Time)`, `GetAntigravityDisplayPlan(authID string) (AntigravityPlanRecord, bool)`, and `DeleteAntigravityDisplayPlan(authID string)`.

- [ ] **Step 1: Write failing tests**

Add round-trip/version/`0600` tests and registry concurrency, replacement, nil-clear, deletion, malformed-file, empty-input, identical-tier de-duplication, and temp-cleanup tests. Exercise the desired API directly:

```go
store := NewFileAntigravityPlanStore(t.TempDir())
want := map[string]AntigravityPlanRecord{
    "auth-1": {PaidTierID: "pro", UpdatedAt: time.Date(2026, 7, 16, 1, 2, 3, 0, time.UTC)},
}
if err := store.Save(context.Background(), want); err != nil { t.Fatal(err) }
got, err := store.Load(context.Background())
if err != nil || got["auth-1"].PaidTierID != "pro" { t.Fatalf("Load = %#v, %v", got, err) }
```

- [ ] **Step 2: Verify red**

Run: `go test ./sdk/cliproxy/auth -run 'Test(File)?AntigravityPlan' -count=1`

Expected: compile failure for undefined plan-store types/functions.

- [ ] **Step 3: Implement the minimal registry and store**

Use an interface whose `Load` and `Save` exchange full `map[string]AntigravityPlanRecord` snapshots and a version-1 envelope containing `version` and `plans`. `Save` creates the directory with `0700`, writes a same-directory temp file with `0600`, closes it, renames it, and removes the temp file on errors. Hold the registry mutex through snapshot persistence to prevent stale save ordering; log failures with `auth_id` only.

- [ ] **Step 4: Forward non-empty hint plans**

After normalizing `hint.UpdatedAt` in `SetAntigravityCreditsHint`, call `SetAntigravityDisplayPlan` only when `PaidTierID` is non-empty.

Run: `go test ./sdk/cliproxy/auth -run 'Test(File)?AntigravityPlan|TestAntigravityCredits' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit when Git metadata is writable**

```bash
git add sdk/cliproxy/auth/antigravity_plan_store.go sdk/cliproxy/auth/antigravity_plan_store_test.go sdk/cliproxy/auth/antigravity_credits.go
git commit -m "feat: persist antigravity display plans"
```

---

### Task 2: Account filtering, pagination, and error details

**Files:**
- Modify: `internal/managementasset/quota_monitor.html`
- Modify: `internal/managementasset/quota_monitor_test.go`
- Create: `internal/managementasset/quota_monitor_logic_test.mjs`

**Interfaces:**
- Produces: DOM-free `AccountViewLogic.accountPlan`, `localDateKey`, `filterAccounts`, and `paginate`, bounded by `/* ACCOUNT_VIEW_LOGIC_START */` and `/* ACCOUNT_VIEW_LOGIC_END */`.

- [ ] **Step 1: Write failing Go structural tests**

Require filter IDs, pagination controls, `AccountViewLogic`, `renderAccountView`, escaped status tooltip wiring, and manual refresh. Assert `autoFillCredits` and `creditsAutoAttempted` are absent.

- [ ] **Step 2: Write failing Node behavior tests**

Use `node:fs`, `node:vm`, `node:test`, and `node:assert/strict` to extract/evaluate the marked object. Test AND filtering, Codex plan fallback, unknown plans, inclusive local dates, invalid dates, page clamping, deletion of the last final-page item, and a changed plan leaving the active filter.

```js
const rows = logic.filterAccounts(fixtures, {
  name: 'alpha', plan: 'plus', status: 'active', from: '2026-07-15', to: '2026-07-15'
});
assert.deepEqual(rows.map((row) => row.name), ['Alpha Codex']);
```

- [ ] **Step 3: Verify red**

Run the Go and Node commands separately:

```bash
go test ./internal/managementasset -run TestQuotaMonitor -count=1
node --test internal/managementasset/quota_monitor_logic_test.mjs
```

Expected: both FAIL for missing controls/helpers.

- [ ] **Step 4: Implement controls and one render path**

Add name, plan, status, from/to controls; page-size 20/50/100; previous/next icon buttons; and a stable indicator. Keep `accountsData` raw. Route initial fetch, filters, page changes, page size, delete, and manual plan refresh through `renderAccountView({resetPage, rebuildOptions})`. Rebuild dynamic options while preserving only valid selections. Render `暂无账号` for empty source and `无匹配账号` for no filtered matches.

- [ ] **Step 5: Remove page-open requests and add error tooltip**

Delete `autoFillCredits` and `creditsAutoAttempted`. Keep the manual refresh endpoint. For `status === 'error'`, render an escaped `title` from `status_message`; never inject unescaped response values into attributes.

- [ ] **Step 6: Verify green**

Run: `go test ./internal/managementasset -count=1 && node --test internal/managementasset/quota_monitor_logic_test.mjs`

Expected: PASS.

---

### Task 3: Cooldown persistence default and restart coverage

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/parse.go`
- Modify: `config.example.yaml`
- Create: `internal/config/save_cooldown_status_test.go`
- Modify: `sdk/cliproxy/auth/cooldown_state_test.go`
- Modify: `sdk/cliproxy/service_stale_state_test.go`

- [ ] **Step 1: Write failing config tests**

Test absent and explicit-false values through both `ParseConfigBytes` and `LoadConfigOptional`; absent must be true and explicit false must remain false. Preserve Home-mode false assertions.

- [ ] **Step 2: Extend restore test first**

Add an expired model record beside the future record in `TestManager_RestoreCooldownStates`; assert only the future unavailable state survives.

- [ ] **Step 3: Verify red**

Run: `go test ./internal/config ./sdk/cliproxy/auth ./sdk/cliproxy -run 'SaveCooldownStatus|RestoreCooldownStates|HomeRuntimeConfig' -count=1`

Expected: absent-key assertions FAIL with false.

- [ ] **Step 4: Change defaults and verify green**

Set `cfg.SaveCooldownStatus = true` before YAML unmarshal in both config initialization paths and change `config.example.yaml` to true. Leave `forceHomeRuntimeConfig` assigning false.

Run: `go test ./internal/config ./sdk/cliproxy/auth ./sdk/cliproxy -run 'SaveCooldownStatus|RestoreCooldownStates|HomeRuntimeConfig' -count=1`

Expected: PASS.

---

### Task 4: Independent capture, removal, and lifecycle

**Files:**
- Modify: `internal/runtime/executor/antigravity_executor.go`
- Modify: `internal/runtime/executor/antigravity_executor_credits_test.go`
- Modify: `internal/api/handlers/management/antigravity_credits.go`
- Modify: `internal/api/handlers/management/antigravity_credits_test.go`
- Modify: `sdk/cliproxy/auth/conductor.go`
- Modify: `sdk/cliproxy/auth/conductor_remove_test.go`
- Modify: `sdk/cliproxy/service.go`
- Create: `sdk/cliproxy/service_antigravity_plan_test.go`

**Interfaces:**
- Consumes: Task 1 registry API.
- Produces: `(*Service).configureAntigravityPlanStore(ctx context.Context, cfg *config.Config)`.

- [ ] **Step 1: Write failing capture tests**

For executor and management refresh, return `paidTier.id = pro` with only an `OTHER` credit entry. Assert `GetAntigravityDisplayPlan` returns `pro` while `GetAntigravityCreditsHint` remains absent.

- [ ] **Step 2: Verify red**

Run: `go test ./internal/runtime/executor ./internal/api/handlers/management -run 'UnmatchedCredits|DisplayPlan' -count=1`

Expected: FAIL because the tier is discarded when the hint is not cacheable.

- [ ] **Step 3: Capture before cacheability branches**

In the executor call `SetAntigravityDisplayPlan` immediately after parsing `paidTier.id`. In `RefreshAntigravityCredits`, call it after a successful fetch and before checking `cacheable`. Do not change hint semantics.

- [ ] **Step 4: Write failing removal/lifecycle tests**

Test antigravity removal deletes its record, restored plans do not create known hints, auth-directory changes replace registry state, Home/nil configuration clears it, and shutdown clears it.

- [ ] **Step 5: Verify red and implement lifecycle**

Run: `go test ./sdk/cliproxy/... -run 'AntigravityPlan|Remove.*DisplayPlan' -count=1`

Expected: FAIL before wiring. Then make `Manager.Remove` delete only antigravity plans. Resolve `cfg.AuthDir` in `configureAntigravityPlanStore`; nil/Home/error clears the registry, file mode loads `NewFileAntigravityPlanStore(authDir)`. Invoke it after manager load at startup, on watcher config updates, and with nil during shutdown. Log errors and continue.

- [ ] **Step 6: Verify green with race detection**

Run: `go test -race ./sdk/cliproxy/auth ./internal/runtime/executor ./internal/api/handlers/management ./sdk/cliproxy -run 'AntigravityPlan|UnmatchedCredits|Remove.*DisplayPlan' -count=1`

Expected: PASS without races.

- [ ] **Step 7: Commit when Git metadata is writable**

```bash
git add internal/runtime/executor/antigravity_executor.go internal/runtime/executor/antigravity_executor_credits_test.go internal/api/handlers/management/antigravity_credits.go internal/api/handlers/management/antigravity_credits_test.go sdk/cliproxy/auth/conductor.go sdk/cliproxy/auth/conductor_remove_test.go sdk/cliproxy/service.go sdk/cliproxy/service_antigravity_plan_test.go
git commit -m "feat: capture and restore antigravity plans"
```

---

### Task 5: Management API fallback

**Files:**
- Modify: `internal/api/handlers/management/auth_files.go`
- Modify: `internal/api/handlers/management/auth_files_paid_tier_test.go`

**Interfaces:**
- Produces: unchanged `paid_tier` field with live-hint-first, persisted-plan-second precedence.

- [ ] **Step 1: Write failing persisted-only and precedence tests**

Configure a fresh recording store per test. Cover persisted-only, live overriding persisted, and neither source.

- [ ] **Step 2: Verify red**

Run: `go test ./internal/api/handlers/management -run TestBuildAuthFileEntry_PaidTier -count=1`

Expected: persisted-only test FAIL.

- [ ] **Step 3: Implement fallback and verify green**

Read the live hint first; when its normalized tier is empty, read `GetAntigravityDisplayPlan(auth.ID)`. Omit the field if both are empty.

Run: `go test ./internal/api/handlers/management -run 'TestBuildAuthFileEntry_PaidTier|TestRefreshAntigravityCredits' -count=1`

Expected: PASS.

---

### Task 6: Regression gate and review

- [ ] **Step 1: Format Go changes**

Run `gofmt -w` on every modified `.go` file.

- [ ] **Step 2: Run focused, race, and frontend suites**

```bash
go test ./sdk/cliproxy/auth ./sdk/cliproxy ./internal/runtime/executor ./internal/api/handlers/management ./internal/managementasset ./internal/config -count=1
go test -race ./sdk/cliproxy/auth ./sdk/cliproxy ./internal/runtime/executor ./internal/api/handlers/management -count=1
node --test internal/managementasset/quota_monitor_logic_test.mjs
```

Expected: PASS without race reports.

- [ ] **Step 3: Run repository gates**

```bash
go test ./...
go vet ./sdk/cliproxy/auth ./sdk/cliproxy ./internal/runtime/executor ./internal/api/handlers/management ./internal/managementasset ./internal/config
go build -o test-output ./cmd/server && rm test-output
git diff --check
```

Expected: tests, touched-package vet, build, and diff check pass. Record unrelated full-repository vet warnings separately.

- [ ] **Step 4: Perform UI smoke validation**

Against disposable auth fixtures, verify desktop and narrow widths: no page-open credits requests, filters combine, date bounds include the local end day, pagination clamps, error text appears on hover, deletion reclamps, and manual refresh updates an active plan filter. Record unavailable browser/runtime prerequisites in the handoff.

- [ ] **Step 5: Request review and resolve findings**

Review against `docs/superpowers/specs/2026-07-15-account-plan-persistence-and-model-health-persistence-design.md`. Fix every critical/important finding with a failing regression test first, then rerun Steps 1-3.
