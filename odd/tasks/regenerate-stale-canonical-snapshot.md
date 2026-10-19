# Regenerate stale canonical snapshot on upgrade

## Objective
A primary restored from a backup before 0.6.8 stores canonical snapshots (`applied_snapshots.snapshot_body`) containing the backup master-key marker secret (nil UUID). 0.6.8 nodes reject them (`validate: snapshot_denied`, SECRET_ID). Upgrading the primary must heal this automatically, with no operator action.

## Evidence
Primary DB: the 4 latest `applied` snapshots have `has_nil_id=1`. `Produce` never regenerates an existing snapshot; `ListCanonicalSnapshotCandidates` skips jobs that already have an `applied` row.

## Tasks
- [x] T1 RED e2e: primary with a stored legacy snapshot containing the marker -> start/reconcile -> node enroll succeeds (fails today).
- [x] T2 Heal: during reconcile, any non-discarded latest snapshot failing `canonicalPublicationIsValid` (or containing a nil-id secret) is discarded and regenerated from its job.
- [x] T3 Unit tests for the heal path and for a healthy snapshot staying untouched.
- [ ] T4 Release PR 0.6.9 (after merge, together with #31).

## Acceptance
- Upgrade alone fixes the primary; healthy snapshots are not regenerated; no secret in logs.
- Checks: `cd apps/api && go build ./... && go vet ./... && go test ./...`.

## Progress
Route: delegated writer (2+ non-trivial files).

Heal: producer `Reconcile` first discards (discarded_at) the 8 newest live snapshots of the current identity/generation that fail `canonicalPublicationIsValid`; `Produce` ignores discarded rows; candidates query ignores discarded rows in its content_hash `not exists`; Publish rebinds a discarded same-hash row. Verified: go build/vet and tests for sync, configuration, snapshot, enrollment, httpserver pass. Uncommitted.
