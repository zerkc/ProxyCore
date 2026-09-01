# Apply Progress — fix-patchbar-failed-apply-state

## Per-task Status

| Task | Status | Commit SHA | Notes |
|------|--------|------------|-------|
| T1: Pure state-machine + tests | ✅ done | `7246806` | `patch-bar-state.ts` + `patch-bar-state.test.ts` — 20 tests |
| T2: `errorMessage` on `StatusPayload.jobs` | ✅ done | `7720e0e` | Extracted `JobRecord` type with `errorMessage?: string \| null` |
| T3: Auto-retry wired in `DashboardProvider` | ✅ done | `dcb8f4d` | `autoRetriedJobIdRef`, `autoRetrying` memo, auto-retry effect, `autoRetryApply`, guard reset in `apply()` |
| T4: 5-state PatchBar rendered | ✅ done | `a352640` | `deriveBarState` switch, `data-failed` attr, `disabled={!loaded \|\| autoRetrying}` |
| T5: Lifecycle smoke tests | ✅ done | `43a3e27` | 9 new lifecycle tests appended to `patch-bar-state.test.ts` |

## File Diff Summary

- **Added**: `apps/ui/src/dashboard/patch-bar-state.ts` (pure state machine), `apps/ui/src/dashboard/patch-bar-state.test.ts` (29 tests total)
- **Modified**: `apps/ui/src/dashboard/types.ts` (+18 net LOC — extracted `JobRecord` with `errorMessage`)
- **Modified**: `apps/ui/src/dashboard/dashboard-context.tsx` (+102 net LOC — auto-retry mechanism)
- **Modified**: `apps/ui/src/dashboard/DashboardShell.tsx` (+27 net LOC — 5-state bar)

## Test Results

- `vitest`: 89 passed (23 test files) — no regressions
- `tsc --noEmit`: green
- New tests: 29 total in `patch-bar-state.test.ts` (20 T1 unit tests + 9 T5 lifecycle scenarios)

## Deviations from Tasks Plan

None — all tasks implemented as specced.

## Verification Notes

- T1 unit tests cover `selectFailedJob` (undefined input, empty, non-failed, newest by createdAt, tie-break by id, Date.parse guard) and `deriveBarState` (checking/applying/live/pending precedence, REQ-6 fallback).
- T5 lifecycle tests cover: S1 failed apply, S2 auto-retry guard contract (same id returns same job), S3 newer job supersedes (pendingJob wins), S4 reorder flicker immunity, S5a/S5b empty errorMessage fallback, S6 no optimistic failed (structural).
- `autoRetrying` memo correctly gates button disable via `!loaded || autoRetrying`.
- `deriveBarState` is a pure function — no React, no DOM, no time dependency.
