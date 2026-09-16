# Replicated Secrets Specification

## Purpose

Ensure replicated secrets remain confidential, portable, and decryptable after offline promotion.

## Requirements

### Requirement: Cluster KEK envelope

Replicated private keys, provider credentials, and other replicated secrets MUST be encrypted under a cluster key-encryption key (KEK). The primary MUST generate the KEK for authenticated enrollment, and a node MUST receive it only through that authenticated encrypted channel in a later phase. The KEK and plaintext secrets MUST NOT appear in snapshot plaintext, logs, or ordinary API responses.

#### Scenario: Snapshot does not expose secrets

- GIVEN an Owner exports a local snapshot containing certificate and provider credentials
- WHEN the blob and ordinary audit output are inspected
- THEN each replicated secret MUST be present only as KEK-protected ciphertext
- AND neither plaintext secrets nor the KEK MUST be disclosed

### Requirement: Durable local KEK protection

Each installation MUST persist its cluster KEK encrypted under that installation's local master key in PostgreSQL. The locally protected KEK MUST survive restart, MUST NOT be copied as node-local ciphertext between installations, and MUST be accessible only to authorized local secret-processing boundaries.

#### Scenario: Restart retains secret access

- GIVEN a Node has a locally protected cluster KEK and an imported snapshot
- WHEN the Node restarts without the primary
- THEN it MUST recover the KEK using its local master key
- AND it MUST be able to decrypt required replicated secrets

### Requirement: Offline promotion decryptability

Every required secret in an accepted snapshot MUST be decryptable locally with the installed cluster KEK before activation. The promotion contract MUST preserve decryptability without contacting the former primary and without promotion-time re-encryption.

#### Scenario: Transparent promotion secret contract

- GIVEN Node B has an accepted snapshot and its locally protected cluster KEK
- AND primary A is offline
- WHEN an Owner validates B for future promotion
- THEN B MUST decrypt every active-data-plane secret locally
- AND no secret MUST require re-encryption merely because B changes role

### Requirement: Complete secret validation

If any required secret envelope is malformed, uses an unavailable KEK, or fails authenticated decryption, the complete snapshot MUST be rejected before active-state mutation. Optional or unknown secret material MUST NOT silently weaken required functionality.

#### Scenario: Unusable secret rejects snapshot

- GIVEN a Worker validates a candidate with one undecryptable required private key
- WHEN secret validation runs
- THEN the Worker MUST reject the complete candidate with a redacted reason
- AND it MUST NOT replace any active configuration
