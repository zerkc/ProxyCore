# Phase 1 Acceptance Specification

## Purpose

Aggregate local snapshot export/import evidence without Phase 2 networking.

## Requirements

### Requirement: Phase 1 local continuity gate

Phase 1 MUST prove deterministic local A-to-B export/import, complete validation, atomic apply, rollback, local ingress overlay, offline service, and equivalent Linux/amd64 and Linux/arm64 fixture behavior. Enrollment, pull transport, acknowledgements, node inventory, and operational promotion remain outside this specification.

#### Scenario: Planned primary maintenance by local import

- GIVEN an Owner exports A's complete snapshot and imports it locally into B
- AND an Operator configures clients to use both installations as DNS resolvers
- WHEN A is stopped
- THEN B MUST continue managed and forwarded DNS and equivalent Nginx routes
- AND B MUST use its own ingress for proxied answers without contacting A

#### Scenario: Transparent promotion portability by local import

- GIVEN an Owner has locally imported A's latest valid snapshot into B
- AND A is offline
- WHEN B's promotability is tested without executing a production promotion workflow
- THEN B MUST retain its active data plane and local ingress
- AND all required replicated configuration and secrets MUST be available without re-encryption

#### Scenario: Safe stale-primary return by local fixtures

- GIVEN a Node fixture records B at a newer leadership generation than A
- WHEN A's startup guard is exercised
- THEN A MUST reject writable-primary startup
- AND A MUST NOT alter B or require network communication with B

#### Scenario: Rejected local snapshot

- GIVEN Node B is serving locally imported snapshot 41
- WHEN an Owner imports snapshot 42 with invalid Nginx configuration or unusable secrets
- THEN B MUST reject snapshot 42 with a redacted local reason
- AND B MUST continue serving snapshot 41

#### Scenario: Cross-architecture local round trip

- GIVEN a Worker runs the shared A-to-B fixture on Linux/amd64 and Linux/arm64
- WHEN each environment exports, imports, renders, and validates the snapshot
- THEN canonical bytes and hashes MUST match
- AND DNS-only answers and Nginx origins MUST be logically equivalent
- AND proxied answers MUST target each fixture's node-local ingress
