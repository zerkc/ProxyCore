# Feature: update-status-version

## Objective
Make the update dialog precise by folding the running version and the "is updating" flag into the status response the UI already polls every 3 s, and show an error when an update stops without the version changing.

## Problem (audit evidence)
- The polled `GET /api/status` (`apps/api/internal/httpserver/config_handlers.go:45-92`, polled every 3 s in `apps/ui/src/dashboard/dashboard-context.tsx:119-193`) carries neither the version nor the update state.
- Version and `updateInProgress` live only in `GET /api/updates` (`update_handlers.go:111-137`), polled every 30 min plus on demand by the dialog (`dashboard-context.tsx:145-164`, `DashboardShell.tsx:481`).
- The dialog (`apps/ui/src/dashboard/VersionStatus.tsx`) has no failed phase: a failed apply shows "Installing update" until the 5-minute watchdog (lines 112-116). Its own backoff is dead code because `refreshUpdate` swallows errors (lines 72-109).
- The updater overlay is best-effort and swallows errors (`update_handlers.go:126-135`), so "not updating" and "unknown" are indistinguishable.
- A 401 during the API restart redirects to `/login` (`dashboard-context.tsx`), which is wrong in the middle of an update.

## Decisions
- `GET /api/status` gains `version` (the `version.Version` build constant, never the checker cache) and `updating` (`true`, `false`, or `null` when the updater cannot be asked) plus optional `updateTarget`. `/api/updates` stays unchanged (still needed for `latest`).
- A shared helper reads the updater state for both handlers, with a short timeout so the 3 s poll never blocks on a slow updater.
- The dialog snapshots the version when the update starts and derives its phase from the polled status: `updating === true` is applying; `updating === false` and the version changed (normalizing a leading `v`) is done; `updating === false`, version unchanged, after `updating` was seen true once or after a grace period of about 15 s since the POST, is a new failed phase with an error message. `updating === null` and status request errors never count as failure. The login redirect is suppressed while the dialog is applying.
- Resume after reload works from status alone (also for NODE users, who can read status but not apply).

## Out of scope
- Surfacing the updater's failure steps in the response.
- Persisting the updater state across updater restarts.

## Constraints
- English for code, UI copy, comments, tests, commits. Conventional commits, no AI attribution in commits. One writer at a time.
- Advisory ~400 authored changed lines per task. Test-first when a runnable deterministic test exists (`go test -short`, UI vitest from repo root with `TZ=UTC`).

## Tasks
- [x] T1.5a Backend: `version`, `updating`, `updateTarget` in `GET /api/status`; shared updater-state helper with a short timeout; tests mirroring the `/api/updates` overlay and best-effort tests (running, idle, updater error gives `null`, no updater configured). Commit 315c0be. RED: new `status_update_test.go` failed (version empty, updating missing) before implementation; GREEN: `go build ./... && go vet ./internal/httpserver/... && go test -short -count=1 ./internal/httpserver/...` ok. Helper `Server.updaterState` (1 s timeout) shared by `handleUpdates` and `handleStatus`; tests also cover NODE, operator and a slow updater. Runbook documented in the same commit. Route: delegated direct.
- [x] T1.5b UI: status types, dialog driven by status, new failed phase with error, no redirect to login while applying, dialog tests (none exist today); drop the dialog's own polling loop. Commit 942cc8d. RED: new `VersionStatus.test.tsx` 7 of 8 failed before implementation; GREEN: `bun run typecheck` clean, `TZ=UTC bunx vitest run apps/ui` 221 passed. Deviations: `done` also requires `updating !== true` and treats a changed version with `updating: null` as done; the 5-minute watchdog (timeout phase, Keep waiting) is kept; `dashboard-context` gained `setUpdateApplying` (ref) to suppress the 401 redirect. Route: delegated direct.
- [x] T1.5c Release: bump version to `0.6.12`, docs touched by the contract (`docs/runbooks/operations.md`). Bump commit 18ccd60; runbook docs shipped inside 315c0be. Go httpserver tests, typecheck and UI vitest re-run green after the bump.

## Authorized scope
apps/api/internal/httpserver (status and update handlers and their tests), apps/ui/src/dashboard (VersionStatus, dashboard-context, DashboardShell, types, tests), docs/runbooks/operations.md, the version files.

## Acceptance
Go tests green, `bun run typecheck` clean, `TZ=UTC bunx vitest run apps/ui` green; an update that ends with the same version shows an error in the dialog; a normal update still ends in done.

## Progress
T1.5a, T1.5b, T1.5c implemented on `feat/update-status-version` (commits 315c0be, 942cc8d, 18ccd60). Independent verification and PR pending.

## Route declaration
Planned: delegated direct (one writer per task, 2+ non-trivial files each). Mapping was one read-only explorer (7 tool uses).

## Delivery
Own branch `feat/update-status-version` off `main`, own PR with its own version bump (`0.6.12`), merge, tag `v0.6.12`, user tests, then the Bun-to-Go migration continues with T2.

## Next step
Parent spot check, then PR, merge, tag `v0.6.12`.
