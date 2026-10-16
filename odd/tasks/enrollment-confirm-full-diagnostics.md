# enrollment-confirm-full-diagnostics

## Objective
Every failure of the NODE enrollment flow (draft, preview, confirm, exchange, accept, validation, trust/sync recording, conversion) must leave a redacted, actionable line in the API log, so a failing real deployment is diagnosable from logs alone.

## Problem (evidence)
After v0.6.4 a real node was linked (TLS listener went ineligible at 05:50:19) yet the UI got `{"error":"enrollment conversion denied"}` and the node log had no line for the attempt. That message is produced at `enrollment_workflow.go:386`, `:421` and by the default branch of `genericConversionReason`; none of these logs, several other confirm exits (`identity transition denied`, `primary snapshot denied`, `enrollment workflow unavailable`, `Convert` errors, exchange failures) also log nothing, and `NodeConverter.Convert` returns early without logging.

## Scope
- T1 Log a stable reason code and step at EVERY non-2xx exit of draft/preview/confirm handlers and a success line for confirm.
- T2 Log the redacted cause class (sentinel code plus Go error type chain, never raw messages, tokens, keys, payloads) for errors from exchange, accept, KEK load, validation issues count, recordTrust, recordSync and Convert.
- T3 `NodeConverter.Convert` logs each early return and failure (unavailable, ctx, ineligible, import, apply wait, terminal status, identity commit) with the same redaction rules.
- T4 Tests assert the codes are logged and that no secret material leaks.

## Constraints
- Artifacts in English. HTTP responses unchanged. No secrets, tokens, keys, payloads, raw error strings in logs.
- Test-first where a deterministic test exists. Advisory ~400 changed lines per task.

## Tasks
- [x] T1 reason code at every exit of the workflow handlers
- [x] T2 redacted cause class for dependency errors
- [x] T3 NodeConverter.Convert step logging
- [x] T4 tests incl. no-leak checks
- [x] T5 request logging middleware for every endpoint + endpoint cause audit
- [x] T6 node sync cycle and Resync redacted cause logging

## Route
Delegated direct: one writer (httpserver + enrollment packages, 2+ non-trivial files).

## Progress / evidence
- T1-T4 (commit: see git log "fix(enrollment): log reason, step and redacted cause at every workflow exit"): every reject goes through `reject(op, step, reason, status, message, detail)` in enrollment_workflow.go; `enrollment.RedactedCause` (sentinel names + %T chain, never err.Error()) is shared; NodeConverter.Convert logs `node conversion rejected reason=... cause=...` on every exit; typed stage errors (`ErrNodeConversionImport`, `ErrNodeConversionApplyWait`, `ErrNodeConversionUnavailable`) let the handler log `class=`. Tests: enrollment_workflow_log_test.go, node_converter_log_test.go (RED observed before implementation, GREEN after, no-leak assertions). HTTP responses unchanged.

- T5: `httpserver/request_log.go` adds `RequestLogMiddleware` (wired in `Server.Handler()`): `http <METHOD> <route> status= dur= bytes= [err="..."] [cause=[...]]`; route = mux pattern or segment-redacted path, no query/headers/bodies, error text truncated to 200 and secret-redacted, health/static only on >=400, panics recovered and logged by type. `noteCause(ctx, step, err)` attaches a redacted sentinel+type-chain cause to that line; added at handlers that hid the cause: enrollment_tokens.go, topology_nodes.go, enrollment_trust.go, enrollment_grant_exchange.go, enrollment_publication.go, enrollment_acknowledgment.go, enrollment_snapshot_by_token.go (one-line additions, responses unchanged). Config/user/certificate handlers already forward the error text in the response so the middleware captures it. Tests: request_log_test.go (table, passthrough, panic, real router), request_log_internal_test.go.

- T6: `internal/sync/node_sync.go` now logs `node sync cycle failed: <code> step=<fetch|unmarshal|hash|store|store_load|store_save|apply|acknowledge|not_modified_mismatch|hash_header_mismatch> cause=sentinels:..|types:..` (diagnostic captured per cycle, failure pointers unchanged). `NodeConverter.Resync` logs `node resync rejected reason=<unavailable|context_done|resync_denied|import_failed|apply_wait_failed|apply_not_applied>` with role/key presence, terminal status and `failure=` (apply job failure text is free text, so only the known "superseded" message maps to a name; anything else is reduced to its length). `cmd/server/node_sync.go`: Apply closure extracted to `nodeSyncApply` and logs `snapshot_denied issues=N`, `resync_failed cause=`, plus credential load/build failures that were previously reported only as credential_rejected. Tests: internal/sync/node_sync_log_test.go, node_converter_log_test.go (Resync; written after the implementation, not RED-first), cmd/server/node_sync_test.go (apply and credential logging).
