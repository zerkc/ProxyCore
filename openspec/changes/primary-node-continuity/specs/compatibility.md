# Snapshot Compatibility Specification

## Purpose

Define fail-closed acceptance and rejection behavior for local snapshots.

## Requirements

### Requirement: Explicit accept-list

An importer MUST accept only explicitly supported `snapshotVersion` and `replicationVersion` combinations. It MUST validate schema, content hash, leadership generation, references, required secrets, and complete CoreDNS and Nginx candidates before creating any active-state side effect.

#### Scenario: Supported snapshot is eligible

- GIVEN an Operator supplies a well-formed local snapshot whose versions are on B's accept-list
- WHEN B validates integrity, generation, references, secrets, and rendered candidates
- THEN B MAY mark the snapshot eligible for atomic apply

### Requirement: Explicit rejection semantics

A malformed, corrupt, unsupported, stale-generation, referentially invalid, undecryptable, or unrenderable snapshot MUST be rejected with a stable, actionable, redacted reason. Rejection MUST NOT mutate desired state, active files, role, node-local settings, or the latest successfully applied snapshot.

#### Scenario: Rejected snapshot

- GIVEN Node B is serving snapshot 41
- WHEN a Worker validates local snapshot 42 with invalid Nginx configuration or unusable secrets
- THEN B MUST reject snapshot 42 and record the rejection reason locally
- AND B MUST continue serving snapshot 41
- AND Phase 1 MUST NOT require reporting that reason over a network transport

### Requirement: Prior-known-good continuity

Validation, import, and control-plane restart MUST preserve the prior known-good revision and its serviceability until the complete candidate has passed apply and health checks. Unsupported future versions SHOULD identify that a software update is required.

#### Scenario: Unsupported version preserves service

- GIVEN a Node is serving a prior-known-good snapshot
- WHEN an Operator imports a snapshot version outside the accept-list
- THEN the Node MUST retain and serve the prior-known-good snapshot
- AND the rejection SHOULD state that the software does not support the candidate version
