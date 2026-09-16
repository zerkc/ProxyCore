# Synchronization Specification

## Purpose

Define Phase 2 authenticated node-pull convergence using complete immutable snapshots. Primary inventory, rich health/drift reporting, push synchronization, mutation replay, and arbitrary remote commands remain out of scope.

## Requirements

### Requirement: Latest complete snapshot pull

A committed NODE MUST authenticate snapshot requests with its dedicated node credential and MUST pull a complete, immutable, versioned snapshot from its enrolled PRIMARY. The PRIMARY MUST offer the latest complete published snapshot eligible for that node; neither party MUST require replay of intermediate mutations or revisions.

#### Scenario: Node missed multiple revisions

- GIVEN Node B last applied revision 41 and the PRIMARY has published complete revision 47
- WHEN B performs an authenticated pull
- THEN the PRIMARY MUST make complete revision 47 available
- AND B MUST be able to converge without downloading revisions 42 through 46

#### Scenario: Partial download is interrupted

- GIVEN B is serving a last-known-good snapshot
- WHEN download of a newer snapshot is interrupted or incomplete
- THEN B MUST reject or discard the incomplete candidate
- AND its active snapshot MUST remain unchanged

### Requirement: Canonical validation and atomic apply

A pulled snapshot MUST pass the canonical integrity, compatibility, leadership-generation, reference, secret, CoreDNS, Nginx, node-local rendering, atomic promotion, reload, and health pipeline before it becomes active. A rejected or failed candidate MUST NOT partially replace active state and MUST leave the previous known-good snapshot serviceable.

#### Scenario: Pulled candidate succeeds

- GIVEN B has downloaded a complete compatible snapshot
- WHEN every canonical validation, promotion, reload, and health stage succeeds
- THEN B MUST make that revision active and last successfully applied
- AND CoreDNS and Nginx MUST represent the same revision using B's node-local ingress

#### Scenario: Pulled candidate is rejected

- GIVEN B is serving revision 41
- WHEN revision 42 fails integrity, compatibility, secret, CoreDNS, Nginx, or post-promotion health validation
- THEN B MUST continue serving complete revision 41
- AND revision 42 MUST NOT be recorded as successfully applied

### Requirement: Post-apply acknowledgement

A NODE MUST acknowledge a snapshot revision only after that exact revision has completed successful atomic apply and post-apply health checks. The acknowledgement MUST identify the node and applied snapshot version, revision, and content hash, MUST authenticate with the current node credential, and MUST NOT contain secrets. Download, validation start, or candidate staging MUST NOT count as acknowledgement of application.

#### Scenario: Successful apply is acknowledged

- GIVEN B has successfully applied revision 42 with its expected content hash
- WHEN B sends an acknowledgement
- THEN the PRIMARY MUST accept it only for B's authenticated identity
- AND the acknowledgement MUST identify revision 42 as applied

#### Scenario: Failed candidate is not acknowledged

- GIVEN B downloaded revision 42 but validation or apply failed
- WHEN synchronization records the outcome
- THEN B MUST NOT acknowledge revision 42 as applied
- AND its latest applied acknowledgement MUST NOT advance beyond its last successful revision

### Requirement: Periodic pull with bounded backoff

A committed NODE MUST support periodic pull through one safely restartable synchronization path. Repeated failure MUST increase delay according to configured lower and upper bounds, and a later success MUST restore the normal polling interval. Synchronization MUST NOT create an unbounded retry queue or block DNS and proxy request handling.

#### Scenario: Primary remains unreachable

- GIVEN periodic synchronization is enabled and the PRIMARY cannot be reached
- WHEN consecutive attempts fail
- THEN B MUST delay later attempts using bounded backoff no shorter than the configured minimum and no longer than the configured maximum
- AND B MUST continue serving its last valid snapshot

#### Scenario: Node restarts during backoff

- GIVEN B has a failed or interrupted synchronization attempt
- WHEN B restarts
- THEN synchronization MUST resume safely without corrupting or replacing the active snapshot
- AND it MUST NOT replay ordinary configuration mutations

#### Scenario: Synchronization recovers

- GIVEN B is in bounded backoff after failures
- WHEN a pull and apply succeeds
- THEN B MUST return to its normal periodic polling interval

### Requirement: API-level manual retry

The NODE API MUST expose an authorized manual synchronization retry operation. The operation MUST use the same credential checks, single synchronization path, complete-snapshot validation, apply, and acknowledgement rules as periodic polling. Concurrent manual and periodic triggers MUST NOT apply competing candidates concurrently.

#### Scenario: Authorized manual retry

- GIVEN an authorized local Owner requests a retry on a committed NODE
- WHEN no synchronization attempt is running
- THEN the API MUST trigger the normal synchronization path without waiting for the current backoff delay
- AND all ordinary validation and acknowledgement rules MUST still apply

#### Scenario: Retry requested while synchronization runs

- GIVEN a synchronization attempt is already active
- WHEN an authorized caller requests manual retry
- THEN the API MUST coalesce or reject the duplicate trigger deterministically
- AND it MUST NOT start a competing apply

### Requirement: Primary-unavailable continuity

Primary reachability and synchronization enablement MUST NOT be prerequisites for serving the active local data plane. A disconnected NODE MUST retain its last valid replicated certificate and MUST NOT attempt primary-owned certificate renewal.

#### Scenario: Primary is unavailable

- GIVEN B has successfully applied a snapshot
- WHEN the enrolled PRIMARY becomes unavailable or periodic synchronization is disabled
- THEN B MUST continue serving DNS and proxy traffic from that snapshot
- AND it MUST retain its last valid certificate without attempting renewal
