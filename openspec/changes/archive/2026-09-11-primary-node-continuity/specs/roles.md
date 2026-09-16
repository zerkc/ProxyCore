# Roles and Leadership Specification

## Purpose

Define durable installation identity and Phase 0/1 role-safety contracts without delivering network enrollment or an operational promotion workflow.

## Requirements

### Requirement: Durable installation identity

Each installation MUST generate exactly one immutable installation ID on first initialization and MUST preserve it across process, container, and host restarts. A node ID, when assigned, MUST identify that installation within its cluster and MUST NOT be imported from a snapshot.

#### Scenario: Fresh installation identity

- GIVEN an Owner initializes a fresh installation
- WHEN the installation starts for the first time and later restarts
- THEN the installation MUST retain the same installation ID
- AND it MUST operate with standalone-primary behavior

### Requirement: Durable role and leadership generation

The system MUST persist role and leadership generation in PostgreSQL. A fresh installation MUST default to standalone-primary behavior. Leadership generations MUST be monotonically ordered and snapshots MUST NOT reduce the latest generation known locally.

#### Scenario: Restart preserves role state

- GIVEN an Operator restarts an installation with persisted role and leadership state
- WHEN startup completes
- THEN the installation MUST restore that role and generation from PostgreSQL

### Requirement: Phase 0/1 transition boundary

Only an Owner MAY authorize a role transition. Phase 0/1 MUST define and validate the transitions `standalone-primary → NODE`, `NODE → PRIMARY`, and older-primary → `stale-primary`. Network enrollment, election, failback, and operational promotion/rejoin workflows remain outside this specification.

#### Scenario: Unauthorized transition is rejected

- GIVEN an Operator requests a role transition
- WHEN authorization is evaluated
- THEN the system MUST reject the transition without changing durable role state

### Requirement: Node write boundary

A NODE MUST reject ordinary DNS, proxy, certificate, functional-setting, and replicated-administrator mutations at the API boundary. It MAY accept explicitly node-local ingress changes. Workers MUST remain able to apply validated imported snapshots.

#### Scenario: Operator attempts a replicated mutation on a node

- GIVEN an Operator is authenticated to a NODE
- WHEN the Operator submits an ordinary configuration mutation
- THEN the NODE MUST reject it
- AND its active and desired configuration MUST remain unchanged

### Requirement: Stale-primary write guard

At startup, an installation whose leadership generation is older than its latest known generation MUST enter or remain in stale-primary guarded state, MUST preserve its active data plane, and MUST block configuration writes. It MUST NOT overwrite newer leadership or silently resume writable-primary operation.

#### Scenario: Safe stale-primary return

- GIVEN a Node records that B was promoted at a newer leadership generation
- WHEN former primary A starts with an older generation
- THEN A MUST continue serving its last valid local data-plane state
- AND A MUST reject configuration writes
- AND A MUST require an explicit future recovery or re-enrollment decision

### Requirement: Promotion continuity contract

The role model MUST permit a validated NODE snapshot to become editable PRIMARY desired state without changing node-local ingress, re-encrypting replicated secrets, or replacing the active data plane solely because of the role transition. This is a contract for later promotion implementation, not a Phase 0/1 promotion command.

#### Scenario: Transparent promotion contract

- GIVEN an Owner evaluates promotion of Node B while A is offline
- AND B holds a valid promotable snapshot
- WHEN the Phase 0 transition contract is validated
- THEN the contract MUST preserve B's CoreDNS and Nginx configuration
- AND it MUST preserve B's node-local ingress
- AND it MUST make replicated DNS, proxy, certificate, and administrator state eligible to become editable without secret re-encryption
