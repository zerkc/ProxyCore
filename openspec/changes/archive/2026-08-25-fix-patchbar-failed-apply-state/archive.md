# SDD Archive Report — fix-patchbar-failed-apply-state

## 1. Summary

Five commits landed cleanly on the current branch, implementing a 5-state PatchBar in `apps/ui` that surfaces apply failures as `Failed: <reason>` and auto-retries once per distinct failed job id before persisting the failure state until the next apply. The change touches only `apps/ui`: `patch-bar-state.ts` (pure state machine), `types.ts` (+`JobRecord.errorMessage`), `dashboard-context.tsx` (auto-retry mechanism), and `DashboardShell.tsx` (5-state bar rendering). No API, worker, or DB contracts changed.

**Verdict: PASS.** All 7 REQs satisfied. 89/89 tests green. tsc clean. Zero CRITICAL findings.

| REQ | Short name | Status |
|-----|------------|--------|
| REQ-1 | Failed-state rendering | PASS |
| REQ-2 | Auto-retry once per failed job | PASS |
| REQ-3 | Persistence until next apply | PASS |
| REQ-4 | Disable manual apply during auto-retry | PASS |
| REQ-5 | Multiple-failed ordering | PASS |
| REQ-6 | Empty errorMessage fallback | PASS |
| REQ-7 | No optimistic update | PASS |

---

## 2. Final commit list

| Commit | Subject | REQs advanced |
|--------|---------|---------------|
| `7246806` | `feat(ui): add patch-bar state machine and unit tests` | REQ-1, REQ-5, REQ-6 |
| `7720e0e` | `feat(ui): expose job errorMessage on StatusPayload` | REQ-1, REQ-6 |
| `dcb8f4d` | `feat(ui): auto-retry once on observed failed apply` | REQ-2, REQ-3, REQ-4, REQ-7 |
| `a352640` | `feat(ui): render Failed state in PatchBar` | REQ-1, REQ-4 |
| `43a3e27` | `test(ui): cover PatchBar failure lifecycle with mock status payloads` | REQ-1..REQ-7 (coverage) |

---

## 3. Artifacts index

All SDD artifacts created during this change, in phase order:

| Topic key | Engram ID | Type | Mirror |
|-----------|-----------|------|--------|
| `sdd/fix-patchbar-failed-apply-state/explore` | #2750 | discovery | `openspec/changes/fix-patchbar-failed-apply-state/proposal.md` (root-cause) |
| `sdd/fix-patchbar-failed-apply-state/proposal` | #2751 | architecture | `openspec/changes/fix-patchbar-failed-apply-state/proposal.md` |
| `sdd/fix-patchbar-failed-apply-state/spec` | #2752 | architecture | `openspec/changes/fix-patchbar-failed-apply-state/spec.md` |
| `sdd/fix-patchbar-failed-apply-state/design` | #2753 | architecture | `openspec/changes/fix-patchbar-failed-apply-state/design.md` |
| `sdd/fix-patchbar-failed-apply-state/tasks` | #2754 | decision | `openspec/changes/fix-patchbar-failed-apply-state/tasks.md` |
| `sdd/fix-patchbar-failed-apply-state/apply-progress` | #2755 | decision | `openspec/changes/fix-patchbar-failed-apply-state/apply-progress.md` |
| `sdd/fix-patchbar-failed-apply-state/verify-report` | #2757 | decision | `openspec/changes/fix-patchbar-failed-apply-state/verify-report.md` |
| `sdd/fix-patchbar-failed-apply-state/archive-report` | #2758 | decision | `openspec/changes/fix-patchbar-failed-apply-state/archive.md` |

OpenSpec mirror directory: `openspec/changes/fix-patchbar-failed-apply-state/` (8 files total including this archive report).

---

## 4. Risks carried forward

Two pre-existing WARNINGs from the verify-report — no new risks introduced by this change:

| ID | Severity | Location | Claim |
|----|----------|----------|-------|
| WARN-1 | low | `patch-bar-state.test.ts:167-180` | REQ-2 S2.3 (auto-retry guard contract) is tested structurally via `selectFailedJob` determinism, not via a React effect test that verifies the ref actually blocks retrigger. Acknowledged in test comment. Caused by: no React test infra in `apps/ui` (pre-existing, T5 design §8). |
| WARN-2 | low | `dashboard-context.tsx:205-216` | REQ-3 S2.1 (persistence across navigation) is verified by code inspection of context state lifespan, not by a DOM/routing test. Caused by: no React test infra in `apps/ui` (pre-existing, T5 design §8). |

Design-level low-severity risks (design §10: R-D1 through R-D7) were all accepted. None materialized.

---

## 5. Non-goals reaffirmed

From the proposal's non-goals list:

- No backend, worker, or DB changes.
- No new design tokens, colors, icons, or copy surfaces.
- No persistent (localStorage) failure memory across full reloads.
- No change to apply semantics, retry policy, or job lifecycle.
- No new endpoints, telemetry, or audit-log surface.

All non-goals held — zero scope creep.

---

## 6. Open follow-ups

Concrete next actions for the maintainer:

1. **Add React test infra to `apps/ui`**: `apps/ui` currently has no `vitest`, `@testing-library/react`, or `happy-dom`. When that infrastructure lands, add DOM-level tests for REQ-2 S2.3 (auto-retry guard contract via effect test) and REQ-3 S2.1 (persistence across navigation via routing test) to replace the code-review verification currently noted as WARN-1 and WARN-2.

2. **Polish `Failed: Apply failed` phrasing** (design R-D3): When `errorMessage` is empty, the label renders as `Failed: Apply failed` (double "failed"). A future polish could render just `Apply failed` (no `Failed:` prefix) when the reason is the fallback — keeping it out of scope avoided confusing operators who already saw `Failed:` in tests.

3. **`data-failed` CSS hook**: `data-failed="true"` was added to `.pc-sync-bar` but no CSS rule consumes it yet (design R-D6). A future theming change can use it to color or style the failed bar distinctly.

4. **UX nudge during auto-retry** (design R-D4): The `Apply now` button is disabled while `autoRetrying` but there is no explanatory message. A future affordance could surface "Retrying…" to operators who click immediately after a failure.

---

## 7. Filesystem archive operations

This report originally existed as a verify-adjacent write-up while the change
folder was still active. The archive phase completes the OpenSpec/hybrid
close-out:

| Operation | Path | Status |
|-----------|------|--------|
| Delta spec (new capability) | `openspec/changes/fix-patchbar-failed-apply-state/specs/patchbar/spec.md` | Created (7 ADDED requirements) |
| Main spec (no prior spec) | `openspec/specs/patchbar/spec.md` | Created from delta |
| Task checkbox reconciliation | `tasks.md` | 5/5 marked `[x]` with apply-progress proof |
| Change folder move | `openspec/changes/archive/2026-08-25-fix-patchbar-failed-apply-state/` | Target |
| Docker verification | N/A | UI-only; no compose/runtime path changed |

### Specs synced

| Domain | Action | Details |
|--------|--------|---------|
| patchbar | Created | 7 ADDED requirements (Failed-state rendering, Auto-retry once per failed job, Persistence until next apply, Disable manual apply during auto-retry, Multiple-failed-jobs ordering, Empty errorMessage fallback, No optimistic update on failure) |

### Task reconciliation reason

The original `tasks.md` used `TASK-N` headings without `- [x]` markers.
`apply-progress.md` and `verify-report.md` prove all five tasks landed
(commits `7246806`, `7720e0e`, `dcb8f4d`, `a352640`, `43a3e27`). The
maintainer explicitly requested archive of this change. Checkboxes were
added as an exceptional archive-time reconciliation so the audit trail
does not show stale incomplete work.

### RDD / review gate

RDD is disabled for this project (`next_transition.kind: stop,
reason_code: rdd_disabled`). Archive proceeds on verify PASS + explicit
user request; no native review receipt is required.
