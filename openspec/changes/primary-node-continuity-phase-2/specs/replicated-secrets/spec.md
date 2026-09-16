# Delta for Replicated Secrets

## MODIFIED Requirements

### Requirement: Cluster KEK envelope

Replicated private keys, provider credentials, and other replicated secrets MUST be encrypted under a cluster key-encryption key (KEK). During Phase 2 enrollment, the PRIMARY MUST transfer the confirmed cluster KEK only to the authorized prospective node over the authenticated encrypted enrollment channel. The receiving node MUST immediately wrap the KEK with its node-local master key before durable persistence and MUST NOT persist the plaintext KEK. The KEK, plaintext secrets, node credential, and enrollment token MUST NOT appear in snapshot plaintext, logs, or ordinary API responses.

(Previously: The cluster KEK envelope was defined, but network transfer was deferred to a later authenticated phase.)

#### Scenario: Snapshot does not expose secrets

- GIVEN an Owner exports a snapshot containing certificate and provider credentials
- WHEN the blob and ordinary audit output are inspected
- THEN each replicated secret MUST be present only as KEK-protected ciphertext
- AND neither plaintext secrets nor the KEK MUST be disclosed

#### Scenario: Enrolled node protects the transferred KEK

- GIVEN a prospective node has confirmed the PRIMARY identity and is authorized by a valid enrollment token
- WHEN the PRIMARY transfers the cluster KEK through the authenticated encrypted enrollment channel
- THEN the node MUST wrap the KEK with its node-local master key before durable persistence
- AND no plaintext KEK MUST remain in durable enrollment state, logs, or ordinary API responses

#### Scenario: KEK protection fails

- GIVEN enrollment has not committed
- WHEN the node cannot wrap or durably persist the received cluster KEK
- THEN enrollment MUST fail or remain recoverable
- AND the installation MUST NOT enter committed `NODE` mode
