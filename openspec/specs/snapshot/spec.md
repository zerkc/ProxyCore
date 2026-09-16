# Snapshot Contract Specification

## Purpose

Define a complete, deterministic, portable snapshot for Phase 0/1 local export and import.

## Requirements

### Requirement: Versioned snapshot envelope

A snapshot MUST contain `snapshotVersion`, `replicationVersion`, `contentHash`, `sourcePrimaryId`, `leadershipGeneration`, an immutable revision identifier, replicated fields, and a `nodeLocal` overlay contract. The content hash MUST cover the canonical snapshot content except the `contentHash` field itself.

#### Scenario: Complete envelope export

- GIVEN an Owner requests local export from installation A
- WHEN A constructs the snapshot
- THEN the snapshot MUST identify its format, replication contract, source, revision, and leadership generation
- AND it MUST include a verifiable content hash

### Requirement: Replicated field completeness

Replicated fields MUST include managed zones and records; resolver pools and forwarding rules; proxy routes, origins, headers, protocols, timeouts, Basic Auth, and streams; active certificate chains, encrypted private keys, and continuity metadata; encrypted provider credentials; functional settings; and Owner/Operator identities including Owner password hashes. Active sessions and complete audit history MUST NOT be replicated.

#### Scenario: Promotable content is represented

- GIVEN an Owner exports a configured installation
- WHEN the snapshot is inspected against the classification contract
- THEN every field required to reproduce DNS, Nginx, certificates, credentials, and future editable ownership MUST be represented as replicated content

### Requirement: Field classification

Every snapshot-capable field MUST be classified as replicated, node-local, or transient. Node ID, role, local DNS and ingress addresses, primary endpoint, enrollment credentials, service health, applied timestamps, host paths, and runtime settings MUST be node-local or excluded. Locks, jobs, logs, metrics, caches, sessions, updater state, and PostgreSQL files MUST be transient and excluded.

#### Scenario: Local and transient data are not copied

- GIVEN a Node imports a snapshot from A into B
- WHEN imported content is materialized
- THEN A's node-local values MUST NOT replace B's values
- AND transient values MUST NOT be materialized from the snapshot

### Requirement: Canonical serialization

Serialization MUST be deterministic for semantically unchanged state, independent of map iteration, process restart, and supported Linux/amd64 or Linux/arm64 execution. Go and TypeScript consumers MUST produce the same canonical bytes and SHA-256 content hash for shared fixtures.

#### Scenario: Cross-architecture stable export

- GIVEN a Worker evaluates the same logical fixture on Linux/amd64 and Linux/arm64
- WHEN each runtime canonically serializes it
- THEN the bytes MUST match
- AND the content hashes MUST match

### Requirement: Immutable local export

A local export MUST represent one complete revision and MUST NOT change after creation. Export is a local/offline operation; networking and node management endpoints remain outside this specification.

#### Scenario: Repeated local export

- GIVEN an Operator exports unchanged desired state twice
- WHEN both exports complete
- THEN both immutable blobs MUST be byte-identical and have the same content hash
