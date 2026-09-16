# Delta for Snapshot Apply

## ADDED Requirements

### Requirement: Enrollment initial snapshot gate

The first snapshot obtained during enrollment MUST use the canonical complete-snapshot validation and atomic apply pipeline. Enrollment MUST NOT commit the installation to `NODE` mode until that snapshot is last successfully applied. Failure MUST preserve or restore recoverable standalone state and MUST NOT acknowledge the candidate as applied.

#### Scenario: Initial candidate applies successfully

- GIVEN enrollment has confirmed the primary, archived standalone state, and staged required secrets and credentials
- WHEN the initial complete snapshot passes the canonical apply pipeline
- THEN the snapshot MUST become last successfully applied before enrollment commits `NODE` mode
- AND only that successfully applied revision MAY be acknowledged

#### Scenario: Initial candidate apply fails

- GIVEN enrollment is pending and the prior standalone state is recoverable
- WHEN the initial snapshot fails validation, promotion, reload, or health checks
- THEN enrollment MUST remain uncommitted and recoverable
- AND no mixed or partial candidate state MAY remain active

### Requirement: Network source attribution

A snapshot pulled in Phase 2 MUST create revision and apply records through the existing apply pipeline. The records MUST identify the operation as synchronized import and MUST include node identity, source primary ID, snapshot version, revision, content hash, and leadership generation without recording enrollment tokens, node credentials, the cluster KEK, private keys, provider credentials, or plaintext secrets.

#### Scenario: Pulled apply is auditable without secret exposure

- GIVEN a NODE pulls and applies a complete snapshot
- WHEN its revision, job, and ordinary logs are inspected
- THEN the records MUST attribute the synchronized source and snapshot identifiers
- AND they MUST NOT expose enrollment or replicated secret material
