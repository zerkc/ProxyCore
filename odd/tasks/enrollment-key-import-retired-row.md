# enrollment-key-import-retired-row

## Objective
A node that already holds a (retired) copy of the primary's cluster KEK must be able to enroll, and a real KEK conflict must be diagnosable by cause.

## Problem (evidence)
Real node log after v0.6.3: `enrollment bootstrap rejected reason=key_conflict` then `enrollment confirm rejected reason=accept_denied`.
`SQLiteKeyStore.ImportInWrite` (apps/api/internal/cluster/sqlite_import.go:21-45) collapses several causes into `ErrClusterKeyDenied`:
1. active row with same id and different material;
2. row with same id but `retired_at` set: `loadByID` filters `retired_at is null`, so the row looks missing, the insert hits the primary key and fails;
3. row exists but cannot be unwrapped (master key mismatch);
4. any other insert failure.
Inference (unproven): the node kept a retired row for the primary's KEK from a previous enrollment/demotion (`sqlite_topology.go:154-171` retires keys). The docker e2e uses fresh databases, so it never exercises this.

## Scope
- T1 `ImportInWrite` distinguishes the four causes with sentinel errors that still satisfy `errors.Is(err, ErrClusterKeyDenied)`.
- T2 When a RETIRED row has the same id AND its unwrapped material equals the incoming KEK, reactivate it (clear `retired_at`) instead of failing. Any mismatch, unwrap failure or other insert failure stays denied.
- T3 `BootstrapAcceptor.Accept` logs the specific reason code (`key_material_mismatch`, `key_retired_mismatch`, `key_unwrap_failed`, `key_insert_failed`, keep `key_conflict` only as fallback) without secrets.
- T4 Regression test that reproduces the retired-row case end to end through `Accept`.

## Constraints
- Security: never reactivate a retired key whose material differs; never log key material, ids of secrets beyond the existing style, or raw errors.
- Artifacts in English. Advisory ~400 changed lines per task.

## Tasks
- [x] T1 sentinel errors in ImportInWrite (+ tests) - cluster tests green; RED was compile failure on undefined sentinels
- [x] T2 reactivate retired row with identical material (+ tests) - reactivation tests green (done in same commit as T1)
- [x] T3 specific reason codes in Accept (+ log tests incl. secret leakage) - RED: 4 subtests logged key_conflict; GREEN after keyDenialReason
- [x] T4 retired-row regression through Accept - subtest "retired row with same material is accepted" in bootstrap_accept_log_test.go (passes since T2; guards the regression)

## Route
Delegated direct: one writer (cluster + enrollment packages).

## Progress / evidence
(pending)

## T5 (follow-up decision)
- [x] T5 Retired row with different or unreadable material is deleted and replaced by the incoming KEK in the same write; active mismatch stays denied; `ErrClusterKeyRetiredMismatch` / `key_retired_mismatch` removed as unreachable. Evidence: RED = 3 cluster tests failing (replace x2, atomicity); GREEN after change; atomicity test shows a blocked insert leaves the retired row intact; Accept end-to-end test accepts retired+different material with an empty log.
