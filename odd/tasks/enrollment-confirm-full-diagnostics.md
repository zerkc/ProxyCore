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

## Route
Delegated direct: one writer (httpserver + enrollment packages, 2+ non-trivial files).

## Progress / evidence
- T1-T4 (commit: see git log "fix(enrollment): log reason, step and redacted cause at every workflow exit"): every reject goes through `reject(op, step, reason, status, message, detail)` in enrollment_workflow.go; `enrollment.RedactedCause` (sentinel names + %T chain, never err.Error()) is shared; NodeConverter.Convert logs `node conversion rejected reason=... cause=...` on every exit; typed stage errors (`ErrNodeConversionImport`, `ErrNodeConversionApplyWait`, `ErrNodeConversionUnavailable`) let the handler log `class=`. Tests: enrollment_workflow_log_test.go, node_converter_log_test.go (RED observed before implementation, GREEN after, no-leak assertions). HTTP responses unchanged.
