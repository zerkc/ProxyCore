# Proposal: Primary–Node continuity

## Intent

ProxyCore needs planned-maintenance and primary-loss continuity without introducing distributed-database or consensus complexity. Each installation should remain a complete DNS and proxy data plane, while a future `PRIMARY`/`NODE` topology gives one installation configuration ownership and lets enrolled nodes serve a validated, locally rendered copy while disconnected.

This change establishes the contracts and safety boundaries first, then proves local snapshot portability. Phase 0 defines durable identity, role, leadership, snapshot, secret, compatibility, promotion, rollback, and stale-primary semantics. Phase 1 implements deterministic local export, validation, import, atomic apply, and cross-architecture fixtures. This sequencing resolves the highest-risk questions—portable configuration, node-local ingress, decryptable secrets, rejected snapshots, and split-brain safety—before networking, enrollment UX, synchronization, or operational promotion are built.

The outcome is evidence that installation B can safely reproduce installation A's data plane from a complete snapshot, retain its own ingress identity, reject unsafe state without disturbing service, and preserve the contracts required for later node-pull enrollment and manual promotion.

## Scope

### In scope

#### Phase 0 — Contracts and safety boundaries

- Durable installation ID, node ID, role, and leadership generation, with fresh installations defaulting to standalone-primary behavior.
- A versioned, deterministic snapshot contract containing all promotable DNS, proxy, certificate, encrypted-secret, functional-setting, and administrator identity data.
- Explicit replicated, node-local, and transient field classification.
- Cluster KEK envelopes for replicated secrets, including durable local protection of the KEK by the installation master key.
- Compatibility rules that reject unsupported snapshot versions before side effects.
- Promotion/stale-primary state-machine contracts and startup write guard behavior.
- Rollback rules, previous-known-good snapshot persistence, and enrollment/snapshot threat-model documentation.
- PostgreSQL schema support and TypeScript schema parity for installation and snapshot state.

#### Phase 1 — Local snapshot export/import

- Deterministic export of a complete immutable snapshot from installation A.
- Local/offline validation and import into installation B; no network enrollment or synchronization transport.
- Validation of schema, content hash, compatibility, references, required secrets, CoreDNS output, and Nginx output before activation.
- Atomic application through the existing revision/apply pipeline, with imported-job audit attribution and retention of the previous known-good state.
- Archival of B's prior standalone configuration, with automatic purge after 30 days.
- Node-local CoreDNS ingress overlay while preserving equivalent DNS-only answers and Nginx origins.
- Portable fixtures demonstrating equivalent behavior on Linux/amd64 and Linux/arm64.
- Phase 1 operator documentation.

### Out of scope

- Phase 2 secure enrollment, authenticated node-pull synchronization, token lifecycle, node revocation, and acknowledgements.
- Phase 3 node dashboard, primary node inventory, drift/health visibility, and manual synchronization UX.
- Phase 4 executable promotion/rejoin workflows and complete operational stale-primary recovery UI.
- Phase 5 lifecycle hardening, mixed-version operations, chaos coverage, and low-resource qualification beyond fixtures feasible in Phase 1.
- Automatic election, quorum, witness nodes, automatic failback, conflict merging, virtual IPs, PostgreSQL replication, SQLite migration, full history/session replication, and Internet-facing discovery.

See the PRD's [Explicit non-goals](../../../docs/prd-primary-node-continuity.md#explicit-non-goals) for the authoritative product exclusions.

## Approach

All implementation follows strict TDD: add a focused failing behavior test, make the smallest implementation pass, then refactor while green. Run narrow package tests first and use `cd apps/api && go test ./...` as the Go regression boundary. Tests remain in the same reviewable work unit as the behavior they verify.

### Phase 0 work plan

1. **Identity and role contracts:** add mirrored Go/TypeScript value types and durable PostgreSQL state for installation identity, node role, node identity, and leadership generation.
2. **Snapshot contract:** define versioned snapshot metadata and replicated/local/transient classifications; preserve deterministic canonical serialization across Go and TypeScript.
3. **Secret portability:** add cluster-KEK encryption/decryption twins, store the KEK encrypted under the local master key, and prove secrets remain decryptable without the old primary or promotion-time re-encryption.
4. **Compatibility and lifecycle safety:** add version accept-list behavior, promotion state transitions, replay/generation checks, and the stale-primary startup write guard.
5. **Security and rollback contract:** document enrollment/snapshot threats, key handling, split-brain behavior, rejected-candidate retention, and previous-known-good rollback.

Phase 0 exits only when a snapshot can reconstruct DNS and Nginx safely, imported state cannot overwrite node-local ingress, secrets remain usable after offline promotion, and split-brain behavior is documented and testable.

### Phase 1 work plan

1. **Exporter:** serialize desired state, identities, owner/operator records (including Owner password hashes), certificates, cluster-key-encrypted secrets, metadata, and content hash into a deterministic v1 blob.
2. **Validator:** reject malformed, corrupt, incompatible, stale-generation, referentially invalid, or undecryptable snapshots before any active-state mutation.
3. **Importer and archive:** archive standalone desired state, schedule its 30-day purge, validate the local blob, create an import-attributed revision/apply job, and record the last applied snapshot.
4. **Atomic apply and rollback:** render candidates with B's local ingress, validate CoreDNS and Nginx, promote only the complete candidate, and retain the previous known-good revision on every failure path.
5. **Portability fixtures and runbook:** prove the A-to-B round trip and logical output equivalence on amd64/arm64 fixtures; document local import and recovery.

Phase 1 exits only when A-to-B import preserves DNS-only answers and Nginx origins, uses each installation's local ingress for proxied answers, rejects unsafe snapshots without replacing active state, and lets B serve the imported configuration with A offline.

## Decisions locked

| Topic | Locked decision |
| --- | --- |
| Scope | This change commits Phase 0 and Phase 1 only; Phases 2–5 require follow-up changes after Phase 1 verification. |
| Secret transport | The primary generates a cluster KEK during enrollment and provisions it over the authenticated enrollment channel. Replicated secrets use that KEK; promotion requires no re-encryption. |
| Sync transport | Future synchronization is node-pull over the existing API; no additional inbound node management endpoint. |
| Owner credentials | Replicate Owner password hashes to preserve identity and generate a per-node emergency local Owner credential during enrollment for disconnected access. |
| Certificate renewal | PRIMARY renews certificates; disconnected NODEs retain the last valid replicated certificate and never attempt ACME while in NODE role. |
| Standalone archive | Retain the pre-enrollment standalone archive for 30 days, then purge it automatically. |
| NODE persistence | Keep PostgreSQL for the full NODE lifecycle, including audit, last-applied snapshot, and rollback state; SQLite migration is excluded. |
| Workflow | Strict TDD, `delivery_strategy=exception-ok`, OpenSpec artifacts, and automatic SDD phase execution. |

## Success criteria

### Phase 0/1 measurable gates

- Canonical export of unchanged state is byte-stable and produces the same content hash across repeated runs and supported architectures.
- Every field needed for DNS, Nginx, certificates, credentials, and future promotion is represented; node-local and transient fields are demonstrably excluded or overlaid.
- Cluster-KEK-encrypted secrets and replicated Owner password hashes round-trip on B without access to A; the local emergency Owner contract is defined for later enrollment implementation.
- Unsupported versions, changed hashes, stale generations, broken references, unusable secrets, and invalid CoreDNS/Nginx candidates produce explicit rejection reasons and zero active-data-plane replacements.
- Import retains the previous known-good snapshot and a recoverable standalone archive; archive expiry is exactly 30 days and cleanup is testable.
- Cross-architecture primary-A/node-B fixtures pass for Linux/amd64 and Linux/arm64: DNS-only answers match, proxied answers target each host's own ingress, and Nginx routes target the same origins.
- B continues serving its imported snapshot in a local offline-primary test for at least the test's bounded observation period, with no control-plane dependency on A.

### Acceptance-scenario traceability

| PRD scenario | Evidence committed by this change | Deferred end-to-end evidence |
| --- | --- | --- |
| Planned primary maintenance | Phase 1 local A→B import proves B's independent DNS/Nginx service, local ingress rendering, and operation with A absent. | Enrollment, periodic pull, and operational status are Phase 2/3. The PRD's 24-hour operational soak belongs to later hardening. |
| Transparent promotion | Phase 0 snapshot/secret/identity contracts prove retained configuration is promotable without re-encryption; state-machine tests prove ingress is invariant across promotion. | Owner-triggered promotion UI/API, accepting future nodes, and live workflow are Phase 4. |
| Safe stale-primary return | Phase 0 generation/replay contracts and startup-guard tests prove an older generation cannot silently resume writes or overwrite newer leadership. | Recovery/re-enrollment UI and complete two-host journey are Phase 4. |
| Rejected snapshot | Phase 1 tests import an invalid Nginx or unusable-secret snapshot after a known-good revision and prove rejection, reason capture, and continued service from the prior snapshot. | Reporting the rejection to A over node-pull is Phase 2/3. |

## Rollback plan

Each unit is independently reviewable and keeps tests with code. Revert the corresponding work-unit commit; do not combine unrelated schema, crypto, import, and UI contracts in one rollback.

| Work unit | Rollback boundary |
| --- | --- |
| Identity/role domain | Remove new mirrored types and role gates; fresh-install standalone behavior remains the prior default. |
| PostgreSQL/Drizzle schema | Stop all new readers/writers first, then revert additive schema code. Additive tables may remain inert to avoid destructive rollback; remove them only through an explicit, backed-up migration. |
| Cluster-key envelope | Revert envelope helpers and cluster-key consumers together. Existing local-master-key ciphertext remains unchanged; never delete stored key material while snapshots reference it. |
| Snapshot schema/serialization | Revert v1 exporter/consumer as one unit; retain stored blobs as inert diagnostics until retention cleanup. |
| Compatibility/state machine/startup guard | Revert guards only if snapshot import and role transition entry points are also disabled, preventing unsafe partial activation. |
| Exporter | Remove export entry points and fixtures; no active state changes are involved. |
| Validator/importer/archive | Disable import before reverting persistence. Restore B from the retained standalone archive or previous known-good revision; cancel associated purge jobs during recovery. |
| Atomic apply/rollback | Route imports away from the apply pipeline and reactivate the recorded previous known-good revision; do not delete diagnostic candidates until service health is confirmed. |
| Fixtures/runbooks/UI contract changes | Revert independently because they do not mutate runtime state; retain warnings if backend behavior remains deployed. |

## Affected areas

- `apps/api/internal/domain/` — identity, role, leadership, versioned snapshot, canonical serialization, and field-classification contracts.
- `apps/api/internal/configuration/` — export/import services, validation, imported revision attribution, archive retention, last-applied/known-good state, and apply integration.
- `apps/api/internal/auth/` — Owner-only role authorization, replicated password-hash representation, and emergency Owner contract boundaries.
- `apps/api/internal/secrets/` — durable cluster KEK storage protected by the local master key and replicated-secret access.
- `apps/api/internal/` role/promotion packages — promotion state machine, compatibility rules, replay protection, and API mutation guards.
- `apps/api/cmd/server/` — startup identity initialization and stale-primary write guard.
- `apps/api/cmd/` — local export/import command surfaces or administrative wiring used to prove Phase 1 without network synchronization.
- `packages/crypto/` — TypeScript cluster-key envelope helpers and compatibility with the Go implementation.
- `packages/renderers/` — primary-A/node-B CoreDNS and Nginx fixtures; renderer code changes only if tests expose a missing node-local seam.
- `packages/db/` — Drizzle schema mirror for installation identity, node state, enrollment-ready records, snapshots, archives, and retention metadata.
- `apps/ui/` — role/status and destructive-action contract types or guarded placeholders needed by Phase 0; full node-mode, enrollment, and promotion UX is deferred.
- `compose.yaml` — PostgreSQL-backed persistence/key wiring and local Phase 1 verification configuration; no new replication infrastructure.
- `docs/security-operations.md` and `docs/runbooks/primary-node.md` — threat model, split-brain/rollback boundaries, local snapshot workflow, and future operational constraints.

## Risks

- **Cross-language snapshot drift:** pin versions, use canonical serialization, and require shared fixtures across Go and TypeScript.
- **Secret lockout after promotion:** prove offline decryption with the cluster KEK before Phase 0 exits; never require promotion-time re-encryption.
- **Partial apply damages service:** validate the complete candidate first and retain the prior known-good revision until post-apply health succeeds.
- **Node-local state overwritten:** encode localization in the schema and assert imported ingress cannot replace B's configured address.
- **Role contracts mistaken for delivered topology:** clearly gate Phase 2–5 APIs/UI and describe Phase 0 promotion behavior as contract/test coverage, not a production promotion workflow.
- **Standalone recovery expires unexpectedly:** expose deterministic 30-day retention metadata and test purge scheduling; destructive recovery remains explicit.
- **Review workload exceeds 400 lines:** the accepted `exception-ok` strategy permits a documented `size:exception`, but implementation should still use cohesive, reversible work-unit commits rather than code-golfing or file-type splits.
