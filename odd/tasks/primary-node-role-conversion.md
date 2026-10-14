# Feature: primary-node-role-conversion

## Objective
Let an owner convert an installation PRIMARY -> NODE and NODE -> PRIMARY from the UI, safely.

## Problem
- `PromoteToPrimary` exists in `apps/api/internal/identity/service.go` but has no HTTP route or UI.
- Transition matrix (`identity/service.go`, `cluster/state.go`) forbids `primary` / `primary-with-nodes` -> `node`.
- `NodeContinuityView` only renders when role is already `node`; a primary has no convert control.

## Decisions (user-confirmed unless noted)
- D1 (user): on promotion, the former primary's nodes are NOT auto-repointed; they must be re-enrolled manually against the new primary.
- D2 (default, PRD-aligned): rotate cluster key on promotion; invalidate old primary relationship and node credentials.
- D3 (default): PRIMARY -> NODE is a direct owner action behind an explicit confirmation; nodes of that primary lose their credentials first.
- D4 (default): promotion is allowed while the old primary is reachable (planned switchover) and offline (failover), always with a split-brain warning and expected-generation check.

## Scope
- apps/api: transition matrix, promote/demote workflow, HTTP routes, audit.
- apps/ui: Promote control on NODE surface, Convert-to-NODE control on PRIMARY surface.
- Tests alongside behavior.

## Tasks
- [x] T1 Backend: allow primary/primary-with-nodes -> node in both matrices; extend PromoteToPrimary into a transactional workflow (D2, D4); owner-guarded routes `POST /api/topology/promote` and `POST /api/topology/demote` with confirmation + expected generation; audit; Go tests (RED first).
- [x] T2 UI: Promote to PRIMARY control (warning dialog) on node surface; Convert to NODE entry on primary surface leading to the enrollment form; UI tests.
- [x] T3 Docs: update PRD status/decisions and README topology section.
- [x] T4 Backend: serve enrollment draft/preview/confirm/recover on the main API (:3000) behind owner session auth, reusing the existing workflow handler and its dependency wiring; the UI gets 404 today because these routes only exist on the TLS enrollment listener (:3443). Go tests (RED first).

- [ ] T5 E2E: stand up a PRIMARY and a NODE with Docker (isolated compose projects), run the real enrollment (token on primary -> draft/preview/confirm from the node) and fix the defects found. Known candidate: handleDraft/handleConfirm verify the token against the LOCAL token store (`workflow.VerifyToken = tokenAuthority.Verify`), but the token is minted by the primary -> 403 "Permission denied". Branch: fix/enrollment-routes-main-api (PR #17).

### Grant exchange (needed so enrollment can complete; ships in 0.5.8, same PR #17)
Decisions (defaults adopted from the explorer, owner may override):
- D5: eligible roles to enroll: `standalone-primary`, `primary`, and a demoted node/`stale-primary` only after explicit owner confirmation (verify role semantics first).
- D6: `--enable-node-converter` defaults ON with an explicit opt-out.
- D7: the draft cache keeps the raw token in memory only, bounded by the draft expiry; never written to disk or logs.

- [x] T6 SQLite `configuration.PrimaryGrantStore` (token consume + grant atomic; BEGIN IMMEDIATE; concurrent issue -> one grant; exact retry same payload; other binding denied). SECURITY. Done: `SQLitePhase2Store.WithPrimaryGrant` in `sqlite_primary_grant.go`; no new migration; tests in `sqlite_primary_grant_test.go` (commit pending).
- [x] T7 `NodeCredentialIssuer` + `POST /api/topology/enroll` (commit 0cfcf59; no RED observed, tests written after handler). handler `httpserver/enrollment_grant_exchange.go` (in-memory fixed-window IP/selector limiter, 429 ENROLLMENT_RATE_LIMITED), issuer `cmd/server/node_credential_issuer.go` (not in enrollment: sync tests import enrollment -> cycle), wired via `newEnrollmentWorkflowTLSSupervisorWithGrantExchange`.
- [x] T8 Node-side exchange client (same pinned TLS h2 transport) + atomic KEK/credential persistence, zeroing of secrets. SECURITY. Delivered (uncommitted; parent commits): `sync.ExchangeClient`, `cluster.SQLiteKeyStore.ImportInWrite`, `enrollment.BootstrapAcceptor` (Accept / LoadNodeCredentialSecret); no migration (KEK in cluster_keys, credential in secrets with id = credential id).
- [x] T9 `handleConfirm` orchestration: ephemeral key -> exchange -> OpenBootstrap -> persist -> strict validate -> Convert; failure leaves a recoverable standalone node. Done (uncommitted; parent commits): draft cache retains token + per-draft recipient key in memory (`PutWithPrimaryURLAndToken`, `AttemptSecrets`, zeroed on delete/expiry/eviction); attemptId = draft id, previewDigest = SHA-256(domain, contentHash, primaryURL, nodeId, installationId); confirm maps denied/invalid 409 (token dropped), rate limited 429 + Retry-After, unavailable/persistence 503; strict validation via `ValidateWithKEK` with the acceptor KEK; wiring in `cmd/server/main.go` (`wireEnrollmentGrantExchange`, no SPKI pin yet). RED observed as compile failure.
- [x] T10 Converter flag default + role eligibility (D5, D6), tests, then real Docker E2E (draft->preview->confirm->node applies, demote->re-enroll, promote). Done (uncommitted; parent commits): `--enable-node-converter` ON by default (`=false` opts out, last wins, unparsable fails closed); converter built regardless of boot role; eligibility is per-request in `handleConfirm` before any exchange via `identity.RoleCanEnrollAsNode` (standalone/primary/stale-primary, or node WITHOUT cluster key = demoted/unbound; refused: bound node, primary-with-nodes) plus defence in depth in `Convert`; commit point is `identity.Service.CommitNodeEnrollment` (role, imported cluster key id, adopt primary generation); `CreateApplyJobFromSnapshot` reuses the latest non-failed import job on retry (unique checksum). Real Docker E2E passed (enroll, retry reuses grant/job, promote, demote->re-enroll without restart). Open: re-enrolling a node whose revoked credential/enrolled_nodes row exists on the target primary is denied (unique node_credentials.node_id / enrolled_nodes.node_id) - needs a design decision.

### Switchover / failback cycle (owner scenario)
Scenario: A and B are primaries. B -> NODE of A. A goes down. B -> PRIMARY and is edited. A returns, A -> NODE of B. After sync, A -> PRIMARY and B -> NODE of A.
Verified root cause of the failing last step: `node_credentials_node_idx UNIQUE(node_id)` and `enrolled_nodes.node_id PRIMARY KEY`; `CreateNodeCredential`/`CreateEnrolledNode` are plain INSERTs and demote/promote only set `revoked_at`, so B's second enrollment against A hits A's old row -> uniform 401.
- D8 (default): re-enrollment rotates the existing row in place when it is revoked and belongs to the same installation; a live (unrevoked) row, or a different installation, is still denied. No new table, no migration; history stays in audit events.
- [~] T11 Rotate-in-place re-enrollment (RED first) + real Docker E2E of the full A/B cycle, including data edited on B visible on A after failback. Report (not implement) whether promote guards against an unsynced node.
  - T11 progress (uncommitted): rotate-in-place implemented in `configuration/sqlite_primary_grant.go` (revoked row of the same installation is re-keyed atomically; live row or different installation denied; retry cannot rotate twice because the token is consumed in the same transaction) with two new grant tests (RED observed). Real Docker E2E of the A/B cycle: steps 1-3 and the step-4 re-enrollment of B to A passed with the fix. Open defects needing a design decision: (1) a node never materializes the imported snapshot into its configuration tables, so promote + apply publishes the pre-enrollment local config and silently drops the former primary's edits; (2) promote + apply with unchanged tables yields no snapshot at the new generation (likely unique `applied_snapshots.content_hash`), so nodes get "primary snapshot unavailable" until the config changes; (3) promote has no guard for an unsynced node and the returning former primary stays writable (not stale) until it is demoted by hand.

### Node as a near-exact copy of the primary, continuous sync (ships as 0.6.0, single PR #18)
Owner model: a NODE is a near-exact copy of the PRIMARY. Exceptions: (a) its own ingress IP, (b) for a proxied domain the DNS answer is the IP of the installation that answers (A answers A's IP, B answers B's IP; nginx proxies through that installation). Everything else lives in the NODE's own editable tables so a promote needs no backup. Each configuration change has a hash so a node knows whether it is outdated relative to the primary.
Decisions: D9 (user) all of this is one feature, one PR (#18), release 0.6.0; no splitting into 0.5.x. D10 (default, "near-exact copy") the node also replicates owners and secrets so the same credentials work after a promote. D11 (default) the node's "applied hash" is the primary's `contentHash` exactly as received; do not recompute it after node-local overrides.
Verified findings (explorer, path:line):
- Import (`snapshot/import.go` ~:100, `configuration/sqlite_phase2_store.go:417` `CreateApplyJobFromSnapshot`) writes only `config_revisions`, `apply_jobs`, `standalone_archives`; editable tables (`zones`, `dns_records`, streams, certificates, settings) are never written; owners and secrets are never written (`snapshot/validate.go:91,128` only reference them). Inverse of `sqliteAssembleSnapshot` (`configuration/sqlite_snapshot.go:79`) is the materializer; reuse write helpers `sqlite_record_write.go`, `sqlite_settings.go`.
- Worker renders from `replicated.configuration.settings.ingress` (`apps/worker/src/apply.ts:266-276`, `render.ts:63`, `packages/renderers/src/coredns.ts:100-110`); `nodeLocal.ingress` is never read, so a node answers the PRIMARY's IP for proxied domains. Node-local overlay is only in the in-memory draft cache (`enrollment/draft_cache.go:27`, `main.go:156`, `enrollment_workflow.go:529-541`).
- `contentHash = sha256(StableStringify(ReplicatedFields))` over configuration+secrets+owners (`snapshot/schema.go` ~:136); NodeLocal/Transient excluded; generation not included. Import `config_revisions.checksum` = sha256 of whole envelope (`configuration/apply_wait.go:179`), separate from `snapshot_content_hash` (`sqlite_phase2_store.go:461-468`). Unique indexes: `config_revisions.checksum` (`004_operational_product.sql:43`), `applied_snapshots_content_hash_idx` (`005_continuity_product.sql:136`) -> reverting to an old configuration collides.
- Primary side exists: `GET /api/topology/sync/snapshot` with `AfterContentHash` short-circuit (`httpserver/enrollment_publication.go:16`, `configuration/sqlite_snapshot_publication.go:253`), ack handler `/api/topology/sync/acknowledge` (`enrollment_acknowledgment.go:24`, `node_snapshot_acks`). Node side: `sync/snapshot_client.go:99` `FetchSnapshot` is only used by the enrollment workflow (`cmd/server/main.go:456-463`); no scheduler, backoff, ack client or status (spec: `openspec/changes/primary-node-continuity-phase-2/tasks.md` section 6, PRD phase 2/3).
Tasks (sequential, one writer each, one commit each):
- [~] T12 (implemented, uncommitted; see T12 progress) Materialize an imported snapshot into the node's editable tables (settings, zones, records, streams, certificates metadata) in the same transaction as the import revision; node's own ingress overrides `settings.ingress` both in the tables and in the revision the worker renders, so proxied DNS answers the node's own IP. RED first.
  - T12 progress: materializer `configuration/sqlite_import_materialize.go` runs inside `CreateApplyJobFromSnapshot`'s writer transaction (tables + revision atomic; replace, not merge). Findings: the importer's "standalone archive" stored the imported envelope, NOT the node's pre-enrollment configuration, so the materializer now archives the replaced local configuration (reason `pre-import-configuration`, 30d TTL) in the same transaction. Ingress: tables get `nodeLocal.ingress`; the worker substitutes `nodeLocal.ingress` over `settings.ingress` at render (`apps/worker/src/apply.ts`), the stored envelope stays byte-identical so primary `contentHash`/checksum verify (D11). Importer now verifies, never recomputes, the hash. T13 seam: import secrets/owners in the same transaction BEFORE certificate rows so `key_secret_id` resolves (currently NULLed when the secret is absent).
- [ ] T13 Import secrets (unwrap with the cluster KEK, re-wrap locally) and owners into the node. SECURITY.
- [ ] T14 Sync loop on the node: scheduler, bounded backoff, `AfterContentHash` conditional pull, ack client, manual retry endpoint.
- [ ] T15 Applied/primary hash exposure and status: node applied hash, last seen primary hash, outdated flag (`GET /api/topology/sync`), promote returns a warning when the node is outdated.
- [ ] T16 Revert-to-old-configuration must not collide on the unique checksum/content_hash indexes.
- [ ] T17 UI: sync status / outdated indicator on the node surface; README + PRD updates (flag default, roles, sync model, URL format).
- [ ] T18 Full Docker E2E: A/B cycle with data intact after failback, proxied DNS answers per node, edit on primary reaches node, outdated detection, promote has the config ready.

## Acceptance
- Promote NODE -> PRIMARY works offline and online, bumps generation, old nodes must re-enroll.
- Demote PRIMARY -> NODE leads into the enrollment preview/confirm flow.
- Writes on former primary are blocked after promotion elsewhere.

## Routes
- T1: delegated (2+ non-trivial files, mapping done by explorer).

## Progress
- Branch: feat/primary-node-role-conversion
- T1 done: commit ce6c78f (go build/vet/test green; RED observed for cluster and identity SQLite only).
- T2 done (commit 28270ae): TopologyConversionPanel + promoteTopology/demoteTopology client; 13 new vitest tests; EnrollmentTrustPanel date test fails on base (pre-existing).
- T3 done: README + PRD status updated.
- Reopened: T4 (user report: POST /api/topology/enrollment/draft -> 404 on :3000; pre-existing wiring gap, confirmed by user choice 1 = mount routes on main API).
- T5 (partial, commit c8b7e96): real Docker E2E (pcprimary + pcnode). Fixed: (1) token no longer verified against the node local store on the owner-session main API; (2) FetchSnapshot argument order swapped (url/token); (3) dedicated TLS 1.3 listener now advertises ALPN h2 (snapshot client is h2-only); (4) draft/preview/recover validate without local KEK (confirm still fails closed); (5) PRIMARY mints its cluster KEK before first token activation. E2E: draft 200, preview 200, promote 200, demote 200. BLOCKED: confirm returns 409 "primary snapshot denied" because the grant exchange (PrimaryService.IssueGrant HTTP endpoint, SQLite PrimaryGrantStore, node-side BootstrapRecipient/OpenBootstrap, KEK/credential persistence, token consumption) is not wired anywhere; the converter also requires --enable-node-converter. Needs a design/scope decision.
