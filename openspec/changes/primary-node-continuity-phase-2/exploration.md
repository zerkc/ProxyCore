# Exploration — Primary/Node continuity Phase 2

## Objective

Convert an independent ProxyCore installation into a read-only node through secure enrollment and complete-snapshot pull synchronization, while preserving recoverability and the Phase 0+1 data-plane continuity guarantees.

## Existing foundations

The archived `2026-09-11-primary-node-continuity` change provides durable installation identity, topology roles, leadership generation, cluster-KEK envelopes, versioned snapshots, validation, local import, archive retention, node-local ingress rendering, and stale-primary detection.

## Phase 2 scope

- Owner-created, short-lived, single-use enrollment tokens stored only as hashes.
- Primary identity preview and explicit destructive replacement confirmation.
- Authenticated encrypted enrollment over the existing API.
- Per-node revocable credentials replacing enrollment tokens.
- Complete-snapshot pull, validation, local apply, and acknowledgement.
- Periodic pull with bounded backoff and manual retry support at the API layer.
- Primary-side node revocation.
- Recoverable interrupted enrollment and 30-day standalone archive integration.
- Server-side rejection of ordinary configuration mutations after successful enrollment.
- Minimal node role settings UI required to perform enrollment.

## Confirmed product decisions

- Transport: node pull over the existing API.
- Secret transport: cluster KEK shared once during enrollment and wrapped locally.
- Owner credentials: replicated hashes plus a node-local emergency Owner credential.
- Certificate behavior: primary renews; disconnected node retains its last valid certificate.
- Standalone archive retention: 30 days.
- Node persistence: PostgreSQL remains in use.

## Non-goals

Node operations dashboard and primary inventory diagnostics beyond enrollment/sync essentials (Phase 3), promotion/rejoin (Phase 4), automatic election, push replication, database replication, and SQLite migration.

## Primary risks

- Token replay or leakage.
- Replacing standalone state before primary identity confirmation.
- Partial enrollment leaving ambiguous role or credentials.
- Snapshot acknowledgement before successful local apply.
- Revoked credentials retaining download access.
- Node UI hiding controls without API enforcement.

## Recommended change boundary

Use a separate OpenSpec change, `primary-node-continuity-phase-2`, depending conceptually on the archived Phase 0+1 capabilities. Keep implementation work units reviewable and forecast likely multi-PR pressure before apply.
