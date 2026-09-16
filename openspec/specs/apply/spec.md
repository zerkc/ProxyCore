# Snapshot Apply Specification

## Purpose

Define local import, archival, atomic activation, rollback, and audit behavior through the existing apply pipeline.

## Requirements

### Requirement: Local import boundary

Phase 1 import MUST consume a local immutable snapshot blob, MUST require authorized Owner initiation for destructive replacement of standalone desired state, and MUST NOT depend on enrollment, synchronization transport, acknowledgements, or a new node management endpoint.

#### Scenario: Owner starts local import

- GIVEN an Owner provides a local snapshot to standalone installation B
- WHEN import begins
- THEN B MUST validate the complete blob before replacing standalone desired state
- AND no connection to A MUST be required

### Requirement: Standalone archive retention

Before the first successful standalone-to-imported replacement, B MUST archive its prior standalone configuration with recoverable retention metadata. The archive MUST be retained for exactly 30 days and MUST then be purged automatically; failed import MUST NOT start or shorten that retention period.

#### Scenario: Standalone state remains recoverable

- GIVEN an Owner successfully imports A's snapshot into standalone B
- WHEN activation succeeds
- THEN B MUST retain its pre-import standalone configuration until the recorded 30-day expiry
- AND cleanup MUST purge it after expiry without deleting active or previous-known-good imported state

### Requirement: Imported revision attribution

An accepted import MUST create a revision and apply job through the existing apply pipeline. Both MUST identify the source as an import, the initiating actor, source primary ID, snapshot revision, content hash, and leadership generation without recording secrets.

#### Scenario: Imported job is auditable

- GIVEN an Owner imports a valid snapshot
- WHEN a Worker claims its apply job
- THEN the revision and job MUST be distinguishable from ordinary desired-state mutations
- AND attribution MUST identify the Owner and imported snapshot without exposing secret material

### Requirement: Atomic full-candidate apply

The Worker MUST render and validate complete CoreDNS and Nginx candidates using B's node-local overlay before promotion. It MUST activate the snapshot as one logical operation through the existing candidate, validate, promote, reload, and health pipeline. No failed stage MAY leave a mixed snapshot active.

#### Scenario: Candidate apply succeeds

- GIVEN a Worker receives a fully validated imported revision
- WHEN CoreDNS and Nginx candidate validation, promotion, reload, and health checks succeed
- THEN B MUST record that snapshot as active and last successfully applied
- AND both services MUST represent the same snapshot revision

### Requirement: Rollback and previous-known-good retention

B MUST retain the previous known-good revision until the imported candidate passes post-apply health checks. If promotion, reload, or health verification fails, the Worker MUST restore or continue the complete previous-known-good state and MUST record the failed candidate diagnostically.

#### Scenario: Apply failure rolls back

- GIVEN Node B is serving snapshot 41
- WHEN a Worker attempts snapshot 42 and a post-promotion health check fails
- THEN B MUST restore or continue serving the complete snapshot 41 configuration
- AND snapshot 42 MUST NOT become last successfully applied
