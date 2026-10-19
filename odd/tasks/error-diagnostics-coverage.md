# Error diagnostics coverage

## Objective
Add actionable, redacted diagnostics at failure boundaries without removing existing logs or messages.

## Specs
- S1: "bien verifica bien todo lo que falta y agrega los logs"
- S2: "por si las dudas solo agrega no saques"
- S3: "cuando sucede un error me faltan muchos logs para poderlo rastrear correctamente y pierdo mucho tiempo desplegando versiones para agregar mas lgos"
- S4: "si" (reply to "¿Querés que amplíe esa correlación y trate aparte la prueba de fecha?")
- S5: "no incluyelos en este"

## Tasks
- T1 (S1-S3): Add safe worker apply/rollback failure diagnostics and tests. Route: inline. Commit: not authorized.
- T2 (S1-S3): Add safe control socket request failure diagnostics and tests. Route: inline. Commit: not authorized.
- T3 (S1-S3): Add API request correlation and safe exchange failure diagnostics and tests. Route: inline. Commit: not authorized.
- T4 (S1-S5): Check remaining failure boundaries, functional checks, documentation and independent security verification. Route: inline/verify. Commit: not authorized. Status: verified.
- T5 (S4): Add request IDs to both dedicated TLS listener constructions and tests. Route: inline. Commit: not authorized.
- T6 (S4): Log a safe API request-to-job bridge and job correlation in worker, with tests. Route: inline. Commit: not authorized.
- T7 (S4): Make the UI date assertion timezone-aware without changing displayed behavior, with focused and suite checks. Route: inline. Commit: not authorized.
- T8 (S5): Bridge backup restore apply enqueue through request log, with dry-run/error tests. Route: inline. Commit: not authorized.
- T9 (S5): Bridge certificate HTTP-01 apply enqueue only after successful commit, with tests. Route: inline. Commit: not authorized.

## Log
- L1: Hola Puedes verificar que tenga todos los logs necesarios, es que cuando sucede un error me faltan muchos logs para poderlo rastrear correctamente y pierdo mucho tiempo desplegando versiones para agregar mas lgos
- L2: bien verifica bien todo lo que falta y agrega los logs
- L3: por si las dudas solo agrega no saques
- L4: Read-only inspection found existing HTTP request logs and enrollment step logs, with missing cross-process correlation, worker apply/rollback diagnostics, control failure logs, and exchange transport classifications. Preserve existing logs and responses; do not print tokens, payloads, request bodies, private keys, or raw error strings in new diagnostics.
- L5: T1 RED: apply.test.ts failed on missing diagnostics (1 failed, 6 passed). GREEN: focused 7 passed, typecheck passed. Added safe job/revision/step/service log lines and preserved existing persisted messages. No commit requested.
- L6: T2 RED: transport-log.test.ts failed on missing log (1 failed). GREEN: focused 1 passed, typecheck passed. Added opt-in control failure logging without altering response or other logs. No commit requested.
- L7: T3 RED: request correlation test failed 1; exchange diagnostic test failed to compile (missing Logf). GREEN: Go 647 tests across internal/sync, internal/httpserver and cmd/server passed. Generated request ID in X-Request-ID and HTTP log; exchange logs only safe classes/statuses. No commit requested.
- L8: Independent read-only verifier found missing worker pre-try, control socket setup and exchange local-validation diagnostics; S2 additive behavior was met, S1/S3 incomplete. Added each with RED test: worker lookup (1 failed/7 passed), control mkdir (1 failed/1 passed), exchange invalid origin (1 failed). GREEN: focused TS 11/11, Go 648 across 3 packages, typecheck clean; git diff --check clean. Full Go go test ./... passed before the follow-up edits. Full TS bun run check failed (1 UI date assertion: expected January 1, 2030, got December 31, 2029; 340 passed, 7 skipped). No automatic retry. RDD mode off. Remaining scope: separate TLS listener lacks common request-ID middleware, API ID does not propagate through queue, and older raw-error logs remain unchanged per L3. No commit requested.
- L9: User: si. Authorizes TLS/request-to-job correlation and a separate UI date test fix. Inspection: job already has generated correlationId; preserve that persisted contract and bridge request_id to job/correlation in API log, then include correlation in worker log. UI formats using host-local Intl.DateTimeFormat, so expectation should follow host locale/timezone without changing display.
- L10: T5 RED: both dedicated TLS listener factory tests failed on missing X-Request-ID/log (2 failed); GREEN: both passed. Wrapped final composed handlers with the existing redacted RequestLogMiddleware; original response statuses retained. No commit requested.
- L11: T6 RED: public HTTP apply test failed on missing bridge (1 failed); worker log test failed on missing correlation field (1 failed). GREEN: focused Go 15 passed (bridge, config routes, request logs), worker 9 passed, typecheck passed. Existing DB-generated job correlationId preserved; new request line contains job/revision/correlation, worker failure log contains correlation. Covers HTTP apply, zone, record, ingress side effect and topology promotion enqueue; separate background work has its own correlation ID. No commit requested.
- L12: T7 RED: timezone-dependent enrollment trust UI assertion failed (expected January 1, got December 31); GREEN: focused 9 passed and typecheck passed after asserting the viewer-local formatted expiry. Display behavior unchanged. Full TS suite 344 passed, 7 skipped; full Go go test ./... passed after T5-T7 implementation. No commit requested.
- L13: Independent verifier S2 met and S4 partial: dedicated TLS handlers and ordinary API apply/zone/record/ingress/topology job bridge pass; two other enqueue paths lack bridge: backup restore trigger in internal/backupcore/httpexport/apply.go and HTTP-01 certificate issue in internal/configuration/sqlite_certificate_acme.go. They discard the ApplyResult behind deeper contracts; reaching them would expand this already ~400-line multi-area diff. Documentation scopes the bridge accurately. git diff --check clean. Ask before broadening into these contracts. No commit requested.
- L14: User: no incluyelos en este. Scope enlarged to include backup restore and certificate HTTP-01 in this change. Read-only map located a context-carried observer at durable enqueue success, with no observer on dry run/failure; preserve existing ApplyTrigger and certificate return contracts.
- L15: T8 RED: backup public HTTP bridge test failed on real restore (2 failures, 2 passes including dry run and rejected enqueue). GREEN: focused Go 5 passed across httpserver/httpexport. Added context-scoped observer attached by backup handler and invoked after successful CreateApplyJob in backup adapter; dry-run/rejected enqueue do not report a job. No commit requested.
- L16: T9 RED: HTTP-01 certificate observer test failed active issuance with zero observed jobs (1 failed). GREEN: focused 8 configuration tests (including no-pool no-observer) and 1 public HTTP certificate test (422 after enqueue still logs job). Notification occurs only after writer.Write commits; handler attaches request observer without altering certificate response. No commit requested.
- L17: Independent verifier found duplicate notification in concrete SQLite backup composition because shared sqliteCreateApplyJob and backup adapter both notified. RED: standalone helper notification test failed (1). Moved notification to HTTP-01 caller after successful helper commit, leaving backup adapter as sole backup notifier. GREEN: 8 focused tests across configuration/httpexport/httpserver and git diff --check. Earlier full Go and TS suites passed before this narrow relocation. No commit requested.
- L18: Final Go go test ./... passed after deduplication; TypeScript bun run check passed (344 passed, 7 skipped). No Docker end-to-end run. All tracked behavioral tasks verified; independent verifier S2/S5 met after targeted duplicate fix and focused checks. Diff is ~550 added / 33 removed lines across multiple areas (review workload risk, user explicitly requested same change). Changes uncommitted by policy; no push or delivery.
