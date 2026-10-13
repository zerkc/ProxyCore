# Feature: backup-ui

## Objective
Expose the existing Owner-only backup export/import API in the dashboard UI so a user can create and restore configuration backups without curl.

## Problem
`POST /api/backup/export` and `POST /api/backup/import?dry_run=` are implemented, wired in production and tested (`go test -short` green on 9 packages), but `apps/ui` has no backup surface. The original feature doc (`full-backup-export-import.md`) put the UI button out of scope.

## Scope
- New `/dashboard/backup` page (`BackupView`) with: Export (optional passphrase, blob download), Import (file picker, optional passphrase, dry-run preview, explicit confirm).
- API client functions for export/import in `apps/ui/src/api.ts`.
- Nav entry + route + icon.
- Runbook note about the UI.

## Out of scope
- Backend changes (contract already verified). Role persistence at login. Progress bars (export is streamed, no Content-Length).

## Backend contract (verified)
- Export: `POST /api/backup/export`, optional header `X-Proxycore-Backup-Passphrase` (base64url). Response `application/zip` + `Content-Disposition` filename.
- Import: `POST /api/backup/import?dry_run=true|false` (required), multipart `bundle` (file) + optional `passphrase` (base64url). 200 returns `ImportReport {dryRun, bundleSha256, formatVersion, exporterVersion, createdAt, installationId, nodeId, tables[{name,rowCount,action}], envWouldChange, certsToRestore[], appliedPostImport, compatibilityConflicts?[]}`.
- Errors `{error}`; 409 carries `code: "master_key_mismatch"`; 401 passphrase mismatch; 400 passphrase required / invalid archive / unsupported version / invalid dry_run; 413 too large (4 GiB); 503 master key not configured; 401/403 auth/role.
- A successful real import deletes all sessions: the UI must send the user to login afterwards.

## Constraints / conventions
- Do NOT use `api()` for multipart (forces JSON content-type); use raw `fetch` + `FormData`, `credentials: "include"`, following `downloadEnrollmentTrustCA`.
- Owner gating: render controls, map 403 to an "Owner access is required" message (as `EnrollmentTokenPanel`).
- UI copy in English; `pc-*` classes; `data-testid`; tests with vitest + `dashboard/testDom.ts` + `createElement` (no JSX), run from repo root.
- Reuse `src/components/PasswordInput.tsx` for passphrases.
- Backup is hidden on node-only surfaces (existing `getDashboardNavigation` behavior left unchanged).
- Advisory heuristic ~400 authored changed lines per task.

## Checks
`bunx vitest run <file>` while iterating; at closure `bun run test`, `bun run typecheck`, `bun run format:check`.
Test-first: RED then GREEN with vitest (runner available, offline).

## Tasks
- [x] **BU-1 — API client**: `exportBackup(passphrase?)` returning `{blob, filename}`, `importBackup(file, {passphrase, dryRun})` returning `ImportReport`, typed `BackupApiError` with kind (`master_key_mismatch`, `passphrase_required`, `passphrase_mismatch`, `invalid_bundle`, `too_large`, `unavailable`, `forbidden`, `unknown`), base64url passphrase encoding. Tests in `api.test.ts` (or `backupApi.test.ts`).
- [ ] **BU-2 — BackupView + wiring**: `apps/ui/src/dashboard/BackupView.tsx` (+ test), route in `App.tsx`, nav entry in `nav.ts`, icon in `DashboardShell.tsx`. Flow: pick file -> dry-run preview table -> confirm -> real import -> redirect to login.
- [ ] **BU-3 — Docs and stale text**: runbook UI section (`docs/runbooks/backup-restore.md`), fix stale "intentionally NOT wired" comment (`apps/api/cmd/server/sqlite_runtime.go:119-122`, `sqlite_runtime_test.go:140`), mark backend FB-* state in `full-backup-export-import.md`.

## Route declaration
- Exploration: delegated (2 read-only explorers: backend verification, UI mapping) — mapping trigger.
- BU-1..BU-3: delegated, one bounded writer (2+ non-trivial files).

## Progress
- Branch `feat/backup-ui` created from `main`. No tasks started.

## Verification evidence
- Backend: `go vet` clean; `go test -short -timeout 300s ./internal/httpserver/... ./internal/backupcore/... ./cmd/server/...` all pass (non-short suite not run).

## Delivery
- Strategy: ask-on-risk (default). Forecast well under 400 authored changed lines per task.

## Next step
Launch writer for BU-1.
