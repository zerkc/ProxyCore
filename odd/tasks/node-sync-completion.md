# Feature: node-sync-completion

## Objective
Close the gaps between the PRIMARY/NODE sync behavior and the product spec: NODE is read-only except update and promote, the join token is self-contained, a primary can unlink a node (and the node learns it safely), and the primary shows its nodes with their gateway IP and last report time.

## Problem (audit evidence)
- Update apply is blocked on NODE: `handleUpdateApply` uses `requireUser` (`apps/api/internal/httpserver/update_handlers.go:155`), which 403s any non-GET when not writable (`server.go:342-356`). The guard is per-handler (no middleware), so a new mutating route is writable by default. `backup/export` is also blocked though it is a read. `requireUser` runs an ingress side effect on every call including GETs (`server.go:357-362`).
- Enrollment token `pcenr1_<selector>_<secret>` carries no primary URL or CA pin; join needs `primaryUrl` + `caPem` separately (`enrollment_workflow.go:217-275`).
- No unlink: revoking only sets `revoked_at` on the token row (`sqlite_enrollment_tokens.go:157`); nothing revokes the node credential. A node sees only a generic `credential_rejected` string, no persisted state, no UI. `CanEnrollAsNode` (`identity/types.go:56-82`) blocks an unlinked NODE from re-enrolling.
- Node liveness: `last_seen_at` columns exist but are never written; no node list API/UI.
- SPKI pin on the exchange is optional (`exchange_client.go:93-112`).

## Decisions (user-approved)
- Unlink lives in a NEW primary-side Nodes view (nodes with gateway/ingress IP, last seen, unlink action). The earlier "token-bound unlink" idea is DISCARDED.
- An unlinked node keeps its last config and keeps serving; it shows a banner offering re-link or convert to PRIMARY.
- The unlink signal must be an explicit authoritative response from the primary; DB/startup errors never mark a node unlinked.
- Defaults: heartbeat on every authenticated pull/ack; "down" threshold configurable; CA embedded in the token as SPKI hash; SPKI pin mandatory on join/sync.

## Out of scope
- mTLS / per-request signature+nonce (tracked as follow-up question; pin mandatory only).
- Split-brain notification of the old primary on promote.

## Constraints
- English for code, UI copy, comments, tests. Conventional commits, no AI attribution lines in commits (user rule). One writer at a time.
- Advisory ~400 authored changed lines per task. Test-first when a runnable deterministic test exists (Go `go test -short`, UI vitest from repo root).

## Tasks
- [x] T1 NODE read-only guard: allow update apply + backup export on NODE; remove ingress side effect on GET for NODE; central allowlist middleware (or equivalent) so new mutating routes default to blocked; route tests for NODE. Commit `4cb2bde` (+155/-11). Verified: go build, go vet, `go test -short ./internal/httpserver/` green (parent re-ran). RED observed on update apply; ingress-on-GET claim had no observable RED (fixture already initialized). Route: delegated direct (writer). Tier: not assessed (RDD review pending check).
- [x] T2 Unlink backend (primary): `POST /api/topology/nodes/{id}/unlink` (owner, primary roles only, 404/409/503) revokes the node credential and sets `enrolled_nodes.unlinked_at` (migration 008); snapshot and acknowledge answer `410 {"error":"node unlinked","code":"NODE_UNLINKED"}` only when unlinked AND the real secret is presented. Re-enroll clears `unlinked_at`. Commit `6ef81bf` (26 files, +584/-21, mostly tests and schema-version bumps). Verified: build, vet, `go test -short` on sqlitedb/configuration/httpserver/backupcore/pgconvert/cmd/server green; parent re-ran configuration + sqlitedb. Surface widened with user approval (sqlitedb tests, pgconvert spec, packages/db bun scripts). Known flaky unrelated test: `TestEnrollmentIdentityProofSignsLiveIdentityAndReturnsOnlyNoStoreProof` (random base64 contains "kek"). Contracts for later tasks: store `UnlinkEnrolledNode`, `SnapshotPublicationCredentialRecord.NodeUnlinkedAt`, `httpserver.NodeUnlinkedCode`, `TopologyNodesPath`. T4 needs migration 009 and the same hardcoded ledger-count bumps. Route: delegated direct.
- [x] T3 Unlink node side: persisted `unlinked` state set ONLY on the explicit signal (never on DB/startup/network errors); allow re-enroll of an unlinked NODE (`CanEnrollAsNode`); status API exposes it. Commit `840214e` (16 files, +649/-37, mostly tests). `ErrSnapshotClientUnlinked` only for 410 with exact `code:"NODE_UNLINKED"` (bounded 1 KiB body); failure code `node_unlinked`; persisted in node-sync progress JSON (no migration); an unlinked node makes zero primary requests, also after restart (wakes every MaxBackoff only to re-read local state); `GET /api/topology/sync` adds `unlinked` + `unlinkedAt`; re-enroll via `RoleCanReEnrollAsNode(role,hasKey,unlinked)` with in-memory `Identity.NodeUnlinked` overlay set by the syncer `OnUnlinked` hook and cleared by `NodeConverter.Convert` after commit. Verified: build, vet, `go test -short` on sync/configuration/identity/enrollment/httpserver/cmd/server green; parent re-ran sync + identity. Evidence gap: no strict RED (impl before tests); mutating the 410 matcher made 9 tests fail. Surface deviation: also edited `httpserver/enrollment_workflow_confirm_test.go` (fixture, test-only). Known edge: if Convert fails after BindNodeSync the persisted flag is cleared but the overlay stays true until restart or a successful convert. Not covered: end-to-end re-enroll against a real primary. Route: delegated direct.
- [ ] T4 Heartbeat + nodes API: write `last_seen_at` on authenticated pull/ack; `GET` nodes list (id, status, ingress/gateway IP, last seen, linked-at).
- [ ] T5 UI: primary Nodes view (list, gateway IP, last seen, offline badge, unlink with confirm) + node banner (re-link / convert to PRIMARY).
- [ ] T6 Self-contained token: encode primary URL + SPKI pin + secret; join API/UI accept token only; backward-compatible decode of `pcenr1`; pin mandatory.
- [ ] T7 Cleanup: delete revoked/expired tokens (`DELETE /api/enrollment/tokens/{id}`, owner-only) + UI button.

## Authorized scope
apps/api (httpserver, identity, enrollment, configuration, sync), apps/ui (dashboard, api client), docs/runbooks, tests. No dependency additions without asking.

## Acceptance
Each task: tests observed RED then GREEN where applicable, full affected package tests green, one work-unit commit on this branch.

## Progress
Branch `feat/node-sync-completion` created from `main` (0751555). No tasks done yet.

## Route declaration
Per task: delegated direct (one writer; 2+ non-trivial files each). Exploration for T1 from the audit (26 tool uses, path:line above).

## Delivery
Strategy: ask-on-risk; running count passed ~400 after T2 (T1 +155, T2 +584). User chose chain strategy `feature-branch-chain`: slices stack on an integration branch, main receives one merge at the end. Slice boundaries and PR contents to be recorded here when PRs are prepared (push/PR/merge remain user decisions).
