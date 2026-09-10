# Tasks: Primary–Node continuity (Phase 0 + Phase 1)

Implementation is organized as reviewable work units, each with its failing
test, minimal implementation, focused verification, runtime harness, and
rollback boundary. Every Phase 0 work unit is the smallest behavior that
keeps the rest of the repo green. Phase 1 work units depend on Phase 0.

## Delivery forecast

Decision needed before apply: No (user already accepted `size:exception`
through `delivery_strategy: exception-ok` in `openspec/config.yaml`).

Chained PRs recommended: No (commits inside one PR; we are not opening a
PR in this change).

400-line budget risk: High. We will commit by work unit and keep authored
changes per commit small. Generated fixtures and golden files do not count
toward the authored budget.

Phase 0 is the critical safety net: it must be green before any Phase 1
work begins.

## Phase 0 — Contracts and safety boundaries

- [ ] 0.1 Add `Role`, `InstallationID`, `NodeID`, `LeadershipGeneration`,
      `SnapshotVersion`, `ReplicationVersion` types in
      `apps/api/internal/domain/` plus a TS mirror in
      `packages/domain/src/`. Tests cover generation, comparison, and
      parsing.
- [ ] 0.2 Add `EnsureSchema` tables `installation_identity` (single row),
      `node_state`, `cluster_keys`, `applied_snapshots` and Drizzle
      mirrors. Tests assert idempotent re-run and required columns.
- [ ] 0.3 Add identity service (`apps/api/internal/identity/`): generate
      installation ID on first boot, persist role and leadership
      generation, expose `IsStalePrimary()`. Tests cover first boot,
      restart, and stale detection.
- [ ] 0.4 Add cluster-KEK envelope helpers in
      `packages/crypto/src/cluster.ts` (TS) and
      `apps/api/internal/secrets/cluster.go` (Go): `WrapWithClusterKey`,
      `UnwrapWithClusterKey`, format `v1.kek:<iv>:<tag>:<ciphertext>`.
      Tests cover round-trip, tamper rejection, and wrong-KEK rejection.
- [ ] 0.5 Add snapshot schema (`apps/api/internal/snapshot/schema.go`)
      and TS mirror: replicated fields (settings, zones, streams,
      certificates, secrets, administrator hashes), `nodeLocal` overlay
      (ingress, nodeId, role, leadershipGeneration, clusterKeyRef),
      transient metadata (snapshotVersion, replicationVersion,
      contentHash, sourcePrimaryId, capturedAt). Tests cover schema
      round-trip and field classification.
- [ ] 0.6 Add snapshot serialization (Go) using existing
      `StableStringify`. Snapshot content hash uses the same canonical
      stringification. Tests assert byte-stable hashing across processes.
- [ ] 0.7 Add compatibility check (`apps/api/internal/snapshot/compat.go`):
      accept-list of `snapshotVersion` values; reject with explicit
      reason when the local node cannot apply. Tests cover accept and
      reject paths.
- [ ] 0.8 Add promotion state machine (`apps/api/internal/cluster/state.go`):
      `standalone-primary`, `primary`, `primary-with-nodes`, `node`,
      `stale-primary`. Transition guards: Owner-only; leadership
      generation monotonic. Tests cover valid and rejected transitions.
- [ ] 0.9 Wire stale-primary startup guard in `cmd/server/main.go`:
      refuse to expose writable endpoints when `IsStalePrimary()`.
      Tests cover guard on, guard off (no stale state), and process
      restart preservation.
- [ ] 0.10 Document the threat model in `docs/security-operations.md`:
      enrollment, snapshot delivery, secret transport, threat actors,
      mitigations.

Focused verification (per unit): `cd apps/api && go test ./...` plus
focused package Vitest where TS mirrors exist. Rollback boundary: revert
the unit's files; no schema drops in Phase 0 because Phase 1 depends on
the new tables.

## Phase 1 — Local snapshot export/import

- [ ] 1.1 Add snapshot exporter (`apps/api/internal/snapshot/export.go`):
      `ExportSnapshot()` produces the canonical blob from the current
      desired state. Round-trip stable across processes. Tests cover
      deterministic output and exclusion of node-local fields.
- [ ] 1.2 Add snapshot validator (`apps/api/internal/snapshot/import.go`,
      `Validate` step): schema, contentHash, compatibility, references,
      required secrets, certificate host coverage. Tests cover each
      rejection reason with a fixture.
- [ ] 1.3 Add snapshot importer (`apps/api/internal/snapshot/import.go`,
      `Import` step): archive prior standalone configuration with
      30-day TTL, write desired revision, enqueue apply job tagged
      `source: "import"`. Tests cover archive creation, TTL metadata,
      and desired-revision replacement.
- [ ] 1.4 Add atomic apply hook (`apps/api/internal/configuration/`):
      imported jobs use the existing desired-revision → apply-job
      pipeline; previous known-good snapshot retained; rollback on
      validation or health failure. Tests cover promotion to active,
      rollback to previous-good, and "no active data plane changes on
      failure" invariant.
- [ ] 1.5 Add node-local CoreDNS fixture (`packages/renderers/src/coredns.test.ts`):
      primary A and node B share DNS-only answers but answer proxied
      records with their own ingress IPv4/IPv6. Tests cover both
      directions and DNS-only equivalence.
- [ ] 1.6 Add cross-architecture fixture pair under
      `packages/renderers/src/__fixtures__/primary-node/`:
      `primary-a.json`, `node-b.json`, expected corefiles for amd64
      and arm64. Tests assert byte-stable rendering for both arches.
- [ ] 1.7 Add retention policy for the standalone archive:
      `apps/api/internal/snapshot/archive.go`. Tests cover 30-day
      purge path and "do not delete if Owner explicitly preserved".
- [ ] 1.8 Add `docs/runbooks/primary-node.md`: how an Owner exports a
      snapshot from A, imports into B locally, verifies DNS-only
      equivalence and per-node proxied answers, and what to do on
      failure.

Focused verification (per unit): `cd apps/api && go test ./...` plus
Vitest for renderer fixtures. Runtime harness: `go test ./internal/snapshot/...`
runs the full round-trip without Docker; CoreDNS/Nginx live verification
is intentionally deferred to Phase 5 hardening. Rollback boundary: revert
the unit's files; archive cleanup is best-effort and does not affect the
active data plane.

## Acceptance evidence (final)

Phase 0+1 done when:

1. `cd apps/api && go test ./...` is green.
2. `cd packages && bun test` (or `vitest`) is green.
3. The Phase 1 fixture round-trips a snapshot from a synthetic primary
   into a synthetic node and produces:
   - DNS-only answers identical between primary A and node B.
   - Proxied A records answered by A's ingress on A and B's ingress on B.
   - Both Nginx candidates routing to the same configured origins.
4. A deliberately corrupt snapshot (bad contentHash, missing secret,
   schema mismatch) is rejected without modifying the active data plane.
5. The stale-primary startup guard blocks configuration writes when the
   local leadership generation is older than the latest known.

## Out of scope (deferred to follow-up changes)

- Phase 2 secure enrollment, authenticated node-pull synchronization,
  token lifecycle, node revocation, acknowledgements.
- Phase 3 node-mode dashboard, primary node inventory, drift/health
  visibility, manual synchronize UX.
- Phase 4 executable promotion/rejoin workflows, complete stale-primary
  recovery UI, credential rotation on promotion.
- Phase 5 lifecycle hardening, mixed-version matrix, chaos coverage,
  low-resource qualification beyond fixtures feasible in Phase 1.

See `docs/prd-primary-node-continuity.md` "Explicit non-goals" for the
authoritative exclusions.
