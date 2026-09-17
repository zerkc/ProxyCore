# Apply Progress: Primary/Node continuity — Phase 2

## Status

- **Phase:** apply
- **Change:** `primary-node-continuity-phase-2`
- **Bounded slice:** User-approved PRIMARY/NODE continuity boundary: persisted-role startup, redacted status, HTTP read-only enforcement, and existing dashboard inspector visibility. This is layered on the completed WU1 persistence work and is not the full Phase 2 apply.
- **Structured status consumed:** `gentle-pi.sdd-status` v1; `artifactStore=openspec`; `applyState=ready`; `nextRecommended=apply`; `actionContext.mode=repo-local`; workspace root `/root/Projects/ProxyCore`; allowed edit root `/root/Projects/ProxyCore`; warnings: none.
- **Delivery decision:** `exception-ok` with explicit user acceptance of `size:exception` for the overall Phase 2 change. This run remains bounded to the continuity slice; no chain was started.
- **Workload boundary:** WU1 persistence remains complete; only the bounded startup/status/mutation/UI behavior below was added. Work Units 2–9 remain otherwise incomplete.
- **Scope guard:** Enrollment, synchronization, role selectors, service-level mutation capabilities, renewal gating, archives, and end-to-end Phase 2 acceptance are not claimed complete.

## Completed implementation tasks

The following implementation-owned WU1 checkboxes are marked `- [x]` in `tasks.md`:

- RED schema-parity and migration tests.
- GREEN additive Go/Drizzle schema and `0004_primary_node_continuity_phase_2.sql`.
- GREEN typed Go/TypeScript records, row decoders, transaction ports, and persistence adapters.
- TRIANGULATE migration/setup idempotency and PostgreSQL uniqueness checks.
- REFACTOR centralized Go schema SQL/row decoding and formatted Go/TypeScript sources.
- Focused verification.
- Runtime harness.
- Rollback boundary documentation.

The WU1 reviewable-commit checkbox remains unchecked because the user explicitly instructed: **do not commit**.

## Current bounded slice completed tasks

- Startup now loads any valid persisted topology role and bootstraps only an absent identity row.
- `/api/status` now carries the same redacted identity fields already exposed by `/api/ready`.
- Authenticated ordinary non-GET HTTP routes share a writable-identity gate; NODE and stale-primary receive `403 TOPOLOGY_READ_ONLY`, while standalone and access/recovery auth routes remain available. This is an HTTP-only slice, not the later service-level mutation policy.
- Dashboard status types/context and the existing shell inspector display role, installation ID, node ID, both generations, writable, and stale-primary state; no selector or enrollment/sync control was added.
- Focused startup, status/mutation, and inspector tests were added within the allowed surfaces.

## Files changed

- Go configuration persistence: `apps/api/internal/configuration/schema.go`, `rows.go`, `store.go`, `types.go`, plus `phase2_schema.go`, `phase2_types.go`, `phase2_rows.go`, and `phase2_store.go`.
- Go tests: `apps/api/internal/configuration/phase2_schema_test.go`.
- Drizzle schema/contracts: `packages/db/src/schema.ts`, `ports.ts`, and `persistence.ts`.
- TypeScript tests: `packages/db/src/phase2-schema.test.ts` and `phase2-persistence.test.ts`.
- Migration artifacts: `packages/db/migrations/0004_primary_node_continuity_phase_2.sql`, `meta/0004_snapshot.json`, and `meta/_journal.json`.
- OpenSpec progress/task state: this file and the WU1 implementation checkboxes in `tasks.md`.
- Bounded continuity API/startup: `apps/api/cmd/server/main.go`, `main_test.go`, `apps/api/internal/httpserver/server.go`, `config_handlers.go`, `server_test.go`, and `config_handlers_test.go`.
- Bounded continuity UI: `apps/ui/src/dashboard/types.ts`, `dashboard-context.tsx`, `DashboardShell.tsx`, and `DashboardShell.test.tsx`.
- OpenSpec progress/task state: this file and the bounded continuity section in `tasks.md`.

No files outside the allowed edit surfaces were changed by this apply slice; pre-existing `.pi`, `.codegraph`, and `odd` paths were preserved.

## Persistence delivered

- Added the additive enrollment, credential, grant, acknowledgement, enrollment-attempt, synchronization-attempt, and standalone-archive tables.
- Added checked lifecycle/source/status values, UUID and UTC timestamp columns, foreign keys, uniqueness constraints, and the one-active-enrollment partial unique index.
- Added node synchronization state, retry timing/failure counters, source attribution, snapshot identity, linked apply-job metadata, and applied-snapshot pending/terminal status.
- Preserved the legacy `node_state.enrollment_token_hash` column as readable/inert; no new persistence path writes plaintext enrollment tokens.
- Added typed Go row mappings and a PostgreSQL transaction port with compare-and-set enrollment transitions and idempotent acknowledgement writes.
- Added matching TypeScript records, transaction ports, in-memory contract implementation, and PostgreSQL persistence adapter.

## Exact verification evidence

### Safety net

- `cd apps/api && go test ./internal/configuration ./internal/identity` — passed before modifying existing Go files.
- `bun run test -- packages/db/src` — passed before modifying existing TypeScript files: 1 file, 3 tests.

### Strict TDD cycles

- RED Go: `cd apps/api && go test ./internal/configuration -run 'TestPhase2SchemaContract'` — failed to compile because `phase2SchemaContract` did not exist.
- RED TypeScript: `bun run test -- packages/db/src/phase2-schema.test.ts` — failed with 2 failing tests because the contract export and `0004` migration did not exist.
- GREEN: schema parity tests passed after the minimum schema/migration implementation.
- TRIANGULATE: TypeScript persistence tests passed with non-empty transaction data and an invalid-state branch; disposable PostgreSQL tests exercised migration/setup idempotency, legacy row readability, indexes, duplicate active attempts, and duplicate installation bindings.
- REFACTOR: `gofmt` and Prettier cleanup completed; focused suites were rerun successfully.

### Focused and full tests

- `cd apps/api && go test ./internal/configuration ./internal/identity` — passed.
- `bun run test -- packages/db/src` — passed: 3 files, 7 tests.
- `bunx tsc --noEmit --pretty false` — passed.
- `cd apps/api && go test ./...` — passed across all API packages.

### Runtime harness

Disposable PostgreSQL 16 was available. The harness ran:

1. With `DISPOSABLE_DB_URL` set only in the shell to the disposable PostgreSQL URL: `DATABASE_URL="$DISPOSABLE_DB_URL" bun run db:migrate` — passed.
2. The same `DATABASE_URL="$DISPOSABLE_DB_URL" bun run db:migrate` command a second time — passed/no-op.
3. `PHASE2_DATABASE_URL="$DISPOSABLE_DB_URL" bash -c 'cd apps/api && go test ./internal/configuration -run TestEnsureSchemaIsIdempotentAgainstMigratedPostgres -count=1'` — passed.
4. The runtime test invoked `EnsureSchema` twice, verified all eight WU1 tables and required indexes, preserved a legacy standalone row, and rejected duplicate active attempts and duplicate installation bindings.

Sensitive values were not recorded in this artifact.

## TDD Cycle Evidence

| Task | Test file | Layer | Safety net | RED | GREEN | TRIANGULATE | REFACTOR |
|---|---|---|---|---|---|---|---|
| WU1 schema contract | `apps/api/internal/configuration/phase2_schema_test.go`, `packages/db/src/phase2-schema.test.ts` | Unit + PostgreSQL integration | Go 2 packages passed; DB package 3 tests passed | Compile/test failures recorded above | Schema/migration parity passed | Idempotency, legacy-row, index, active-attempt, and node-binding checks passed on disposable PostgreSQL | Centralized `phase2SchemaContract`; formatted and reran |
| WU1 cross-process ports | `packages/db/src/phase2-persistence.test.ts` | Unit | DB package baseline passed | Missing `InMemoryContinuityPersistence` failed as expected | Transaction persistence and invalid state cases passed | In-memory transaction boundary exercises separate records and acknowledgement key | Port and adapter mappings formatted; focused suite reran |

### Current bounded slice strict TDD cycle

- **RED:** `cd apps/api && go test ./cmd/server` failed to compile because the new `bootstrapIdentity` seam was not implemented yet; `bun test apps/ui/src/dashboard/DashboardShell.test.tsx` reported 1 error because the new inspector-row export was not implemented yet. The database-backed HTTP RED cases could not execute because `DATABASE_URL` was unset in this environment.
- **GREEN:** `cd apps/api && go test ./cmd/server ./internal/httpserver` passed (51 tests across 2 packages); `bun test apps/ui/src/dashboard/DashboardShell.test.tsx` passed (1 test); `bun run typecheck` passed.
- **TRIANGULATE:** The bounded tests cover persisted NODE startup plus absent-identity bootstrap, standalone HTTP write success, NODE and stale-primary HTTP write rejection, redacted status field coverage, and non-writable inspector rendering data.
- **REFACTOR:** Shared `publicIdentity` serialization now feeds both readiness and status, the HTTP write gate is centralized in authenticated request handling, and focused tests were rerun after formatting/cleanup.

### Independent verification and disposable PostgreSQL spot check

- **First verifier run:** With `DATABASE_URL` unset, `cd apps/api && go test ./cmd/server ./internal/httpserver` passed the non-database cases but skipped the database-backed handler tests. That run did not execute the status/mutation integration coverage.
- **Follow-up spot check:** With `DATABASE_URL` set to the disposable PostgreSQL instance, `cd apps/api && go test ./cmd/server ./internal/httpserver -count=1` passed both packages and executed the database-backed tests.

## Deviations

- Drizzle generation surfaced `internal_ca` and `users.password_change_required` as already-owned by earlier migrations; redundant statements were removed from `0004` so the additive migration remains runnable after migrations `0000`–`0003`.
- The generated migration metadata is retained; migration SQL is generated output and is not counted as authored review work.
- No commit, review receipt, validation receipt, or delivery gate was created; those are parent lifecycle responsibilities.

## Remaining work

- WU1 reviewable commit outcome remains unchecked because committing is explicitly prohibited for this run:
  `- [ ] **Reviewable commit outcome:** Produce \`feat(db): add phase 2 continuity persistence contracts\` with schema, store, and adjacent tests together. <!-- sdd-owner: implementation -->`
- Work Units 2–9 remain otherwise incomplete and unchecked. This slice does not claim enrollment security primitives, primary authority, snapshot sync, node commit, scheduler, service-level mutation policy, role selectors, renewal gating, archive persistence, or end-to-end acceptance.
- Parent-owned lifecycle checkboxes remain byte-for-byte unchanged.

## Rollback boundary

Revert only the bounded startup identity load, redacted status serialization, HTTP writable-identity gate, dashboard inspector fields, and adjacent tests/artifacts. Retain WU1 additive Phase 2 tables/columns and referenced data; no destructive down-migration is part of this slice.

## Next recommendation

`parent-lifecycle` — review/commit decisions remain parent-controlled. Do not apply Work Unit 2 or later in this run.
