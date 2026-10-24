# Feature: update-progress

## Objective
Show a progress bar with the current step in the update dialog, driven by the status the UI already polls every 3 s.

## Problem (audit evidence)
- The updater runs 5 fixed steps (`pull`, `preflight`, `restart`, `verify`, `bootstrap`; `apps/updater/internal/updater/updater.go:197-290`) but publishes `status.Steps` only when a step fails (line 214) or when everything ends (line 290). While running, nobody can tell which step is active.
- The API keeps only `status` and `targetVersion` of the updater state (`UpdaterStatus`, `apps/api/internal/httpserver/update_handlers.go:33-36`).
- The dialog (`apps/ui/src/dashboard/VersionStatus.tsx`) shows a spinner with no progress.

## Decisions (user-approved)
- Step-based bar only: the step name plus completed/total steps. No parsing of `docker compose pull` output (fragile across Docker versions).
- The updater publishes the running step and the completed count as they change, under its existing mutex. Total is the fixed number of steps.
- `GET /api/status` exposes it only while `updating === true` as `updateProgress: { step, completed, total }`. `/api/updates` is unchanged.
- The UI shows a determinate bar with the step label when `updateProgress` is present, and keeps the current indeterminate spinner when it is absent. An old updater without the new fields must keep working (the updater is replaced last by the bootstrap sidecar, so the first update to this version runs with an old updater).
- `pull` can be slow on small hosts: the label names the step so a still bar is understood as "alive".
- Bar is clamped (0..total), never regresses, and API restarts or `updating === null` keep the last known progress.

## Out of scope
- Real byte-level pull progress; surfacing failure steps/output; persisting updater state across updater restarts.

## Constraints
- English for code, UI copy, comments, tests, commits. Conventional commits, no AI attribution in commits. One writer at a time.
- Advisory ~400 authored changed lines per task. Test-first when a runnable deterministic test exists (`go test -short`, UI vitest from repo root with `TZ=UTC`).

## Tasks
- [x] T1.6a Updater: publish current step and completed count while running (mutex-safe), tests. Commit 93c8c13. RED: tests failed to compile against the old code (undefined pipelineSteps, unknown Status fields); GREEN with `go test -short -race`. Step names/total come from one `pipelineSteps` list; `Last()` also copies Steps.
- [x] T1.6b API: carry progress through `UpdaterStatus` and the updater HTTP client; `updateProgress` in `GET /api/status` only while updating; tests (old updater payload without the fields gives no `updateProgress`). Commit 8a0ea83. RED: compile failure (unknown UpdaterStatus fields); GREEN afterwards. Runbook documented in the same commit.
- [x] T1.6c UI: progress bar and step label in the dialog, indeterminate fallback, tests. Commit 916373d. RED: 5 of the 6 new vitest cases failed before the implementation (the fallback case passed trivially). Progress is merged during render, not in an effect; the label is one string child because the test DOM does not update multi-node text.
- [x] T1.6d Release: bump `0.6.12` to `0.6.13`; document the field in `docs/runbooks/operations.md`. Bump commit b97f247; docs in 8a0ea83.

## Authorized scope
apps/updater (updater package, cmd), apps/api/internal/httpserver (update and status handlers, tests), apps/ui/src/dashboard (VersionStatus, types, tests), docs/runbooks/operations.md, the version files.

## Acceptance
Go tests green in api and updater, `bun run typecheck` clean, `TZ=UTC bunx vitest run apps/ui` green; the bar advances per step in the dialog and falls back to the spinner without progress data.

## Progress
T1.6a-d implemented on `feat/update-progress` (route: delegated direct, one writer). Checks: updater build/vet/`test -short -race` ok; api httpserver vet/test ok; `bun run typecheck` clean; `TZ=UTC bunx vitest run apps/ui` 227 passed.

## Route declaration
Planned: delegated direct (one writer, 2+ non-trivial files).

## Delivery
Own branch `feat/update-progress` off `main`, own PR with its own bump (`0.6.13`), merge, tag `v0.6.13`, user tests, then the Bun-to-Go migration continues with T2.

## Next step
Open the PR, merge, tag `v0.6.13`, user tests; then T2 of the Bun-to-Go migration.
