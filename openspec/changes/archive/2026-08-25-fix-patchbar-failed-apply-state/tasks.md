# Tasks — fix-patchbar-failed-apply-state

> Status: complete
> Phase: archive
> Source design: Engram `#2753` (`sdd/fix-patchbar-failed-apply-state/design`)
> Source spec: Engram `#2752` (`sdd/fix-patchbar-failed-apply-state/spec`)
> Source proposal: Engram `#2751` (`sdd/fix-patchbar-failed-apply-state/proposal`)

## Completion

Archive-time checkbox reconciliation: the original tasks artifact used
`TASK-N` headings without `- [x]` markers. `apply-progress.md` and
`verify-report.md` prove all five implementation tasks landed (commits
`7246806`, `7720e0e`, `dcb8f4d`, `a352640`, `43a3e27`). User explicitly
requested archive of this change.

- [x] TASK-1: Add pure state-machine module + tests
- [x] TASK-2: Extend `StatusPayload.jobs` shape with `errorMessage`
- [x] TASK-3: Wire auto-retry inside `DashboardProvider`
- [x] TASK-4: Render the 5-state PatchBar from `DashboardShell`
- [x] TASK-5: End-to-end state-machine smoke (mock status payload)

## Goal

Break the design into ordered, implementable work units. Each task is small enough to fit in one commit (one reviewable work unit), references the spec REQs it advances, and pins the file:line area it touches. Tests land alongside code.

## Constraints carried from design

- PatchBar-only change in `apps/ui`. No API, worker, or DB contract changes.
- No new dependencies in `apps/ui/package.json`. Pure `vitest` at the repo root picks up the new test file via the existing `apps/**/*.test.ts` glob (`vitest.config.ts:24`).
- `apps/ui` currently has zero React test infrastructure. Auto-retry effect, persistence, and disabled-button behavior are verified by code review + manual smoke (design §8). The pure state machine is unit-tested.

## Order rationale

Bottom-up so the bar can be built on a verified foundation, with tests living next to the code they cover (no orphan test commit).

---

## Tasks

### TASK-1: Add pure state-machine module + tests

- **REQ coverage:** REQ-1, REQ-5, REQ-6
- **Files:**
  - `apps/ui/src/dashboard/patch-bar-state.ts` (new, ~50 LOC)
  - `apps/ui/src/dashboard/patch-bar-state.test.ts` (new, ~150 LOC)
- **Acceptance:** New module exports `JobLike`, `BarState`, `selectFailedJob`, `deriveBarState`. Tests cover:
  - `selectFailedJob` returns `undefined` for `undefined`/empty/non-`failed` arrays.
  - `selectFailedJob` picks newest by `createdAt` desc when multiple failed jobs exist.
  - `selectFailedJob` tie-breaks by `id` desc on identical `createdAt` (deterministic across renders).
  - `selectFailedJob` ignores jobs whose `createdAt` fails `Date.parse`.
  - `deriveBarState` returns `checking` when `loaded === false`.
  - `deriveBarState` returns `applying` when `pendingJob === true` even with `failureReason` present.
  - `deriveBarState` returns `failed` when `failureReason` is set, taking precedence over both `inSync === true` and `inSync === false`.
  - `deriveBarState` returns `live` when `inSync === true` and no failure.
  - `deriveBarState` returns `pending` when `inSync === false` and no failure.
- **Verification:**
  - `bun run test -- apps/ui/src/dashboard/patch-bar-state.test.ts` — all assertions pass.
  - `bun run typecheck` — green (pure TS, no React imports).
- **Commit:** `feat(ui): add patch-bar state machine and unit tests`

### TASK-2: Extend `StatusPayload.jobs` shape with `errorMessage`

- **REQ coverage:** REQ-1, REQ-6
- **Files:**
  - `apps/ui/src/dashboard/types.ts` (modify, lines 23-43; +6 LOC, 1 line modified)
- **Acceptance:** Extract `JobRecord` as a named exported type (currently inline at `types.ts:34-39`); add `errorMessage?: string | null` to it. `StatusPayload.jobs` now uses `JobRecord[]` instead of the inline shape. The three downstream consumers (`dashboard-context.tsx`, `DashboardShell.tsx`, any future ingest) see the new optional field without `any` casts.
- **Verification:**
  - `bun run typecheck` — green. No call site breaks because the new field is optional and the existing inline fields are kept.
- **Commit:** `feat(ui): expose job errorMessage on StatusPayload`

### TASK-3: Wire auto-retry inside `DashboardProvider`

- **REQ coverage:** REQ-2, REQ-3, REQ-4, REQ-7
- **Files:**
  - `apps/ui/src/dashboard/dashboard-context.tsx` (modify, lines 17-54, 58-381; +45 LOC, 2 lines modified)
- **Acceptance:**
  - `useRef<string | null>(null)` named `autoRetriedJobIdRef` declared near the top of `DashboardProvider` (sibling of the existing `useState` calls at `dashboard-context.tsx:60-71`).
  - `useMemo` named `autoRetrying` reads `status?.jobs` + the ref; returns `true` iff `selectFailedJob(...)?.id === autoRetriedJobIdRef.current` AND no newer job (by `createdAt`) exists in `status.jobs`. Otherwise `false`.
  - `useEffect` watching `[status?.jobs]` runs after the polling effects: when a failed job is observed whose id is not in the ref AND no newer job of any status exists, set the ref and call a private `autoRetryApply()` that wraps `mutate("/api/apply", {}, "Apply queued (auto-retry)")` without resetting the ref.
  - `apply()` at `dashboard-context.tsx:288` resets `autoRetriedJobIdRef.current = null` and any local flag before calling `mutate`.
  - `DashboardContextValue` (lines 17-54) gains three additive fields: `failedJob?: JobRecord`, `failureReason: string` (with the REQ-6 fallback applied), `autoRetrying: boolean`.
  - `value` object (lines 331-374) exports those three fields.
  - The auto-retry effect does not write to `status` (REQ-7): it only calls `mutate`, which itself calls `refresh()` after success. No optimistic update path exists.
- **Verification:**
  - `bun run typecheck` — green. No call site outside `DashboardShell` consumes the new fields; verified by grep over `apps/ui/src/`.
  - `bun run test` — green (existing suites untouched).
  - Manual: in `bun run dev:ui`, drive a failing apply (kill worker briefly, click Apply) and confirm PatchBar shows `Failed: <reason>` and `Apply now` is disabled for one tick before re-enabling.
- **Commit:** `feat(ui): auto-retry once on observed failed apply`

### TASK-4: Render the 5-state PatchBar from `DashboardShell`

- **REQ coverage:** REQ-1, REQ-4
- **Files:**
  - `apps/ui/src/dashboard/DashboardShell.tsx` (modify, lines 11-27, 53-64, 173-253; +25 LOC, ~8 LOC modified)
- **Acceptance:**
  - Destructure `failedJob`, `failureReason`, `autoRetrying` from `useDashboard()` at `DashboardShell.tsx:11-27`.
  - Replace the `pendingJob` inline computation at `DashboardShell.tsx:58-62` with the context-derived value (or keep the inline if simpler — TBD at apply time, must be consistent with design §7.5).
  - Pass new props to `<PatchBar>`: `failureReason`, `autoRetrying`.
  - `PatchBar` (lines 173-253) gains two new props. The 4-branch `label` ternary at `DashboardShell.tsx:189-195` is replaced by a call to `deriveBarState({ loaded, pendingJob, inSync, failureReason })` and a `switch (state.kind)`.
  - Render the labels exactly: `Checking patch state` / `Apply in progress` / `Failed: ${state.reason}` / `Patch pending` / `Patch live`.
  - `data-failed={state.kind === "failed" ? "true" : "false"}` is added to `.pc-sync-bar` (existing `data-drift` and color tokens stay).
  - Button `disabled` becomes `!loaded || autoRetrying` (REQ-4). Existing button text (`Apply now` / `Re-apply`) stays.
  - Existing `waiting = !loaded || pendingJob || !inSync` keeps current `data-drift` semantics. The failed bar still appears in `text-signal` because `waiting` is `true` while a failed job is the latest one (REQ-1.3 precedence is via label, not via `waiting`).
  - Regression check: with `failureReason === undefined`, the bar renders exactly as it does today across all `loaded`/`inSync`/`pendingJob` combinations.
- **Verification:**
  - `bun run typecheck` — green.
  - `bun run test` — green (regression: pure module tests still pass).
  - Manual: with the dev server, observe the 5 states in the bar by toggling apply success/failure; confirm the existing 4 states look byte-identical when there is no failed job.
- **Commit:** `feat(ui): render Failed state in PatchBar`

### TASK-5: End-to-end state-machine smoke (mock status payload)

- **REQ coverage:** REQ-1, REQ-2 (S2.3 chain), REQ-3 (S2.2 transition), REQ-5 (S2.2 reorder), REQ-6, REQ-7
- **Files:**
  - `apps/ui/src/dashboard/patch-bar-state.test.ts` (extend, +80 LOC appended)
- **Acceptance:** A new `describe("lifecycle")` block in the existing test file drives the pure module through:
  1. **Failed apply**: feed `selectFailedJob` + `deriveBarState` a `jobs` array containing `{id: "J1", status: "failed", errorMessage: "DNS validation timed out", createdAt: T1}`. Assert `state.kind === "failed"` and `state.reason === "DNS validation timed out"`.
  2. **Auto-retry fired, still failed (REQ-2 S2.3, REQ-3 S2.4)**: simulate the auto-retry effect's guard. After dispatch, `selectFailedJob` continues to return `J1` on subsequent polls because it is still the newest `failed` job. Assert that the chosen logic — the `newerExists` guard added in TASK-3 — means the auto-retry effect would NOT retrigger when the same id appears again. (The test asserts the *behavioral contract* via the pure selector, not the React effect.)
  3. **Newer job supersedes (REQ-3 S2.2)**: append `{id: "J2", status: "queued", createdAt: T2 > T1}` to the jobs array. Assert `selectFailedJob(jobs)` returns `undefined` and `deriveBarState` returns `applying`.
  4. **Reorder flicker (REQ-5 S2.2)**: shuffle `jobs` so the failed job moves position; assert `selectFailedJob` still returns the same failed job id.
  5. **Empty errorMessage fallback (REQ-6)**: feed `{id: "J3", status: "failed", errorMessage: ""}` and `{id: "J4", status: "failed", errorMessage: null}`; assert `deriveBarState` returns `{kind: "failed", reason: "Apply failed"}` for both.
  6. **No optimistic Failed (REQ-7)**: assert that `deriveBarState` with no failed job and `pendingJob === false` never returns `failed`, regardless of recent `apply()` calls. The pure module has no concept of time, so this is a structural assertion that no input path produces `failed` without `failureReason`.
- **Verification:**
  - `bun run test` — all assertions pass.
- **Commit:** `test(ui): cover PatchBar failure lifecycle with mock status payloads`

---

## Coverage matrix

| REQ | Tasks |
| --- | ----- |
| REQ-1 (Failed-state rendering) | T1, T4 |
| REQ-2 (auto-retry once per failed job) | T3, T5 |
| REQ-3 (persistence until next apply) | T3, T5 |
| REQ-4 (disable manual apply during auto-retry) | T3, T4 |
| REQ-5 (multi-failed ordering) | T1, T5 |
| REQ-6 (empty errorMessage fallback) | T1, T2, T4, T5 |
| REQ-7 (no optimistic update) | T3, T5 |

Every spec REQ (1–7) is covered by at least one task. REQ-2 and REQ-3 have no direct unit tests for the React effect (no DOM/test-infra in `apps/ui`), per design §8 — the lifecycle smoke in T5 covers the observable contract through the pure selector.

---

## Review Workload Forecast

- `estimated_changed_lines`: ~320 (production ~134, test ~150, comments/type-only ~36). Excludes generated goldens (none in this change).
- `chained_prs_recommended`: `false`
- `line_budget_risk`: `low`
- `decision_needed_before_apply`: `false`
- `single_pr_safe`: `true`

The 5 commits (one per task) sit inside one PR well under the 800-line review budget. The change is logically cohesive (one feature: a 5th bar state + one auto-retry) and there is no meaningful PR boundary inside it. Tests ship with the code they verify per work-unit-commits.

---

## Out-of-scope reminders for the implementer

- Do not add `vitest`/`@testing-library/react`/`happy-dom` to `apps/ui/package.json`. The pure module is sufficient and adding test infra would balloon scope.
- Do not modify `/api/status`, the worker, or the DB schema. The server already returns `errorMessage` and the `failed` job status.
- Do not add new design tokens. `data-failed="true"` is a future-theming hook; no CSS consumes it yet.
- Do not introduce `localStorage` persistence of the failure label.