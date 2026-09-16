# Exploration: Primary–Node continuity

Snapshot of the current ProxyCore codebase relevant to the
`docs/prd-primary-node-continuity.md` PRD, focused on Phase 0 (contracts)
and Phase 1 (local snapshot export/import) work.

## What already exists

The PRD is being added on top of an MVP that is already operational. The
following primitives are already present and can be reused as the substrate
for the PRIMARY/NODE model.

### Configuration snapshot and renderers

- `apps/api/internal/domain/snapshot.go` already implements a deterministic
  `StableStringify`, `ChecksumSnapshot` (sha256 hex), and
  `NormalizeSnapshot` over `ConfigurationSnapshot`. The `ConfigurationSnapshot`
  struct itself carries `Settings`, `Zones`, `Streams`, and `Certificates`.
  This is the natural place to anchor `snapshotVersion`, `revisionId`,
  `sourcePrimaryId`, `leadershipGeneration`, and `contentHash` fields for
  the new snapshot schema, because the checksum already covers the whole
  tree.
- `apps/api/internal/configuration/store.go` already persists
  `config_revisions` (number, checksum, snapshot JSON, actor, applied_at) and
  `apply_jobs`. The `RevisionRecord` model maps cleanly onto the PRD's
  per-revision acknowledgement surface (FR-13).
- `packages/renderers/src/coredns.ts` already accepts `IngressAddresses` as
  a parameter and renders proxied answers from it. `proxiedAnswers()` writes
  `A`/`AAAA` lines from `ingress.ipv4`/`ingress.ipv6`. This is exactly the
  node-local seam the PRD needs (FR-16). No renderer changes are required
  to support node-local rendering — we just need to pass a different
  `IngressAddresses` per node.
- `packages/renderers/src/nginx.ts` renders routes, certificates, basic
  auth, and stream listeners deterministically and includes a `checksum`
  derived from all candidate files. The renderer is also
  ingress-independent; per-node ingress differences live in DNS answers,
  not in Nginx.

### Identity, secrets, and authentication

- `apps/api/internal/auth/` (Go) implements scrypt password hashing,
  Owner/Operator roles, last-Owner protection, sessions with revocation,
  and bootstrap. This is the surface we need to gate behind the role
  authorization layer for Phase 1 (FR-2, FR-3).
- `packages/crypto/src/index.ts` is the Node-side twin that exports
  `hashPassword`, `verifyPassword`, `encryptSecret`, `decryptSecret`
  (AES-256-GCM with a 32-byte master key, base64url transport), and
  `createOpaqueToken`/`hashOpaqueToken`. The format
  `v1:<iv>:<tag>:<ciphertext>` is the natural place to introduce
  cluster-key envelopes for replicated secrets.
- `apps/api/internal/secrets/` exposes a `Store` interface and
  `NewPgStore(pool, masterKeyBase64)` implementation. The AES-GCM
  envelope with the local master key is the unit of trust that needs
  to remain decryptable after promotion.

### Apply and validation pipeline

- `apps/api/internal/configuration/store.go` already sequences
  desired-revision → apply-job → claimed-by-worker → applied/rolled-back.
  The Phase 1 round trip can reuse this same pipeline locally; we just
  need a new "imported snapshot" entry point that bypasses the desired
  mutation path but feeds the same render/validate/promote/reload
  pipeline.
- `apps/worker/src/render.ts` and `apply.ts` (Node) plus the legacy
  `apps/control/` socket protocol render CoreDNS/Nginx, validate, and
  apply. The PRD requires no changes to the worker apply contract; we
  only need a new source of snapshot data (an imported blob instead of a
  freshly produced one).

### Persistence

- `packages/db/src/schema.ts` and `apps/api/internal/configuration/schema.go`
  together define the schema. New tables for `installation_identity`,
  `node_state`, `enrollment_tokens`, and `applied_snapshots` are needed;
  the schema split between the two is mid-migration, so Phase 0 must
  add tables via `EnsureSchema` (Go) for the API path and update the
  Drizzle mirror for parity.
- `compose.yaml` runs PostgreSQL via `postgres:16-alpine` with
  `proxycore_jobs` LISTEN/NOTIFY wakeups; this already provides the
  background-wakeup the worker uses. No new infra is required for
  Phase 1.

## Gaps the PRD requires Phase 0 to close

| Gap | Where it lands | Notes |
| --- | --- | --- |
| Installation ID, node ID, leadership generation | New `installation_identity` table + `installation_id` UUID | Stable across restarts; generated once at first install. |
| Role model (`PRIMARY` / `NODE`) | New `node_state` row per installation | Defaults to `PRIMARY` for fresh installs. |
| Versioned snapshot schema with replicated/local/transient field classification | New `Snapshot` schema in `apps/api/internal/domain/` (and a matching TypeScript module) | Top-level `snapshotVersion`, `replicationVersion`, `contentHash`, `sourcePrimaryId`, `leadershipGeneration`, `nodeLocal` overlay. |
| Cluster-key envelope for replicated secrets | New helper in `packages/crypto` and Go twin | Wraps the existing AES-GCM with a cluster KEK derived at enrollment. |
| Promotion state machine | New `promotion.go` service | States: `standalone → primary → primary_with_nodes → promoted → stale_primary`. |
| Stale-primary startup guard | `cmd/server/main.go` | At boot, refuse to become PRIMARY if `installation_identity.leadership_generation < latest_known`. |
| Compatibility rules | New `snapshot_version.go` | Accept-list of `snapshotVersion` values the node can apply; rejects newer with a clear message. |
| Threat model | `docs/security-operations.md` | Documented alongside the snapshot contract. |

## Gaps Phase 1 closes

1. **Exporter** — `ExportSnapshot()` returns a canonical blob
   (`snapshotVersion=1`, `contentHash`, `sourcePrimaryId`,
   `leadershipGeneration`, full settings/zones/streams/certificates,
   `nodeLocal` overlay describing what a node should rewrite).
2. **Validator** — `ValidateSnapshot(blob)` checks schema, hash,
   snapshotVersion accept-list, and references before any side effect.
3. **Importer** — `ImportSnapshot(blob)` is the entry point that lets a
   standalone install become a NODE; it archives the prior desired state
   (30-day retention per resolved decision), downloads the snapshot blob,
   validates, applies atomically, and records `lastAppliedSnapshot`.
4. **Node-local CoreDNS rendering** — reuses the existing
   `renderCoreDnsCandidate({ ingress: nodeLocalIngress })` with the
   node's locally configured IPv4/IPv6. The PRD requires zero renderer
   changes.
5. **Atomic apply + rollback** — reuses the existing desired-revision
   pipeline: candidate → validate → promote → reload → health. A failed
   snapshot keeps the previous applied revision; nothing mutates the
   active data plane until validation passes.
6. **Cross-architecture fixtures** — `packages/renderers/src/coredns.test.ts`
   already has a fixture pattern; Phase 1 adds a fixture pair
   ("primary A" + "node B") that exports/imports and asserts the
   per-node proxied answers differ while DNS-only answers match.

## Architectural tensions flagged

- **TypeScript vs Go domain split.** `packages/domain` and
  `apps/api/internal/domain` model the same concepts. The Go API is the
  current source of truth for the API surface, but renderers and crypto
  remain in TypeScript. Phase 0 must therefore keep snapshots serializable
  from either side using `StableStringify` semantics (already shared)
  rather than relying on language-specific classes.
- **Ingress discovery.** `cmd/server/main.go` initializes the ingress
  IPv4/IPv6 from environment. In a multi-node topology, each node must
  override this with its locally configured value before rendering; the
  renderers already accept it as input, so the change is at the
  configuration layer.
- **Master key locality.** Today, the master key is set via
  `PROXYCORE_MASTER_KEY_BASE64` per host. With cluster-key secrets we
  add a second key (the cluster KEK) that is generated on the primary
  during enrollment and stored locally on the node. This means Phase 0
  must define how the cluster KEK is provisioned to the node (via the
  enrollment channel) and rotated (only on primary change).
- **Master key is currently process-level.** Phase 0 must add a durable
  `cluster_key` record so that the KEK survives restarts and can be
  shared with the worker. This record must be readable only by the
  local node, never serialized in API responses, and stored encrypted
  with the local master key.

## Open risks discovered

| Risk | Mitigation |
| --- | --- |
| Snapshot format drift between TypeScript and Go | Pin `snapshotVersion` in the schema; both sides round-trip through `StableStringify`. |
| Re-using `apply_jobs` for imported snapshots can confuse auditing | Tag imported jobs with `source: "import"` so they are distinguishable in queries. |
| The legacy `apps/worker/` is Node while the API is Go | Phase 1 keeps the worker untouched; the imported snapshot goes through the same desired-revision → apply-job path used today. |
| `packages/domain` is mid-migration | We add new types in both languages with explicit mirrors; do not mutate the old domain in Phase 0. |
| Container egress discovery for ingress | We do NOT auto-detect per-node ingress; the operator configures it explicitly. Document this in the operator runbook. |

## Work-item ordering for Phase 0

Phase 0 work units are intentionally ordered by dependency. Each unit
ships with focused Go/Vitest tests:

1. **Domain types** — `Role`, `NodeID`, `InstallationID`,
   `LeadershipGeneration`, `SnapshotVersion` in Go (and TS mirror).
2. **Schema migration** — new `installation_identity`, `node_state`,
   `enrollment_tokens`, `applied_snapshots` tables via `EnsureSchema`;
   update Drizzle mirror.
3. **Cluster-key envelope** — wrap AES-GCM secrets with a cluster KEK;
   helper `encryptWithClusterKey(plaintext, clusterKey)` /
   `decryptWithClusterKey(...)`.
4. **Snapshot schema** — versioned blob with replicated/local/transient
   classification, including `sourcePrimaryId`, `leadershipGeneration`,
   `contentHash`.
5. **Snapshot serialization** — go twin of `StableStringify` over the
   new schema, plus checksum helpers.
6. **Compatibility matrix** — accept-list + reject reason.
7. **State machine** — `promotion.go` with the five states and the
   transition guards.
8. **Stale-primary guard** — startup-time check in `cmd/server/main.go`.
9. **Threat model doc** — `docs/security-operations.md` update.

## Work-item ordering for Phase 1

Phase 1 work units follow Phase 0 and likewise ship with focused tests:

1. **Exporter** — primary API that serializes the desired state plus
   the snapshot metadata. Round-trip-stable across restarts.
2. **Validator** — schema check, hash check, compatibility check,
   secret availability check.
3. **Importer** — standalone install path: archive prior state, fetch
   snapshot blob, validate, write desired revision, enqueue apply job.
4. **Atomic apply + rollback** — reuses existing apply pipeline but
   enforces "previous known-good snapshot retained for rollback".
5. **Node-local rendering fixture** — `coredns.test.ts` extended with a
   primary A / node B pair that asserts per-node ingress rendering.
6. **Cross-arch fixtures** — minimal core-file golden fixtures that
   pass on both Linux/amd64 and Linux/arm64 (CI matrix later).
7. **Runbooks** — `docs/runbooks/primary-node.md` covering Phase 1
   operator steps.

## Out of scope for this change (matches PRD non-goals)

- Automatic leader election, quorum, witness node, automatic failback.
- Conflict merging between two writable primaries.
- Virtual IP / VRRP / anycast.
- PostgreSQL replication, switching to SQLite.
- Full audit/history replication (audit may be re-recorded locally;
  historical replication deferred).
- Active session replication.
- Internet-facing cluster discovery.
