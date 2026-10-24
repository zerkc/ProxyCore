# Feature: reconcile-last-applied

## Objective
Stop the worker's nginx reconcile from reverting a freshly applied configuration. A change applied by an ordinary job must stay applied.

## Problem (audit evidence, from a live primary on v0.6.14)
- After an ordinary `combined` job applies revision X, about 2 s later a `reconcile-nginx:<Y>` job re-applies a different revision Y. The cycle repeated at 04:01:36, 04:02:03, 04:02:13, 04:02:18 and 04:03:40 in the reporter's `/api/status`, and `appliedRevision` flipped to Y.
- Cause: when no job is active the worker picks the reconcile target with `latestAppliedRevision` (`apps/worker/src/poller.ts:139-157`), which returns `revisions.latest()` if it has `appliedAt`. `latest()` is `ORDER BY revision_number DESC LIMIT 1` with no filter on `applied_at` or `current_applied_revision_id` (`packages/db/src/sqlite-foundation/revision-latest-record.ts`).
- The Go API reuses a revision when the snapshot checksum matches an existing one and keeps its old `revision_number` (`apps/api/internal/configuration/sqlite_enqueue.go:29`). So re-applying an older configuration produces a revision whose number is never the highest.
- The drift probe (`apps/worker/src/apply.ts:221-236`, `apps/control/src/docker-control.ts:464-495`) compares the live nginx config with the candidate of the probed revision. Probing the wrong revision always reports `mismatched-candidate-config`, so reconcile (`apply.ts:56-77`) re-applies the wrong revision and `markApplied` makes it the current one.
- Reconcile is nginx-only, so DNS stays on the user's revision while nginx reverts.

## Decisions
- Reconcile targets the revision of the most recent successful (`applied`) job, ordered by `finishedAt` (falling back to `createdAt`), loaded with `revisions.get`. Reconcile jobs count as applied jobs, so the target is stable after a repair.
- If no applied job exists in the list, fall back to the previous `latest()` with `appliedAt` behavior.
- No change to the `RevisionStore` port, the stores, the Go side or `revision_number` semantics. The drift probe and the guards stay as they are, so genuine drift (for example the baked fallback after a container restart) still reconciles.
- Installs already stuck on the stale revision do not self-heal; the user re-applies the intended change once after updating.

## Out of scope
- A CoreDNS reconcile; refreshing `revision_number` on reuse; self-healing a desired-vs-applied mismatch.

## Constraints
- English for code, comments, tests and commits. Conventional commits, no AI attribution in commits. Test-first (`bunx vitest run` from repo root with `TZ=UTC`).

## Tasks
- [x] T1.7a Worker: `latestAppliedRevision` uses the most recent applied job; tests (revision 43 just applied while 45 is an older applied revision with a probe that reports drift only for 45: no `reconcile-nginx` job is enqueued and the probe targets 43; no applied job falls back to `latest()`; keep the existing case at `poller.test.ts:132-176`).
- [x] T1.7b Release: bump `0.6.14` to `0.6.15`.
  - T1.7a evidence: commit 2a13b99. RED: 3 of 4 new tests failed before the fix (newest-applied-job target, reconcile-job target, finishedAt/createdAt ordering); the no-applied-job fallback test passed already, as expected since it keeps the old behavior. GREEN: `TZ=UTC bunx vitest run apps/worker packages/db` 30 passed, 7 skipped; `bun run typecheck` clean.
  - T1.7b evidence: commit 356cc6b; `cd apps/api && go build ./...` OK.

## Authorized scope
apps/worker/src/poller.ts, apps/worker/src/poller.test.ts, the version files.

## Acceptance
`bun run typecheck` clean, `TZ=UTC bunx vitest run` green for apps/worker, and the new test fails before the fix.

## Progress
T1.7a and T1.7b done and committed. Deviation: the old fallback that sorted applied revisions by `appliedAt` was dropped; fallback is now `latest()` with `appliedAt`, as the Decisions state.

## Route declaration
Delegated direct (one writer).

## Delivery
Own branch `fix/reconcile-last-applied` off `main`, own PR with bump `0.6.15`, merge, tag `v0.6.15`, user tests. The Bun-to-Go migration branches rebase on it and shift their bumps by one.

## Next step
Open the PR, merge, tag `v0.6.15`, user tests.
