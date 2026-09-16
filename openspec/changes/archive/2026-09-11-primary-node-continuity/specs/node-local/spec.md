# Node-local Data-plane Specification

## Purpose

Define Phase 1 equivalence when the same logical snapshot is rendered with installation-local ingress.

## Requirements

### Requirement: Local CoreDNS ingress overlay

For proxied records, CoreDNS rendering MUST use the applying installation's configured local IPv4 and/or IPv6 ingress and MUST NOT use the source primary's ingress. Import MUST NOT overwrite B's local DNS or proxy ingress settings.

#### Scenario: Proxied answers are local

- GIVEN a Worker renders the same imported snapshot on A and B
- AND A and B have different configured ingress addresses
- WHEN each Worker renders proxied DNS answers
- THEN A's answers MUST target A's ingress
- AND B's answers MUST target B's ingress

### Requirement: DNS-only equivalence

DNS-only records and forwarding behavior MUST retain their configured logical values across source and importing installations, except for deterministic environment-independent serialization differences explicitly excluded by the contract.

#### Scenario: DNS-only records match

- GIVEN a Worker renders A's snapshot locally on B
- WHEN DNS-only records and forwarding rules are compared
- THEN B's logical answers and forwarding behavior MUST equal A's

### Requirement: Nginx route and origin equivalence

The same accepted snapshot MUST produce equivalent Nginx host, path, stream, TLS, authentication, and upstream-origin behavior on A and B. Node-local DNS ingress differences MUST NOT alter configured Nginx origins.

#### Scenario: Routes preserve origins

- GIVEN a Worker renders A's accepted snapshot on B
- WHEN representative Nginx routes are evaluated
- THEN B MUST route the same names and paths to the same configured origins as A

### Requirement: Offline data-plane independence

After successful local import, B's CoreDNS and Nginx request handling MUST NOT require A or any synchronization transport. Certificate renewal MUST remain primary-owned; a NODE MUST retain its last valid replicated certificate and MUST NOT attempt ACME renewal.

#### Scenario: Planned primary maintenance

- GIVEN an Operator has locally imported A's valid snapshot into Node B
- AND both installations are advertised as DNS resolvers
- WHEN A is stopped for planned maintenance
- THEN B MUST continue answering managed and forwarded DNS
- AND B's proxied answers MUST target B's ingress
- AND B's Nginx MUST continue routing to configured origins without contacting A
- AND B MUST retain its last valid replicated certificate without attempting renewal
