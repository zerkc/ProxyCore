# Enrollment Specification

## Purpose

Define Phase 2 primary identity verification, one-time enrollment authorization, recoverable standalone-to-node conversion, dedicated node credentials, and revocation. Promotion, rejoin, node inventory, and extended recovery tooling remain out of scope.

## Requirements

### Requirement: Owner-issued enrollment token

Only an authenticated Owner on a writable PRIMARY MUST be permitted to create an opaque enrollment token. The token MUST have a bounded expiration, MUST be accepted at most once, and MUST be persisted only as a cryptographic hash with lifecycle metadata. Plaintext token material MUST be returned only by the creation operation and MUST NOT appear in later ordinary API responses or logs.

#### Scenario: Owner creates a token

- GIVEN an authenticated Owner is operating a writable PRIMARY
- WHEN the Owner creates an enrollment token with an allowed expiration
- THEN the PRIMARY MUST return the plaintext token exactly for that creation operation
- AND it MUST persist only the token hash and lifecycle metadata

#### Scenario: Non-owner requests a token

- GIVEN a caller is not an authenticated Owner
- WHEN the caller requests an enrollment token
- THEN the PRIMARY MUST reject the request
- AND it MUST NOT create token state

#### Scenario: Expired or replayed token is presented

- GIVEN an enrollment token is expired, consumed, or revoked
- WHEN a prospective node presents it
- THEN the PRIMARY MUST reject enrollment
- AND it MUST NOT issue a node credential or disclose the cluster KEK

### Requirement: Primary identity confirmation

Before destructive replacement is authorized, the prospective node MUST obtain primary identity material over the authenticated encrypted API, MUST present the identity to the Owner, and MUST require explicit confirmation that the intended primary was identified. Connectivity or token validation alone MUST NOT constitute confirmation.

#### Scenario: Owner confirms the intended primary

- GIVEN a prospective node has contacted a primary and obtained verifiable identity material
- WHEN the node presents that identity to the Owner
- THEN enrollment MUST remain uncommitted until the Owner explicitly confirms the identity and replacement

#### Scenario: Owner cancels identity confirmation

- GIVEN the prospective node is displaying a primary identity
- WHEN the Owner cancels or declines confirmation
- THEN the installation MUST remain standalone and editable
- AND its existing configuration MUST remain active

### Requirement: Durable staged enrollment

Enrollment MUST durably distinguish pending, committed, failed, and recoverable attempts. Before commit, cancellation, process interruption, transport failure, or validation failure MUST leave the installation standalone and editable. Repeating recovery for the same attempt MUST be idempotent and MUST result in either recoverable standalone state or one valid committed enrollment, never an ambiguous writable NODE state.

#### Scenario: Enrollment is interrupted before commit

- GIVEN an Owner confirmed enrollment and the attempt is pending
- WHEN the process or transport fails before the safe commit point
- THEN restart MUST identify the attempt as failed or recoverable
- AND the installation MUST retain a safe path to its standalone state
- AND it MUST NOT silently become a writable or partially enrolled NODE

#### Scenario: Recovery is repeated

- GIVEN an enrollment attempt is recoverable after interruption
- WHEN recovery is invoked more than once
- THEN the system MUST preserve one consistent outcome
- AND it MUST NOT consume additional credentials, duplicate node identity, or repeat destructive replacement

### Requirement: Standalone archive before enrollment commit

Before committed replacement, the node MUST archive its prior standalone configuration with the canonical 30-day retention metadata. A failed or cancelled attempt MUST NOT shorten retention or make the archive unrecoverable. The role MUST become durably `NODE` only after the archive exists, the primary identity is confirmed, the cluster KEK is locally protected, a dedicated node credential is durably stored, and a complete snapshot has been successfully applied.

#### Scenario: Enrollment reaches its commit point

- GIVEN the Owner confirmed the primary and destructive replacement
- AND the prior standalone configuration has been archived
- AND the node has locally protected the cluster KEK and stored its node credential
- AND the initial complete snapshot has been successfully applied
- WHEN enrollment commits
- THEN the installation MUST durably enter `NODE` mode
- AND the standalone archive MUST remain recoverable until its recorded 30-day expiry

#### Scenario: Initial snapshot does not apply

- GIVEN enrollment is pending and the standalone archive exists
- WHEN the initial complete snapshot is rejected or its apply fails
- THEN enrollment MUST NOT commit `NODE` mode
- AND the prior standalone configuration and archive MUST remain recoverable

### Requirement: Dedicated node credential

After successful token authorization, the PRIMARY MUST issue a distinct credential scoped to one enrolled node. The node MUST persist that credential through an authorized local secret boundary, and all later snapshot pulls and acknowledgements MUST authenticate with it rather than the enrollment token. Node credentials MUST NOT appear in logs or ordinary API responses.

#### Scenario: Enrollment exchanges credentials

- GIVEN a valid unconsumed enrollment token authorizes a prospective node
- WHEN the PRIMARY completes the credential exchange
- THEN it MUST atomically consume the token
- AND it MUST issue a credential bound to that node
- AND subsequent synchronization MUST reject the enrollment token as authentication

### Requirement: Node credential revocation

An authorized Owner on the PRIMARY MUST be able to revoke an enrolled node credential. Every snapshot pull and acknowledgement MUST evaluate current server-side revocation state, and a revoked credential MUST be denied immediately for future requests. Revocation MUST NOT remotely erase or disable the node's active data-plane configuration.

#### Scenario: Revoked node attempts a pull

- GIVEN an Owner has revoked Node B's credential
- WHEN B requests a snapshot with that credential
- THEN the PRIMARY MUST deny the request
- AND it MUST NOT disclose snapshot or secret material
- AND B MUST continue serving its last successfully applied local snapshot

### Requirement: Emergency local Owner continuity

A committed NODE MUST retain a node-local emergency Owner credential in addition to Owner password hashes received through snapshots. The emergency credential MUST be excluded from replicated snapshot content and MUST NOT be overwritten by synchronization.

#### Scenario: Replicated administrators are updated

- GIVEN a NODE has a local emergency Owner credential
- WHEN it successfully applies a snapshot containing replicated Owner password hashes
- THEN the replicated identities MUST be updated according to the snapshot
- AND the local emergency Owner credential MUST remain usable and unchanged
