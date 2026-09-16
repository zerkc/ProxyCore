# Proposal: Primary/Node continuity — Phase 2

## Problem

Phase 0+1 established durable topology identity and safe local snapshot export/import, but converting an independent ProxyCore installation into a managed node still requires a secure network workflow. There is no supported way for an Owner to verify a primary, authorize destructive replacement of standalone configuration, establish revocable node credentials, or continuously converge through full snapshots.

Without Phase 2, operators cannot realize the intended two-host workflow without ad hoc credential exchange and manual snapshot movement. Such workarounds risk token leakage, accidental state loss, partial enrollment, premature acknowledgement, and a UI-only read-only boundary.

## Intent

Add the smallest secure enrollment and node-pull synchronization slice that turns an independent installation into a functioning read-only node while preserving recoverability and all Phase 0+1 data-plane safety guarantees.

## Goals

- Allow an authenticated Owner on a primary to create a short-lived, single-use enrollment token whose stored representation is a hash.
- Let a prospective node connect over the existing authenticated, encrypted API, verify and display the primary identity, and require explicit confirmation before replacing standalone state.
- Exchange the confirmed cluster KEK during enrollment and wrap it with the node-local key.
- Replace the one-time token with a dedicated, revocable per-node credential after successful enrollment.
- Pull the latest complete immutable snapshot, validate and apply it through the existing Phase 1 pipeline, and acknowledge only a successfully applied version.
- Periodically converge by pulling complete snapshots with bounded backoff; expose an API-level manual retry path.
- Archive the replaced standalone configuration under the existing 30-day retention policy and leave interrupted enrollment recoverable.
- Enforce node read-only behavior at the server/API boundary after enrollment succeeds.
- Provide only the minimal node role settings UI needed to complete enrollment safely.

## Scope

### Enrollment on the primary

- Owner-authorized creation of expiring, single-use enrollment tokens.
- Hash-only token persistence, one-time consumption, expiration enforcement, and replay rejection.
- Primary identity material sufficient for the prospective node to verify and present the intended primary before replacement.
- Enrollment over the existing API using authenticated encrypted transport.
- Issuance and persistence of a revocable per-node credential.
- Primary-side revocation that immediately denies future snapshot access by that credential.

### Conversion on the node

- Minimal settings flow for local node IP, primary address, enrollment token, identity preview, save/cancel, and explicit destructive confirmation.
- Archive of prior standalone configuration with 30-day retention before committed replacement.
- Durable enrollment state transitions that distinguish pending, committed, failed, and recoverable attempts.
- Cluster KEK receipt during enrollment and local wrapping; enrollment secrets must not enter logs or ordinary API responses.
- Retention of a node-local emergency Owner credential alongside replicated Owner password hashes.
- Transition to durable `NODE` mode only at the safe commit point, followed by server-side rejection of ordinary configuration mutations.

### Pull synchronization

- Node-initiated retrieval of the latest complete, immutable, versioned snapshot.
- Authentication using the per-node credential after enrollment.
- Integrity, compatibility, secret, CoreDNS, and Nginx validation through the canonical Phase 0+1 snapshot pipeline.
- Atomic local promotion and retention of the previous known-good snapshot.
- Acknowledgement only after the snapshot has been applied successfully.
- Periodic polling with bounded backoff, safe restart after interruption, and API-level manual retry.
- Convergence directly to the latest complete snapshot after one or many missed primary changes.
- Continued service from the last valid snapshot when the primary is unreachable or a candidate is rejected.

### Security and lifecycle rules

- PostgreSQL remains the persistence layer.
- The primary remains responsible for certificate renewal; a disconnected node retains its last valid certificate.
- Ordinary node configuration writes are denied by authorization/policy on the server, not merely hidden in the UI.
- Enrollment tokens, node credentials, cluster KEKs, private keys, provider credentials, and plaintext secrets are excluded from logs and ordinary responses.

## Non-goals

- Phase 3 node dashboard, primary node inventory, rich drift/health diagnostics, or operational reporting beyond enrollment and synchronization essentials.
- Phase 4 promotion, leadership transfer, stale-primary rejoin, or credential rotation associated with promotion.
- Phase 5 lifecycle hardening, compatibility matrices, chaos coverage, and extended recovery tooling.
- Automatic election, quorum, consensus, automatic failover, or automatic failback.
- Primary-push synchronization or arbitrary remote command execution.
- PostgreSQL physical/logical replication or any other database replication.
- SQLite migration or a reduced node persistence model.
- Virtual IP management, WAN discovery, session replication, job/history replication, or conflict merging.
- Changes to the canonical Phase 0+1 snapshot semantics except where integration is required to invoke them over the authenticated pull workflow.

## Affected areas and impact

- **Topology and authorization:** durable enrollment state and API mutation guards extend the existing role model.
- **Authentication and secrets:** enrollment-token lifecycle, node credentials, revocation, and cluster-KEK transfer introduce security-sensitive persistence and API surfaces.
- **Snapshot transport:** existing complete snapshot export/import becomes available through authenticated node pull and post-apply acknowledgement.
- **Workers/control plane:** periodic synchronization, bounded backoff, interruption recovery, and manual API retry are added without coupling data-plane service to primary availability.
- **Configuration archival:** conversion integrates with the existing archive machinery and its confirmed 30-day retention policy.
- **Frontend:** a deliberately minimal enrollment/settings flow is added; operational dashboards and inventory remain deferred.
- **Operations:** a primary apply remains independent from node convergence; node failure or drift must not roll back healthy primary state.

The implementation is likely to cross API, persistence, worker, authorization, snapshot, and minimal UI boundaries. Before apply, work should be decomposed into reviewable units and checked against the 400-line review budget; no delivery exception is implied by this proposal.

## Dependencies

- Canonical Phase 0+1 capabilities under `openspec/specs/` and their archived implementation record under `openspec/changes/archive/2026-09-11-primary-node-continuity/`.
- Durable installation ID, node ID, topology role, and leadership generation.
- Versioned complete snapshots and replicated/local/transient field classification.
- Cluster-KEK envelope support and node-local wrapping primitives.
- Deterministic export, validation, atomic import, prior-known-good rollback, and node-local ingress rendering.
- Existing authenticated API, Owner authorization, PostgreSQL persistence, worker/control pipeline, and standalone archive retention machinery.

No research lane is selected. Design must rely on these confirmed product decisions and repository contracts rather than reopening Phase 0+1 choices.

## Risks and mitigations

| Risk | Mitigation / required boundary |
| --- | --- |
| Enrollment token leakage or replay | Short expiry, hash-only storage, atomic single-use consumption, redaction, and replay tests. |
| Wrong primary or destructive replacement before consent | Verify and display primary identity before explicit archive/replace confirmation; do not commit role replacement earlier. |
| Partial enrollment leaves ambiguous role or credentials | Durable staged state machine, idempotent recovery, and a defined commit point; interruption must preserve either recoverable standalone state or a valid enrolled state. |
| Cluster KEK or credential exposure | Authenticated encrypted transport, local wrapping, no secret logging/ordinary response exposure, and narrowly scoped credential storage. |
| Snapshot acknowledged before safe apply | Emit acknowledgement only after complete validation and atomic successful promotion. |
| Revoked node retains pull access | Authorize every pull using current server-side revocation state; enrollment tokens cannot remain usable as node credentials. |
| Node remains writable through hidden endpoints | Central server-side mutation policy for NODE mode, covered independently of UI visibility. |
| Bad, incompatible, or interrupted snapshot disrupts service | Reuse complete Phase 1 validation and atomic apply; retain and serve the previous known-good snapshot. |
| Polling overloads constrained hosts or the primary | Bounded polling/backoff with one safely restartable synchronization path and no mutation replay queue. |
| Scope expands into operations or promotion | Keep UI/status to enrollment and synchronization essentials; defer inventory, rich diagnostics, promotion, and rejoin to their designated phases. |

## Rollout

1. Add backward-compatible enrollment, credential, revocation, and synchronization persistence without changing fresh-install standalone behavior.
2. Introduce primary enrollment endpoints behind Owner authorization and node enrollment endpoints with explicit identity and replacement confirmation.
3. Connect authenticated full-snapshot pull to the existing validation/apply pipeline, initially preserving the current active snapshot until the candidate succeeds.
4. Enable periodic pull and bounded backoff only for successfully enrolled nodes; expose manual retry through the API.
5. Add the minimal enrollment/settings UI after server-side state transitions and mutation guards are authoritative.

Existing standalone installations and Phase 0+1 local snapshot workflows must continue unchanged unless an Owner explicitly begins and confirms enrollment.

## Rollback and recovery

- Before enrollment commits, cancellation or failure leaves the installation standalone and editable.
- The prior standalone configuration is archived before replacement and retained for 30 days, enabling explicit recovery from interrupted or failed conversion.
- A failed or interrupted snapshot download/apply never replaces the active snapshot; the node continues serving the previous known-good state.
- Disabling periodic synchronization does not disable DNS or proxy service and does not erase the active snapshot.
- Primary-side revocation stops future downloads without remotely deleting or disabling the node's last valid data-plane configuration.
- Schema/API rollout must be backward-compatible so application rollback does not require destructive database reversal. Any rollback that would reinterpret a committed node as standalone is prohibited; recovery must be explicit rather than silently restoring write authority.

## Success criteria

- A clean installation B securely enrolls with primary A using an unexpired one-time token, after verifying A's identity and confirming archive/replacement.
- The enrollment token cannot be reused and is not stored in plaintext; B uses a revocable per-node credential for later pulls.
- B receives, validates, atomically applies, and acknowledges a complete snapshot, then serves equivalent DNS/proxy behavior with its own node-local ingress values.
- After missing multiple changes, B converges directly to A's latest complete snapshot without replaying intermediate mutations.
- A rejected, incompatible, interrupted, or partially downloaded snapshot leaves B serving its previous known-good snapshot and remains unacknowledged as applied.
- Revoking B prevents all subsequent authenticated snapshot downloads by B's credential.
- Ordinary configuration mutation requests to an enrolled node are rejected server-side.
- Interrupted enrollment has a documented and tested recovery path, with the prior standalone state archived for 30 days.
- Loss of A does not invalidate B's active data-plane configuration or retained certificate.

## Acceptance boundary

Phase 2 is accepted when all of the following are demonstrated end to end:

1. **Secure join:** Owner creates a short-lived token on A; B verifies A's identity, explicitly confirms replacement, archives its standalone state, enrolls once, stores the shared cluster KEK only in node-local wrapped form, and receives a revocable node credential.
2. **Read-only transition:** B becomes durably `NODE` only after the enrollment commit point, retains its emergency local Owner credential, and rejects ordinary configuration mutations at the API boundary.
3. **Full-snapshot convergence:** B pulls and safely applies the latest complete snapshot using Phase 0+1 validation and localization, acknowledges only the applied revision, and can skip multiple missed revisions by fetching the latest complete snapshot.
4. **Failure continuity:** interrupted enrollment is recoverable; interrupted or rejected synchronization leaves the prior snapshot active; primary unavailability leaves DNS and proxy service running from that snapshot.
5. **Revocation:** after A revokes B, B's established credential cannot download snapshots, while revocation alone does not erase B's active local data plane.
6. **Bounded synchronization:** successful enrollment enables periodic node pull with bounded backoff and an API-level manual retry path.

Acceptance does **not** require a Phase 3 dashboard or primary inventory, promotion/rejoin behavior, automatic leadership, database replication, SQLite, or Phase 5 hardening. Those capabilities must not be inferred from enrollment status, minimal synchronization state, or the presence of revocation records.
