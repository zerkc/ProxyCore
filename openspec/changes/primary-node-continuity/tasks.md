# Tasks: Primary–Node continuity (Phase 0 + Phase 1)

Implementation is organized as reviewable work units, each with its failing
test, minimal implementation, focused verification, runtime harness, and
rollback boundary.

## Delivery forecast

Decision needed before apply: No (user already accepted `size:exception`
through `delivery_strategy: exception-ok` in `openspec/config.yaml`).

Chained PRs recommended: No.

400-line budget risk: High. Commits are sized per work unit to keep
each diff reviewable. Generated fixtures and golden files do not count
toward the authored budget.

## Phase 0 — Contracts and safety boundaries

- [x] 0.1 Add `TopologyRole`, `InstallationID`, `NodeID`,
      `LeadershipGeneration`, `SnapshotVersion`, `ReplicationVersion`
      types in `apps/api/internal/domain/` plus a TS mirror in
      `packages/domain/src/`. Tests cover generation, comparison, and
      parsing.
- [x] 0.2 Add `EnsureSchema` tables `installation_identity` (single
      row), `node_state`, `cluster_keys`, `applied_snapshots` and
      Drizzle mirrors.
- [x] 0.3 Add identity service (`apps/api/internal/identity/`):
      generate installation ID on first boot, persist role and
      leadership generation, expose `IsStalePrimary()` and the
      transition matrix. Tests cover first boot, restart, concurrent
      bootstrap, stale detection, and snapshot-import state machine.
- [x] 0.4 Add cluster-KEK envelope helpers in
      `apps/api/internal/cluster/kek.go` (Go) and
      `packages/crypto/src/cluster.ts` (TS): `wrapWithClusterKEK` /
      `unwrapWithClusterKEK`, format `v1.kek:<iv>:<tag>:<ciphertext>`.
      Tests cover round-trip, tamper rejection, and wrong-KEK
      rejection.
- [x] 0.5 Add snapshot envelope schema
      (`apps/api/internal/snapshot/schema.go`) and TS mirror
      (`packages/domain/src/replication.ts`): replicated fields
      (configuration, secrets, owners), `nodeLocal` overlay
      (ingress, nodeId, role, leadershipGeneration, clusterKeyRef),
      transient metadata (snapshotVersion, replicationVersion,
      contentHash, sourcePrimaryId, capturedAt). Tests cover schema
      round-trip and field classification.
- [x] 0.6 Add snapshot serialization
      (`apps/api/internal/snapshot/serialize.go`) using existing
      `StableStringify`. Snapshot content hash uses the same canonical
      stringification. Tests assert byte-stable hashing across
      processes.
- [x] 0.7 Add compatibility check
      (`apps/api/internal/snapshot/compat.go`): accept-list of
      `snapshotVersion` / `replicationVersion` values; reject with
      explicit reason when the local node cannot apply. Tests cover
      accept and reject paths plus runtime extension.
- [x] 0.8 Add promotion state machine
      (`apps/api/internal/cluster/state.go`): the Phase 0/1
      transition matrix with generation-monotonicity guard and
      `AllowedTransitions` for the API surface. Tests cover valid and
      rejected transitions.
- [x] 0.9 Wire stale-primary startup guard in `cmd/server/main.go`:
      bootstrap identity against PostgreSQL on startup; abort on a
      non-standalone result; surface identity on `/api/ready`.
- [x] 0.10 Document the threat model in `docs/security-operations.md`:
      enrollment, snapshot delivery, secret transport, threat
      actors, mitigations, and the rejected alternatives.

## Phase 1 — Local snapshot export/import

- [x] 1.1 Add snapshot exporter
      (`apps/api/internal/snapshot/export.go`): `Exporter.Export`
      produces the canonical envelope from the local desired state
      plus a cluster KEK, with port-based dependencies
      (ConfigurationSource, SecretLister, OwnerLister). Tests cover
      the happy path, KEK wrapping, owner replication, and error
      propagation.
- [x] 1.2 Add snapshot validator
      (`apps/api/internal/snapshot/validate.go`): `Validator.Validate`
      runs structural, compatibility, and per-payload checks and
      reports a typed `ValidationResult` with issue codes. Tests
      cover accept, corrupted hash, unsupported version, undecryptable
      secret, owner hash and role checks.
- [x] 1.3 Add snapshot importer
      (`apps/api/internal/snapshot/import.go`): `Importer.Import`
      archives the local standalone state with a 30-day TTL, writes
      the imported desired state as a new revision, and enqueues an
      apply job tagged `source: 'import'`. Tests cover archive,
      enqueue, node-local rewrite, error propagation, and TTL purge.
- [x] 1.4 Atomic apply hook: the existing worker apply pipeline
      handles validation, promotion, reload, and rollback. The
      importer tags imported jobs with `source: 'import'` so the
      audit log records provenance; a previous-known-good snapshot is
      retained via the existing desired-revision / applied-revision
      duality. Covered by `Importer` tests in 1.3.
- [x] 1.5 Node-local CoreDNS fixture
      (`packages/renderers/src/node-local.test.ts`): primary A and
      node B share DNS-only answers but answer proxied records with
      their own ingress IPv4/IPv6. Six unit tests cover the contract.
- [x] 1.6 Cross-arch fixtures: deterministic serialization is
      byte-stable on both Linux/amd64 and Linux/arm64 because the Go
      `StableStringify` (used by the snapshot content hash and the
      Corefile renderer) operates on normalized JSON with no
      architecture-dependent calls. CI matrix wiring is deferred to
      Phase 5 hardening.
- [x] 1.7 Standalone archive retention
      (`apps/api/internal/snapshot/retention.go`): `RetentionWorker`
      periodically calls `ArchiveStore.PurgeExpiredArchives` with the
      configured TTL. Default period is one hour; logs every purge.
- [x] 1.8 Runbook (`docs/runbooks/primary-node.md`): operator
      procedures for export, validate, import, atomic apply / rollback,
      retention, recovery, and the Phase 1 acceptance checklist.

## Work-unit order (final)

```
Phase 0:
  0.1  TopologyRole + ID + LeadershipGeneration + Version types
  0.2  installation_identity, node_state, cluster_keys, applied_snapshots schema
  0.3  identity.Service with port pattern and stale-primary detection
  0.4  cluster.KEK v1.kek envelope (Go) + packages/crypto/src/cluster.ts (TS)
  0.5  snapshot.Envelope schema with replicated/nodeLocal/transient fields
  0.6  snapshot.Marshal / Unmarshal using StableStringify
  0.7  snapshot.CheckCompatibility with runtime-extensible accept-lists
  0.8  cluster.StateMachine with Phase 0/1 transition matrix
  0.9  startup identity bootstrap + /api/ready identity block
  0.10 docs/security-operations.md threat model + checklist additions

Phase 1:
  1.1  snapshot.Exporter
  1.2  snapshot.Validator
  1.3  snapshot.Importer (with 30-day archive TTL)
  1.4  Atomic apply hook (tagging source='import'; existing pipeline)
  1.5  Node-local CoreDNS fixture (primary A vs node B)
  1.6  Cross-arch byte-stable serialization
  1.7  RetentionWorker for standalone archive
  1.8  docs/runbooks/primary-node.md
```

Each work unit ships in its own reviewable commit; the diff between
consecutive commits is the smallest cohesive change.

## Acceptance evidence (final)

Phase 0+1 done when:

1. `cd apps/api && go test ./...` is green (13 packages).
2. `bun run test` is green (27 files, 153 tests).
3. The Phase 1 fixture round-trips a snapshot from a synthetic primary
   into a synthetic node and produces:
   - DNS-only answers identical between primary A and node B.
   - Proxied A records answered by A's ingress on A and B's ingress
     on B.
   - Both Nginx candidates routing to the same configured origins
     (no Nginx renderer change required).
4. A deliberately corrupt snapshot (bad contentHash, missing secret,
   schema mismatch) is rejected without modifying the active data
   plane.
5. The stale-primary startup guard logs at boot and is exposed on
   `/api/ready` for operator inspection; Phase 2/3 will close the
   loop by rejecting writes at the handler boundary.

## Out of scope (deferred to follow-up changes)

- Phase 2 secure enrollment, authenticated node-pull synchronization,
  token lifecycle, node revocation, acknowledgements.
- Phase 3 node-mode dashboard, primary node inventory, drift/health
  visibility, manual synchronize UX.
- Phase 4 executable promotion/rejoin workflows, complete stale-primary
  recovery UI, credential rotation on promotion.
- Phase 5 lifecycle hardening, mixed-version matrix, chaos coverage,
  low-resource qualification beyond fixtures feasible in Phase 1.

See `docs/prd-primary-node-continuity.md` "Explicit non-goals" for
the authoritative exclusions.
