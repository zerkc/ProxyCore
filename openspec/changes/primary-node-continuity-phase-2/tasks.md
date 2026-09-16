# Tasks: Primary/Node continuity — Phase 2

This plan implements only the Phase 2 enrollment, node-pull synchronization, read-only boundary, and minimal continuity UI defined by `proposal.md`, `design.md`, and the six change-local specs. It preserves the Phase 0+1 snapshot semantics and does not add promotion, rejoin, inventory, dashboards, election, database replication, SQLite, or Phase 5 hardening.

## Review Workload Forecast

| Field | Value |
|-------|-------|
| Estimated changed lines | Approximately 2,600–3,200 authored lines across production code, tests, UI, and runbook updates; generated migration text is excluded from the authored estimate but remains part of snapshot/review identity |
| 400-line budget risk | High |
| Chained PRs recommended | Yes |
| Suggested split | PR 1 persistence → PR 2 security primitives → PR 3 primary enrollment authority → PR 4 snapshot pull/apply/ack → PR 5 node enrollment commit → PR 6 scheduler/startup → PR 7 mutation policy → PR 8 UI and runbooks → PR 9 end-to-end/release gates |
| Delivery strategy | ask-on-risk |
| Chain strategy | pending |

Decision needed before apply: Yes
Chained PRs recommended: Yes
Chain strategy: pending
400-line budget risk: High

The native session setting `ask-on-risk` governs this plan; the historical `exception-ok` value in `openspec/config.yaml` does not grant a size exception. Each work unit below is intended to be one independently reviewable commit/PR slice. Do not begin apply until the parent resolves the delivery decision and selects a chain strategy or explicitly authorizes another supported path.

## Global execution rules

- RED evidence must show the new focused test failing against the pre-change implementation before the behavior is written.
- GREEN evidence must show the smallest implementation satisfying the RED contract.
- TRIANGULATE evidence must exercise an independent boundary such as PostgreSQL concurrency, a fake HTTPS peer, restart/crash injection, a property/vector, a direct service call, or a worker integration—not only the happy-path unit test.
- REFACTOR evidence must preserve behavior, remove duplication, keep tests adjacent to the behavior, and rerun the focused commands.
- Record exact commands, results, redacted failure codes, runtime scenario results, and changed-line counts in the apply/verify receipts. Never record token, credential, KEK, private-key, password-hash, snapshot-body, or plaintext-secret values.

## Phase 2 work units

### 1. Additive persistence and cross-process contracts

**Start:** Phase 0+1 schema exists with `installation_identity`, `cluster_keys`, `node_state`, `applied_snapshots`, `config_revisions`, and `apply_jobs`; `node_state.enrollment_token_hash` is legacy/inert.

**Finish:** Go `EnsureSchema`, Drizzle schema, migration, row mapping, and store ports describe the same additive Phase 2 persistence model without changing fresh standalone behavior.

- [x] **RED** Add schema-parity and migration tests for `enrollment_tokens`, `enrolled_nodes`, `node_credentials`, `enrollment_grants`, `node_snapshot_acks`, `enrollment_attempts`, `sync_attempts`, and `standalone_archives`; cover checked states, unique/foreign-key constraints, the one-active-attempt index, `node_state` additions, source-attribution fields, and pending/terminal applied-snapshot state in `apps/api/internal/configuration/phase2_schema_test.go` and `packages/db/src/phase2-schema.test.ts`. <!-- sdd-owner: implementation -->
- [x] **GREEN** Extend `apps/api/internal/configuration/schema.go`, `packages/db/src/schema.ts`, and the next migration `packages/db/migrations/0004_primary_node_continuity_phase_2.sql` with matching UTC timestamp, UUID, enum/check, index, and foreign-key definitions; leave `node_state.enrollment_token_hash` readable but unused. <!-- sdd-owner: implementation -->
- [x] **GREEN** Extend `apps/api/internal/configuration/rows.go`, `apps/api/internal/configuration/store.go`, `packages/db/src/ports.ts`, and `packages/db/src/persistence.ts` with typed records and transaction ports for `node_state.enrollment_attempt_id`, `primary_url`, `primary_installation_id`, `primary_tls_spki_sha256`, `credential_id`, `sync_enabled`, attempt timestamps/failure counters, source attribution (`ordinary`/`import`/`sync`), linked apply terminal results, snapshot identity, and acknowledgement outbox metadata. <!-- sdd-owner: implementation -->
- [x] **TRIANGULATE** Run the migration and Go schema setup twice against isolated PostgreSQL schemas, query all constraints/indexes, and prove old standalone rows remain readable while concurrent inserts cannot create duplicate node bindings or active attempts. <!-- sdd-owner: implementation -->
- [x] **REFACTOR** Centralize Phase 2 persistence constants/row decoding so Go SQL, Drizzle names, nullable handling, and status values cannot drift; rerun the parity tests after formatting and cleanup. <!-- sdd-owner: implementation -->
- [x] **Focused verification:** Run `cd apps/api && go test ./internal/configuration ./internal/identity` and `bun run test -- packages/db/src`; record migration parity and idempotency results. <!-- sdd-owner: implementation -->
- [x] **Runtime harness:** Use disposable PostgreSQL with `bun run db:migrate`, then invoke `configuration.EnsureSchema` twice and run the isolated transaction/concurrency tests; if PostgreSQL is unavailable, record the harness as blocked rather than claiming success. <!-- sdd-owner: implementation -->
- [x] **Rollback boundary:** Revert only the new schema readers, writers, ports, and migration generator inputs; retain additive Phase 2 tables/columns and all referenced data, with no destructive down-migration. <!-- sdd-owner: implementation -->
- [ ] **Reviewable commit outcome:** Produce `feat(db): add phase 2 continuity persistence contracts` with schema, store, and adjacent tests together. <!-- sdd-owner: implementation -->

### 2. Enrollment security primitives, identity proof, and redaction

**Start:** Existing Go/TypeScript `v1.kek` and local master-key secret helpers exist, but no enrollment token, sealed bootstrap, TLS identity proof, or Phase 2 redaction contract exists.

**Finish:** Security-sensitive values have typed, bounded, testable primitives and cannot enter ordinary serialization, logs, metrics labels, audit values, URLs, or error details.

- [ ] **RED** Add failing unit/property tests beside `apps/api/internal/enrollment/` and `apps/api/internal/cluster/` for `pcenr1_<selector>_<secret>` generation, at-least-256-bit secret entropy, hash-versioned domain separation, constant-time comparison, TTL bounds, X25519/HKDF-SHA-256/AES-256-GCM sealed bootstrap AAD, tamper/wrong-key/wrong-binding rejection, and zero secret serialization. <!-- sdd-owner: implementation -->
- [ ] **RED** Add failing identity/TLS tests for canonical HTTPS URL enforcement, redirect rejection, hostname validation, SPKI/CA fingerprint capture, nonce and attempt binding, signature expiry, changed fingerprint, primary-ID mismatch, and leadership-generation regression; add nested-body/header canaries for `apps/api/internal/secrets/secrets.go` redaction. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Implement the new `apps/api/internal/enrollment/token.go`, `identity_proof.go`, and `bootstrap.go` contracts plus `apps/api/internal/cluster/bootstrap.go`; use CSPRNG material, explicit versions, authenticated associated data, bounded expiry, and generic denial errors. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Extend the shared redaction boundary in `apps/api/internal/secrets/secrets.go` and its tests to recursively redact nested structures and HTTP headers before audit/logger/metrics calls; keep existing `v1.kek` and local-master-key envelope formats unchanged. <!-- sdd-owner: implementation -->
- [ ] **TRIANGULATE** Compare Go bootstrap/identity-proof vectors with the existing `packages/crypto/src/cluster.ts` vectors, inject clocks/nonces where appropriate, and prove malformed, replay-bound, and tampered payloads fail without returning sensitive distinctions. <!-- sdd-owner: implementation -->
- [ ] **REFACTOR** Separate pure encoding/verification from transport and persistence adapters, make all error paths stable and generic, and rerun the canary-redaction snapshots after removing debug material. <!-- sdd-owner: implementation -->
- [ ] **Focused verification:** Run `cd apps/api && go test ./internal/enrollment ./internal/cluster ./internal/secrets` and `bun run test -- packages/crypto/src`; record vector, entropy, binding, and redaction results without sensitive values. <!-- sdd-owner: implementation -->
- [ ] **Runtime harness:** N/A for the pure cryptographic primitives; the fake HTTPS identity-peer harness is introduced and exercised in Work Unit 3, while this unit must still pass all in-process vectors. <!-- sdd-owner: implementation -->
- [ ] **Rollback boundary:** Revert only new enrollment/bootstrap/proof helpers and redaction call-site changes; preserve existing Phase 0+1 KEK/local-secret formats and retain encrypted rows as inert data. <!-- sdd-owner: implementation -->
- [ ] **Reviewable commit outcome:** Produce `feat(security): add bounded enrollment and sealed bootstrap primitives` with primitive code, vectors, and redaction tests together. <!-- sdd-owner: implementation -->

### 3. Primary enrollment authority and credential authentication

**Start:** `apps/api/internal/httpserver/server.go` exposes only session/cookie authentication and ordinary configuration routes; the primary has no enrollment-token, node-credential, snapshot, or revocation endpoints.

**Finish:** Owner-only primary operations issue/revoke one-time authorization and per-node credentials, while enrollment-token and node-credential principals are isolated and every pull/ack checks current revocation state.

- [ ] **RED** Add HTTP and PostgreSQL integration tests for Owner-only `POST /api/topology/enrollment-tokens`, bounded expiry, one-time plaintext response, hash-only persistence, non-owner denial, idempotent token revoke, generic expired/revoked/replayed `401 ENROLLMENT_DENIED`, request-size/rate-limit behavior, `Cache-Control: no-store`, and absence of token material from audit/log/status responses. <!-- sdd-owner: implementation -->
- [ ] **RED** Add concurrent exchange tests proving `POST /api/topology/enroll` consumes one token once, binds one attempt/installation/node/public key, issues one stored hash-only credential, stores one sealed grant, returns the same sealed grant for an exact retry, and rejects a different binding without disclosing the reason. <!-- sdd-owner: implementation -->
- [ ] **RED** Add privilege and revocation tests proving an Owner session cannot use node routes, an enrollment token cannot pull/ack/mutate, a node credential cannot use Owner/configuration routes, and `POST /api/topology/nodes/{nodeId}/revoke` immediately denies subsequent pull and ack requests without deleting node data. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Implement primary services/stores in `apps/api/internal/enrollment/primary.go` and `apps/api/internal/sync/credentials.go`, using row locks and database uniqueness for atomic token consumption, idempotent sealed grants, node binding, credential hashing, current revocation checks, redacted stable event codes, and `primary → primary-with-nodes` lifecycle when the first node is enrolled. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Add handlers/middleware in `apps/api/internal/httpserver/topology_handlers.go`, `snapshot_handlers.go`, `server.go`, and focused tests for token creation/revoke, identity preview, enrollment exchange, node revoke, generic errors/cache headers, and the distinct Enrollment and node-bearer authentication schemes. <!-- sdd-owner: implementation -->
- [ ] **TRIANGULATE** Exercise the real Go API against fake TLS primary/node peers with concurrent exchange requests and a revocation committed by a second request; verify stream authorization happens only after current database checks and no response/header/cache/log contains a secret. <!-- sdd-owner: implementation -->
- [ ] **REFACTOR** Consolidate principal parsing, generic error mapping, rate limiting, and redacted audit metadata so every endpoint uses the same scope boundary and cannot accidentally accept cookies or query-string credentials. <!-- sdd-owner: implementation -->
- [ ] **Focused verification:** Run `cd apps/api && go test ./internal/enrollment ./internal/sync ./internal/httpserver`; include the PostgreSQL concurrency, endpoint contract, scope, revocation, and redaction results. <!-- sdd-owner: implementation -->
- [ ] **Runtime harness:** Run fake HTTPS primary/node servers with real `httptest` API handlers; verify TLS pinning, request limits, `201/204/401/202` contracts, and revocation without process restart. <!-- sdd-owner: implementation -->
- [ ] **Rollback boundary:** Disable the new topology token/exchange/revoke routes and scheduler entry points as one unit; do not remove credential, grant, or revocation rows or reinterpret a committed node as standalone. <!-- sdd-owner: implementation -->
- [ ] **Reviewable commit outcome:** Produce `feat(enrollment): add primary token exchange and node credential authority` with services, handlers, and integration tests in one reviewable slice. <!-- sdd-owner: implementation -->

### 4. Authenticated complete-snapshot publication, pull, apply, and acknowledgement

**Start:** Phase 1 has `snapshot.Exporter`, `Validator`, `Importer`, and the existing Go-to-TypeScript worker apply path, but no network publication, bounded download, source-aware sync import, or post-apply ack.

**Finish:** A committed node can fetch only the latest complete immutable snapshot, apply it through the existing canonical worker pipeline, retain the prior known-good state on failure, and acknowledge only an exact successful application.

- [ ] **RED** Add publisher/client contract tests for `GET /api/topology/snapshots/latest?after=...`: node-credential-only access, `204` when current, `200` latest complete immutable bytes when behind, quoted content-hash `ETag`/metadata, no ranged-resume behavior, bounded response size, and partial/oversized download discard. <!-- sdd-owner: implementation -->
- [ ] **RED** Add sync coordinator tests for hash/length/schema/version/source-primary/leadership checks, latest-only convergence (for example revision 41 directly to 47), single-flight locking, source attribution, and no acknowledgement before a linked worker job is terminal `applied`. <!-- sdd-owner: implementation -->
- [ ] **RED** Add failure tests for integrity, compatibility, secret, CoreDNS, Nginx, promotion, reload, health, database-commit, and ack failures; prove the previous active snapshot remains serviceable and failed candidates do not advance applied or acknowledgement pointers. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Implement `apps/api/internal/snapshot/publisher.go`, `apps/api/internal/sync/client.go`, `coordinator.go`, and `ack.go`; write attempt-scoped temporary files with restrictive permissions, fsync/close before verification, invoke the existing validator/apply ports without archiving or changing role for ordinary sync, and persist a durable pending-ack tuple. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Extend `apps/api/internal/configuration/store.go`, `rows.go`, `packages/db/src/ports.ts`, and `packages/db/src/persistence.ts` to create `source='sync'` revisions/jobs with primary/node/revision/hash/version/generation attribution and to query exact linked worker terminal outcomes. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Make only the minimum `apps/worker/src/apply.ts`/persistence-adapter change required to finalize linked snapshot status; keep rendering, control-socket operations, and the canonical Phase 0+1 validation/apply sequence unchanged. <!-- sdd-owner: implementation -->
- [ ] **TRIANGULATE** Run a two-installation harness with fake TLS transport and the real worker apply path: skip multiple revisions, interrupt a download, reject a secret or renderer candidate, fail after promotion, restart before ack, and verify exact retry/ack behavior and prior-known-good service continuity. <!-- sdd-owner: implementation -->
- [ ] **REFACTOR** Isolate transport, file lifecycle, validation/import, apply-result proof, and ack-outbox responsibilities; remove any snapshot body or secret from logs/audit/ordinary response serialization and rerun the complete focused suite. <!-- sdd-owner: implementation -->
- [ ] **Focused verification:** Run `cd apps/api && go test ./internal/snapshot ./internal/sync ./internal/configuration` and `bun run test -- apps/worker/src packages/db/src`; record latest-only, failure-continuity, source-attribution, and ack ordering results. <!-- sdd-owner: implementation -->
- [ ] **Runtime harness:** Use two real API instances or API handlers plus fake TLS peers and the existing TypeScript worker/control fixtures; verify CoreDNS/Nginx candidate behavior with node-local ingress and no primary dependency after activation. <!-- sdd-owner: implementation -->
- [ ] **Rollback boundary:** Disable network sync publication/import and pending-ack processing together; retain active/previous-known-good snapshots, durable jobs, and attribution rows, and never delete or directly edit active data-plane files during rollback. <!-- sdd-owner: implementation -->
- [ ] **Reviewable commit outcome:** Produce `feat(sync): apply authenticated complete snapshots with exact acknowledgements` with transport, persistence attribution, worker adapter, and tests together. <!-- sdd-owner: implementation -->

### 5. Recoverable node enrollment, archive, local secrets, and safe commit

**Start:** Local import can archive/enqueue but startup wires `snapshot.NoopArchiveStore`; identity transitions are not atomic with enrollment prerequisites; there is no durable enrollment attempt state or emergency Owner preservation.

**Finish:** The node moves through durable staged states and commits `NODE` only after identity confirmation, archive, local KEK/credential protection, and successful initial apply; interruption leaves either recoverable standalone state or one valid committed node.

- [ ] **RED** Add state-machine tests for `DRAFT → VERIFIED → CONFIRMED → EXCHANGED → ARCHIVED → INITIAL_APPLY_PENDING → COMMITTED`, cancellation, `FAILED`, `RECOVERABLE`, illegal edges, one active attempt, repeated recover/cancel, and crash injection after every durable step. <!-- sdd-owner: implementation -->
- [ ] **RED** Add safe-commit predicate tests requiring matching terminal worker `applied`, `config_revision.applied_at`, snapshot version/revision/hash/generation, archive, protected KEK, stored credential, and confirmed preview digest before the durable role transaction. <!-- sdd-owner: implementation -->
- [ ] **RED** Add tests proving the archive is encrypted/durable, retention starts at successful commit and remains exactly 30 days across recovery, failed/cancelled attempts preserve standalone editability, and emergency local Owner credentials survive replicated Owner-hash application unchanged. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Implement the node coordinator/state machine in `apps/api/internal/enrollment/node.go` and `state.go`; persist token-free attempts, hold request-only token material in memory, resume exact staged work idempotently, and expose authenticated-user `GET /api/topology/enrollment` plus Owner-only preview, confirm, recover, and pre-commit cancel contracts. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Add PostgreSQL `ArchiveStore` and retention integration in `apps/api/internal/snapshot/retention.go` and `apps/api/internal/configuration/archive_store.go`; archive the pre-enrollment desired snapshot with encrypted storage and defer the 30-day expiry clock until successful commit. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Extend `apps/api/internal/identity/service.go` and `store.go` with an atomic enrollment commit/load operation that updates role, primary lineage, generation, `node_state`, applied snapshot pointer, and attempt state together; generate/retain the node-local emergency Owner credential and wrap the node credential, ephemeral private key, and cluster KEK through the local master-key secret boundary. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Integrate the Work Unit 4 initial sync trigger so `INITIAL_APPLY_PENDING` commits only after the exact worker/apply proof, then sends the exact ack; on failure, preserve/recover the standalone desired and active state through the canonical apply path rather than direct file edits. <!-- sdd-owner: implementation -->
- [ ] **TRIANGULATE** Run crash/restart recovery at each durable boundary, including response loss after primary exchange and filesystem-applied/database-uncommitted state; prove one credential/grant, one node identity, no premature mutation guard, no ack on failure, and no repeated destructive replacement. <!-- sdd-owner: implementation -->
- [ ] **REFACTOR** Keep state transitions compare-and-set/row-lock protected, centralize recovery action selection, zero/discard plaintext buffers where practical, and make archive/secret cleanup conditional on confirmed standalone serviceability. <!-- sdd-owner: implementation -->
- [ ] **Focused verification:** Run `cd apps/api && go test ./internal/enrollment ./internal/identity ./internal/snapshot ./internal/configuration`; include state-table, crash-recovery, archive-retention, safe-commit, emergency-Owner, and initial-apply-gate results. <!-- sdd-owner: implementation -->
- [ ] **Runtime harness:** Run a two-installation enrollment scenario with a fake TLS primary, disposable PostgreSQL, local master key, and worker apply fixture; interrupt and restart the node at every staged boundary and inspect only redacted state. <!-- sdd-owner: implementation -->
- [ ] **Rollback boundary:** Stop new enrollment triggers before reverting coordinator code; leave standalone attempts recoverable, committed nodes durably `NODE`, archives/KEK/credentials intact, and never auto-restore an archive or lower leadership generation. <!-- sdd-owner: implementation -->
- [ ] **Reviewable commit outcome:** Produce `feat(enrollment): commit recoverable node conversion after initial apply` with state machine, archive store, identity transaction, secret wrapping, and tests together. <!-- sdd-owner: implementation -->

### 6. Periodic synchronization, bounded backoff, manual retry, and lifecycle wiring

**Start:** The API has only the certificate renewal loop and the retention no-op wiring; configuration has no sync timing fields and startup fatally rejects any persisted role other than standalone.

**Finish:** Only committed nodes schedule one restart-safe pull path, recover pending work, coalesce manual retries, back off within configured limits, continue serving locally while the primary is unavailable, and never run primary-owned ACME renewal.

- [ ] **RED** Add clock/RNG-injected backoff tests for configured normal interval, minimum/maximum bounds, jitter range, exponential failure growth, success reset, `401` stable revoked state, restart scheduling, manual cooldown, and no unbounded trigger queue. <!-- sdd-owner: implementation -->
- [ ] **RED** Add lifecycle tests for periodic/manual/enrollment/restart trigger coalescing under one advisory lock, pending-ack retry, disabled synchronization, primary transport loss, persisted `next_attempt_at`, and continued API/DNS/Nginx/worker responsiveness. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Implement `apps/api/internal/sync/scheduler.go`, `backoff.go`, and persisted attempt/status updates; add Owner-only `POST /api/topology/sync/retry` plus authenticated-user `GET /api/topology/sync` with redacted status, `202 scheduled|already-running`, and bounded cooldown. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Extend `apps/api/internal/config/config.go` with validated `normalInterval`, `minBackoff`, and `maxBackoff` settings, then wire committed-node-only scheduling, startup reconciliation, pending-ack retry, archive retention, and graceful shutdown in `apps/api/cmd/server/main.go`. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Load persisted identity before write/renewal decisions, accept valid persisted non-standalone roles, fail closed when compatibility enforcement is unavailable, replace `snapshot.NoopArchiveStore` with PostgreSQL storage, and start ACME renewal only for writable primary roles. <!-- sdd-owner: implementation -->
- [ ] **TRIANGULATE** Stop the fake primary, restart during backoff and during an active attempt, issue manual retry while periodic work runs, revoke the credential, and verify local DNS/proxy/certificate service remains active with no ACME invocation or ordinary mutation replay. <!-- sdd-owner: implementation -->
- [ ] **REFACTOR** Make scheduling decisions pure and persistence-backed, keep backoff independent from request handling, consolidate redacted sync status/result codes, and rerun scheduler plus startup/renewal tests. <!-- sdd-owner: implementation -->
- [ ] **Focused verification:** Run `cd apps/api && go test ./internal/sync ./internal/config ./internal/configuration ./cmd/server`; record backoff bounds, restart, coalescing, revocation, primary-unavailable continuity, and renewal-gating results. <!-- sdd-owner: implementation -->
- [ ] **Runtime harness:** Run the API with disposable PostgreSQL and fake primary/worker processes, including restart and primary outage windows; verify `/api/topology/sync` stays responsive and the local data plane remains available. <!-- sdd-owner: implementation -->
- [ ] **Rollback boundary:** Disable periodic/manual sync scheduling first, then revert scheduler wiring; keep schema, credentials, archives, active snapshots, and committed `NODE` identity intact, and require a Phase 2-capable binary for recovery. <!-- sdd-owner: implementation -->
- [ ] **Reviewable commit outcome:** Produce `feat(sync): add bounded restart-safe node scheduling` with configuration, scheduler, startup/renewal gating, and tests together. <!-- sdd-owner: implementation -->

### 7. Central categorized NODE mutation policy

**Start:** Ordinary handlers call `requireUser`, configuration services mutate directly, and only the identity service exposes a coarse writable flag; UI visibility is the only prospective node read-only boundary.

**Finish:** A durable committed `NODE` rejects every ordinary replicated mutation at both HTTP and service boundaries, permits only allow-listed node-local settings and an unforgeable synchronized apply capability, and leaves pre-commit standalone behavior unchanged.

- [ ] **RED** Add a mutation matrix test covering every mutating route registered in `apps/api/internal/httpserver/server.go`—settings, apply, users, zones/records, streams, certificates, renewal, providers, and future unclassified routes—and direct/internal service callers for standalone, pending/recoverable, committed `NODE`, and stale-primary roles. <!-- sdd-owner: implementation -->
- [ ] **RED** Add tests proving `ReplicatedMutation` is denied, `NodeLocalMutation` allows only local ingress and Phase 2 sync settings, `SnapshotApply` cannot be forged from request input, `TopologyMutation` is Owner-only, and unclassified operations fail closed. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Implement `apps/api/internal/policy/` with categorized mutation decisions, durable identity lookup, and an internal capability type that only validated enrollment/sync orchestration can create; inject it into handlers plus configuration/auth/certificate write methods. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Update `apps/api/internal/httpserver/server.go`, `config_handlers.go`, `user_handlers.go`, `certificate_handlers.go`, and all route-specific mutation handlers to enforce the central policy, and remove incidental ingress initialization from `requireUser` unless explicitly classified as node-local. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Gate `apps/api/internal/configuration/renewal.go` and related service writes so Workers can apply a validated synchronized snapshot while ordinary callers cannot use the same path to mutate replicated state. <!-- sdd-owner: implementation -->
- [ ] **TRIANGULATE** Probe every mutating endpoint directly against a committed-node API and call representative services without HTTP; verify desired/active state is unchanged, local ingress and internal apply work, and pending/failed/cancelled standalone attempts retain editability. <!-- sdd-owner: implementation -->
- [ ] **REFACTOR** Replace scattered role checks with policy adapters, document the default-deny classification, keep error bodies generic, and rerun the full mutation matrix after route registration cleanup. <!-- sdd-owner: implementation -->
- [ ] **Focused verification:** Run `cd apps/api && go test ./internal/policy ./internal/httpserver ./internal/configuration ./internal/auth`; record direct-route, direct-service, local-allow-list, worker-capability, and pre-commit results. <!-- sdd-owner: implementation -->
- [ ] **Runtime harness:** Run a real API instance with a persisted committed-node identity and the existing worker fixture; issue direct HTTP probes for each mutation route and verify service health and synchronized apply behavior. <!-- sdd-owner: implementation -->
- [ ] **Rollback boundary:** Revert the policy, handler registration, and service injection as one atomic boundary; never remove only the guard while leaving a node-enrollment entry point active, and never downgrade a committed node to standalone. <!-- sdd-owner: implementation -->
- [ ] **Reviewable commit outcome:** Produce `feat(policy): enforce categorized node write boundaries` with policy, route/service wiring, matrix tests, and worker-capability tests together. <!-- sdd-owner: implementation -->

### 8. Minimal continuity UI and operator documentation

**Start:** `apps/ui/src/App.tsx` has dashboard routes for overview/DNS/certificates/ingress/streams, `dashboard-context.tsx` polls `/api/status`, and no continuity settings model or node-specific navigation exists.

**Finish:** Owners can safely preview/confirm/recover/cancel enrollment and inspect/retry committed-node sync without exposing secrets; the runbooks explain identity comparison, revocation, key backup, outage continuity, and rollback.

- [ ] **RED** Add UI tests/helpers for required local IP/HTTPS primary/token validation, password-style token input, immediate token clearing after request settlement, no token in URL/storage/telemetry/error text/DOM after submission, separate confirmation with `confirmReplacement:true`, and cancel preserving standalone state. <!-- sdd-owner: implementation -->
- [ ] **RED** Add state-rendering tests for server-driven pending/failed/recoverable/committed views, refresh polling, permitted-action gating, `aria-live` outcome, keyboard/focus transfer, non-color-only status, committed retry coalescing feedback, and hidden replicated editing controls. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Add typed continuity API helpers and views under `apps/ui/src/dashboard/ContinuityView.tsx` plus focused state utilities; extend `apps/ui/src/dashboard/types.ts` and `dashboard-context.tsx` for `/api/topology/enrollment`, `/api/topology/sync`, preview/confirm/recover/cancel/retry, and redacted status. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Add the Role/Continuity route/navigation in `apps/ui/src/App.tsx` and `apps/ui/src/dashboard/nav.ts`, and update `apps/ui/src/dashboard/DashboardShell.tsx`/dashboard views to hide replicated editing actions only after server-reported committed `NODE`; retain API authorization as the source of truth. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Update `docs/runbooks/primary-node.md` and `docs/security-operations.md` with out-of-band identity/fingerprint comparison, token handling, revocation effects, master-key-plus-wrapped-KEK backup, 30-day archive/recovery, outage/certificate behavior, compatibility-gated rollback, and explicit Phase 2 non-goals. <!-- sdd-owner: implementation -->
- [ ] **TRIANGULATE** Exercise the UI against fake API responses for mismatched identity, cancelled confirmation, browser refresh during recovery, failed initial apply, revoked node, unavailable primary, and manual retry already running; inspect DOM and browser storage for canary secrets. <!-- sdd-owner: implementation -->
- [ ] **REFACTOR** Keep secret handling isolated from reusable status state, use associated labels and stable accessible focus targets, remove any dashboard/inventory/diagnostics/promotion language, and rerun component/pure-state tests. <!-- sdd-owner: implementation -->
- [ ] **Focused verification:** Run `bun run test -- apps/ui/src`, `bun run typecheck`, and `bun run build:ui`; record accessibility, secret-clearing, server-state refresh, and committed-navigation results. <!-- sdd-owner: implementation -->
- [ ] **Runtime harness:** Use the Vite dev/preview UI against a fake API or two-installation test backend; if no browser automation is available, record manual keyboard/focus and storage inspection as the explicit harness result rather than claiming end-to-end browser coverage. <!-- sdd-owner: implementation -->
- [ ] **Rollback boundary:** Remove only the continuity route, UI state, and Phase 2 documentation additions; leave backend mutation guards, committed-node state, credentials, archives, and revocation behavior authoritative. <!-- sdd-owner: implementation -->
- [ ] **Reviewable commit outcome:** Produce `feat(ui): add minimal enrollment and node continuity settings` with UI code, tests, accessible copy, and operator documentation together. <!-- sdd-owner: implementation -->

### 9. End-to-end acceptance, security inventory, and release gates

**Start:** Work Units 1–8 are individually green and their migrations, APIs, scheduler, policy, UI, and worker paths are available for integrated verification.

**Finish:** The Phase 2 acceptance boundary is demonstrated without claiming Phase 3/4/5 behavior; blocked Docker/runtime checks are recorded honestly.

- [ ] **RED** Add an integrated two-installation test inventory under `apps/api/internal/phase2_test/` or the repository’s existing integration-test location for secure join, token non-reuse/hash-only storage, identity confirmation, sealed KEK/credential delivery, archive, initial apply gate, emergency Owner continuity, direct latest-snapshot convergence, exact acknowledgement, revocation, backoff, retry coalescing, outage continuity, and NODE mutation denial. <!-- sdd-owner: implementation -->
- [ ] **GREEN** Wire the integrated fixtures to the real Go API, disposable PostgreSQL, fake TLS primary, existing TypeScript worker/control path, canonical snapshot vectors, Linux amd64/arm64 renderer fixtures, and minimal UI API contracts; fault download, validation, promotion, reload, health, DB commit, restart, and ack boundaries. <!-- sdd-owner: implementation -->
- [ ] **TRIANGULATE** Verify plaintext enrollment tokens and all canary secrets are absent from both databases, logs, metrics labels, audit before/after JSON, ordinary responses, URLs, and browser storage; verify revocation stops pull/ack without erasing active DNS/proxy/certificate state. <!-- sdd-owner: implementation -->
- [ ] **TRIANGULATE** Run migration smoke/parity twice, full Go/TypeScript tests, direct mutation-route probes, persisted non-standalone startup, renewal suppression on NODE, primary outage observation, and exact latest-only convergence from a lower revision to a later published revision. <!-- sdd-owner: implementation -->
- [ ] **REFACTOR** Consolidate fixtures and redact integrated receipts, remove duplicate assertions already covered by work-unit tests, confirm generated migration identity, and update the Phase 2 acceptance evidence without expanding scope. <!-- sdd-owner: implementation -->
- [ ] **Focused verification:** Run `cd apps/api && go test ./...`, `bun run test`, `bun run typecheck`, `bun run build:ui`, and `bun run db:migrate` against an isolated database; record exact results and any skipped environment-dependent checks. <!-- sdd-owner: implementation -->
- [ ] **Runtime harness:** Run the Docker-backed two-installation/worker/CoreDNS/Nginx suite required by `openspec/config.yaml`; if Docker or required host services are unavailable, mark that gate blocked and preserve the command/output for a later verification phase. <!-- sdd-owner: implementation -->
- [ ] **Rollback boundary:** This unit changes only integration fixtures, release evidence, and runbook/receipt references; do not “fix” a failing gate by weakening a security test or altering production behavior outside its owning work unit. <!-- sdd-owner: implementation -->
- [ ] **Reviewable commit outcome:** Produce `test(phase2): verify enrollment and node continuity acceptance boundary` with integrated tests, security inventory, migration smoke evidence, and honest runtime results. <!-- sdd-owner: implementation -->

## Parent-controlled lifecycle gates

- [ ] Before apply begins, resolve the `ask-on-risk` decision, select `stacked-to-main` or `feature-branch-chain` if chaining, or obtain an explicit supported size-exception authorization; do not infer consent from the forecast. <!-- sdd-owner: parent -->
- [ ] After apply, start or reuse a bounded review for each selected work-unit/PR slice, confirm focused and runtime evidence, and keep any review correction within the owning slice. <!-- sdd-owner: parent -->
- [ ] Before final acceptance, confirm no task or diff includes Phase 3 dashboard/inventory, Phase 4 promotion/rejoin/election, or Phase 5 hardening/chaos/compatibility-matrix scope. <!-- sdd-owner: parent -->

## Phase 2 acceptance boundary

The implementation is complete only when the integrated evidence demonstrates: Owner-issued one-time authorization; verified primary identity and separate destructive confirmation; recoverable standalone-to-node conversion; local wrapping of the cluster KEK and node credential; 30-day archive retention; emergency Owner continuity; committed-NODE server-side mutation denial; complete latest-only snapshot pull through canonical validation/apply; exact post-apply acknowledgement; bounded restart-safe periodic sync and API retry; immediate credential revocation; and continued local DNS/proxy/certificate service while the primary is unavailable.
