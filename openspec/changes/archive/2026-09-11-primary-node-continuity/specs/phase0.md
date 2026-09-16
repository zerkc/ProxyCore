# Phase 0 Acceptance Specification

## Purpose

Aggregate the acceptance boundary for contracts and safety behavior only.

## Requirements

### Requirement: Phase 0 contract gate

Phase 0 MUST provide testable identity, role, leadership, snapshot classification, compatibility, secret portability, rollback, promotion-continuity, and stale-primary contracts. Network enrollment, synchronization, dashboards, operational promotion/rejoin, election, VIPs, failback, database replication, and storage migration remain outside this specification.

#### Scenario: Transparent promotion contract

- GIVEN Owner B has the latest valid promotable snapshot and cluster KEK while A is offline
- WHEN the role transition contract is evaluated
- THEN it MUST preserve B's active CoreDNS and Nginx state and node-local ingress
- AND all replicated secrets MUST remain decryptable without re-encryption
- AND replicated configuration MUST be eligible to become editable

#### Scenario: Safe stale-primary return contract

- GIVEN Node A knows a leadership generation newer than its persisted writable generation
- WHEN A starts
- THEN A MUST preserve its local data plane
- AND A MUST block writes and MUST NOT overwrite newer leadership

#### Scenario: Unsafe snapshot contract

- GIVEN a Worker receives a snapshot with an unsupported version, bad hash, stale generation, invalid reference, or unusable secret
- WHEN contract validation runs
- THEN the snapshot MUST be rejected before active-state mutation
- AND the prior-known-good state MUST remain serviceable
