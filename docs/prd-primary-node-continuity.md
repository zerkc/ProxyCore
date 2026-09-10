# PRD: Primary–Node continuity for DNS and proxy

ProxyCore will support a low-complexity `PRIMARY`/`NODE` topology for LAN continuity. Each installation remains a complete, independently runnable data plane. A `PRIMARY` owns configuration; a `NODE` receives validated snapshots and serves DNS and proxy traffic from its own IP. If the primary host is removed for maintenance or fails, the node keeps serving its last valid snapshot and can be promoted manually without rebuilding configuration.

## Product decision

| Topic | Decision |
| --- | --- |
| Topology | One writable `PRIMARY` and one or more read-only `NODE` installations |
| Availability target | Keep DNS and proxied routes available during planned maintenance or loss of the primary host |
| DNS behavior | Each CoreDNS instance returns its own node-local Nginx ingress IP for proxied records |
| Proxy behavior | Every node runs Nginx with the same routes, certificates, credentials, and upstream definitions |
| Synchronization | The primary publishes versioned desired-state snapshots; nodes validate and apply them locally |
| Offline behavior | A node serves the last successfully applied snapshot without contacting the primary |
| Promotion | Manual `NODE → PRIMARY`; synchronized state becomes locally editable without reconfiguration |
| Failback | Never automatic; the former primary must explicitly rejoin the current primary as a node |
| Database replication | Not required; this is configuration replication, not PostgreSQL high availability |
| Storage migration | Moving PostgreSQL to SQLite is outside this PRD |
| Virtual IP | Not used |
| Automatic election/quorum | Not used |

## User outcome

An owner can install ProxyCore normally on hosts A and B, convert B into a node of A, and then manage the installation exclusively from A. Both hosts answer DNS and proxy requests independently. Removing A does not stop B's data plane. The owner may promote B to primary, after which B exposes the full administration experience with the configuration it already had.

## Network model

The LAN DHCP server advertises both ProxyCore hosts as DNS resolvers:

```text
DNS 1: host A
DNS 2: host B
```

Each host uses its own ingress address for proxied answers:

```text
CoreDNS A: app.home.arpa → Nginx A
CoreDNS B: app.home.arpa → Nginx B
```

Both Nginx instances route the hostname to the same configured origin:

```text
Nginx A ─┐
         ├── app.home.arpa → 192.168.1.50:8080
Nginx B ─┘
```

This avoids virtual IP requirements and makes promotion transparent to the data plane: promotion changes configuration ownership, not the node-local DNS or Nginx addresses already in service.

> DNS resolver fallback is client-controlled. Cached DNS answers may continue targeting the previous node until their TTL expires. This design improves LAN continuity but does not promise zero-second failover for every client.

## Roles

### Standalone

A fresh installation starts as a standalone primary. It can be administered normally and does not participate in replication.

### PRIMARY

The primary:

- owns editable desired configuration;
- authenticates administrators;
- creates versioned replication snapshots;
- distributes snapshots to enrolled nodes;
- displays node synchronization and data-plane health;
- performs certificate issuance and renewal unless later delegated explicitly;
- remains fully functional without any node.

### NODE

A node:

- exposes only node-mode settings and operational status in its UI;
- rejects ordinary configuration mutations;
- receives snapshots only from its enrolled primary;
- validates snapshots before applying them;
- runs its own CoreDNS and Nginx;
- rewrites installation-local ingress values to its configured local IP;
- keeps serving the last valid snapshot while disconnected;
- can be manually promoted to primary.

## Primary journeys

### 1. Install two independent instances

1. The owner installs ProxyCore normally on host A.
2. The owner installs ProxyCore normally on host B.
3. Both start as independent primary-capable installations.
4. Existing single-host behavior remains unchanged until a role change is requested.

### 2. Enroll B as a node of A

1. On A, the owner selects **Add node** and creates a short-lived, single-use enrollment token.
2. On B, the owner changes the installation mode to `NODE`.
3. B presents only:
   - local node IP;
   - primary IP or URL;
   - enrollment token;
   - save/cancel actions.
4. B verifies connectivity and presents the identity of A before destructive replacement.
5. The owner confirms that B's independent configuration will be archived and replaced.
6. B enrolls with A using the token.
7. B downloads the latest complete snapshot.
8. B validates CoreDNS, Nginx, certificates, and snapshot integrity.
9. B atomically promotes the candidate and records it as its last valid snapshot.
10. B enters read-only node mode.

### 3. Operate from the primary

1. An operator changes DNS or proxy configuration on A.
2. A applies the configuration locally through the existing worker pipeline.
3. After local validation succeeds, A publishes a new immutable snapshot.
4. B downloads, validates, and applies that snapshot using its own worker/control pipeline.
5. For proxied records, B renders its own ingress IP instead of A's ingress IP.
6. A displays desired, distributed, and applied versions for every node.

A primary apply and a node apply are separate outcomes. Failure on B must not roll back a healthy apply on A; it must leave B on its previous snapshot and report drift visibly.

### 4. Primary unavailable

1. A becomes unavailable or is stopped for maintenance.
2. B detects loss of synchronization but does not disable its data plane.
3. B continues serving DNS and Nginx from its last valid snapshot.
4. B does not attempt certificate renewal, desired-state changes, or automatic promotion.
5. The node UI reports `Primary unreachable` and the age/version of the active snapshot.

### 5. Promote B to primary

1. The owner selects **Promote to PRIMARY** on B.
2. B warns that the former primary must be offline or isolated.
3. The owner confirms manual promotion.
4. B creates a new leadership generation and invalidates its former enrollment relationship.
5. B converts its synchronized snapshot into editable local desired state.
6. B enables the complete administration UI and primary services.
7. CoreDNS and Nginx continue using B's existing local ingress IP; no data-plane rewrite or restart is required unless validation detects drift.
8. Future configuration changes are made on B.

### 6. Rejoin the former primary

The former primary must not resume leadership automatically.

1. A starts and detects that its previous generation is stale or that it cannot prove current leadership.
2. A keeps its data plane on its last valid snapshot but blocks configuration writes.
3. The owner explicitly converts A to `NODE` and enrolls it against B.
4. A archives its stale desired state, downloads B's complete snapshot, validates it, and rejoins.

## Node-mode UI

A node hides ordinary resource editing but retains enough information for safe operation.

| Surface | Available in NODE mode |
| --- | --- |
| Local node IP | Editable with validation |
| Primary address | Editable through an explicit reconnect flow |
| Connection status | Visible |
| Primary identity | Visible |
| Last synchronization | Visible |
| Desired/applied snapshot version | Visible |
| CoreDNS health | Visible |
| Nginx health | Visible |
| Synchronization error | Visible and actionable |
| Synchronize now | Available |
| Promote to PRIMARY | Available with confirmation |
| Disconnect/reset | Available with destructive confirmation |
| DNS/proxy/user editing | Hidden and rejected by the API |

Hiding controls is not an authorization boundary. Node-mode mutation endpoints must reject requests server-side.

## Replication contract

### Replicate

The minimum promotable snapshot contains:

- managed zones and DNS records;
- resolver pools and suffix forwarding rules;
- proxied-record settings and explicit origins;
- Nginx path, header, protocol, timeout, Basic Auth, and stream configuration;
- active certificate chains and encrypted private keys;
- certificate metadata required for continuity;
- encrypted DNS-provider credentials required by retained certificate definitions;
- installation-wide functional settings;
- owner/operator identities required to administer the promoted node;
- schema and snapshot format versions;
- revision ID, content hash, source primary ID, and leadership generation.

### Do not replicate initially

- active browser sessions;
- transient locks or leases;
- in-flight and completed jobs;
- application logs;
- complete audit history;
- metrics and caches;
- updater transient state;
- PostgreSQL storage files.

Audit may record replicated actions locally, but historical audit replication is deferred.

### Localize per node

The following values must never be copied blindly from the primary:

- node ID;
- node role;
- node-local DNS address;
- node-local proxy ingress IPv4/IPv6;
- primary endpoint and enrollment credentials;
- local service health and applied timestamps;
- host paths and Compose/runtime settings.

The same logical snapshot therefore produces equivalent, not byte-identical, CoreDNS output on every node. In particular, proxied DNS answers use the applying node's ingress address.

## Snapshot lifecycle

```text
PRIMARY desired state
        │
        ▼
immutable versioned snapshot
        │
        ├── sign/authenticate + hash
        ▼
NODE candidate storage
        │
        ├── schema compatibility
        ├── integrity verification
        ├── decrypt/re-encrypt secrets
        ├── render node-local CoreDNS/Nginx
        ├── validate CoreDNS
        └── nginx -t + representative checks
        ▼
atomic promote and controlled reload
        │
        └── retain previous known-good snapshot for rollback
```

Nodes acknowledge only successfully applied versions. Failed candidates remain diagnostic artifacts within retention limits and never replace the active configuration.

## Security requirements

1. Enrollment requires a short-lived, single-use token created by an authenticated owner on the primary.
2. The node must verify and display the primary identity before replacing local state.
3. Replication uses authenticated and encrypted transport, even on the LAN.
4. Tokens, private keys, provider credentials, and plaintext secrets never appear in logs or ordinary API responses.
5. After enrollment, each node uses a revocable node credential rather than retaining the enrollment token.
6. A primary can revoke a node and reject future synchronization requests.
7. Snapshots include integrity protection and reject replay from an older leadership generation.
8. Promotion rotates or invalidates credentials that authorized the old primary relationship.
9. Secret replication must define one explicit model before implementation:
   - share a cluster key through enrollment; or
   - transport decrypted secrets over the authenticated channel and re-encrypt with the node-local key.
10. Promotion must leave the new primary able to decrypt every secret required by the active data plane.

## Consistency and failure policy

This feature uses eventual configuration consistency, not distributed transactions.

| Failure | Required behavior |
| --- | --- |
| Primary unavailable | Node serves last valid snapshot |
| Snapshot download interrupted | Active snapshot remains unchanged |
| Snapshot incompatible | Reject and report required software update |
| CoreDNS validation fails | Keep previous CoreDNS configuration |
| Nginx validation fails | Keep previous Nginx configuration |
| Certificate/secret unavailable | Reject the complete snapshot; do not partially apply |
| Node unavailable during primary change | Primary succeeds locally and reports node drift |
| Node rejoins | Fetch latest complete snapshot, not a chain of missed mutations |
| Former primary returns after promotion | Block writes until explicitly re-enrolled or intentionally recovered |
| Primary and promoted node both active | Report suspected split-brain; no automatic merge |

## Compatibility rules

- Snapshot formats are explicitly versioned.
- A node must reject formats it cannot safely interpret.
- The primary UI must show when a node must update before accepting new snapshots.
- Updates should normally upgrade nodes before the primary when a snapshot format changes.
- Schema/configuration evolution should use additive, backward-compatible phases where practical.
- Data-plane services must continue serving the previous snapshot throughout control-plane updates.
- ARM and x86 hosts must produce equivalent logical CoreDNS/Nginx behavior from the same snapshot.

## Functional requirements

### Role management

- **FR-1:** A fresh installation MUST operate as a standalone primary.
- **FR-2:** Only an Owner MAY convert an installation to `NODE` or promote it to `PRIMARY`.
- **FR-3:** A node MUST reject ordinary configuration mutations at the API boundary.
- **FR-4:** Role changes MUST be durable across container and host restarts.

### Enrollment

- **FR-5:** A primary MUST generate revocable, single-use enrollment tokens with expiration.
- **FR-6:** Joining MUST require explicit confirmation before replacing an independent installation's configuration.
- **FR-7:** The independent configuration MUST be archived for explicit recovery or deleted according to a documented retention policy.
- **FR-8:** A node MUST persist the verified identity of its primary.

### Synchronization

- **FR-9:** The primary MUST expose a complete, immutable, versioned snapshot.
- **FR-10:** A node MUST validate integrity, compatibility, secrets, CoreDNS, and Nginx before promotion.
- **FR-11:** A node MUST apply snapshots atomically and retain the previous known-good snapshot.
- **FR-12:** Reconnection MUST converge from a complete snapshot without replaying every missed mutation.
- **FR-13:** The primary MUST display each node's latest seen, downloaded, and applied versions.

### DNS and proxy continuity

- **FR-14:** Both primary and node MUST serve DNS independently.
- **FR-15:** Both primary and node MUST serve equivalent Nginx routes to the same configured origins.
- **FR-16:** Every node MUST answer proxied DNS records with its own configured ingress IP.
- **FR-17:** DNS-only records MUST retain their configured values on all nodes.
- **FR-18:** Loss of the primary MUST NOT remove or invalidate the node's active data-plane configuration.
- **FR-19:** Operational guidance MUST recommend advertising both node IPs as DNS resolvers through DHCP.
- **FR-20:** Proxied records SHOULD use a short configurable TTL, with a recommended default of 30–60 seconds for this topology.

### Promotion and rejoin

- **FR-21:** Promotion MUST preserve the active synchronized configuration.
- **FR-22:** Promotion MUST make retained configuration editable locally.
- **FR-23:** Promotion MUST NOT change the promoted node's ingress IP.
- **FR-24:** Promotion MUST create a new leadership generation and display a split-brain warning.
- **FR-25:** A former primary MUST NOT overwrite a promoted primary automatically.
- **FR-26:** Rejoining a former primary MUST require explicit node enrollment and full synchronization.

## Non-functional requirements

- Data-plane request handling must not depend on primary reachability.
- A node must fit the same low-resource target as a normal installation, including Raspberry Pi-class hosts with approximately 1 GB RAM.
- Synchronization must be resumable or safely restartable without corrupting the active snapshot.
- Snapshot size and retained candidates must be bounded.
- All applies must remain observable and reversible to the previous known-good snapshot.
- No arbitrary remote command execution may be introduced by replication.
- Builds remain executable through Docker Compose on the target device.

## Phased implementation

### Phase 0 — Contracts and safety boundaries

**Goal:** Make role and snapshot semantics explicit before introducing network replication.

Deliverables:

- role model: `PRIMARY` and `NODE`;
- durable installation ID, node ID, and leadership generation;
- versioned snapshot schema;
- replicated/local/transient field classification;
- secret transport decision;
- promotion and stale-primary state machine;
- compatibility and rollback rules;
- threat model for enrollment and snapshot delivery.

Exit criteria:

- [ ] A snapshot can represent all data required to reconstruct DNS and Nginx safely.
- [ ] Node-local ingress fields cannot be overwritten by imported state.
- [ ] The secret model supports promotion without the old primary.
- [ ] Split-brain behavior is documented and testable.

### Phase 1 — Local snapshot export/import

**Goal:** Prove that one installation can reproduce another installation's data plane without networking.

Deliverables:

- deterministic snapshot exporter;
- snapshot validator and importer;
- node-local rendering of proxied DNS answers;
- complete CoreDNS/Nginx validation;
- atomic apply and rollback;
- cross-architecture fixture tests where feasible.

Exit criteria:

- [ ] Export from A and import into B produces equivalent DNS-only answers.
- [ ] Proxied answers resolve to A on A and B on B.
- [ ] Both Nginx instances route to the same origins.
- [ ] Corrupt or incompatible snapshots never replace the active candidate.
- [ ] B serves the imported snapshot while A is offline.

### Phase 2 — Secure enrollment and pull synchronization

**Goal:** Convert an independent installation into a functioning node.

Deliverables:

- primary enrollment-token flow;
- node role settings UI;
- authenticated primary/node transport;
- full-snapshot pull and acknowledgement;
- periodic synchronization with bounded backoff;
- node revocation;
- archive/replace confirmation for prior standalone configuration.

Exit criteria:

- [ ] B can join A using a one-time token.
- [ ] B becomes read-only after successful enrollment.
- [ ] B converges after missing multiple primary changes.
- [ ] Revoked B can no longer download snapshots.
- [ ] Interrupted enrollment leaves B recoverable.

### Phase 3 — Node operations and primary visibility

**Goal:** Make degraded and drifting states understandable.

Deliverables:

- node-mode dashboard;
- primary node inventory;
- last-seen/downloaded/applied version status;
- CoreDNS and Nginx health reporting;
- manual synchronize action;
- explicit drift and compatibility diagnostics;
- operational runbook for DHCP DNS ordering and planned maintenance.

Exit criteria:

- [ ] Operators can distinguish offline, outdated, rejected, and healthy nodes.
- [ ] A node stays useful when primary health reporting is unavailable.
- [ ] A failed node apply is visible without affecting the primary apply.

### Phase 4 — Manual promotion

**Goal:** Promote a node without reconstructing configuration or interrupting its data plane.

Deliverables:

- promotion confirmation flow;
- leadership-generation increment;
- conversion of replicated state into editable desired state;
- credential invalidation/rotation;
- stale-primary startup guard;
- former-primary rejoin workflow;
- promotion and rejoin runbooks.

Exit criteria:

- [ ] B can be promoted while A is offline.
- [ ] B retains DNS answers, Nginx routes, certificates, and users.
- [ ] B's node-local ingress address remains unchanged.
- [ ] A cannot automatically overwrite B when it returns.
- [ ] A can explicitly rejoin B as a node and converge.

### Phase 5 — Hardening and lifecycle compatibility

**Goal:** Make the topology safe across upgrades and long-running operation.

Deliverables:

- snapshot compatibility matrix;
- node-first rolling update guidance;
- retention and cleanup;
- chaos/failure-path tests;
- low-memory build/runtime measurements;
- certificate renewal behavior during disconnection;
- recovery tooling for broken enrollment or lost credentials.

Exit criteria:

- [ ] Mixed-version behavior fails safely.
- [ ] Nodes continue serving during control-plane and updater restarts.
- [ ] Snapshot storage remains bounded.
- [ ] The complete topology can build and run through Compose on the target low-resource host.

## Acceptance scenarios

### Planned primary maintenance

```gherkin
Given A is PRIMARY and B is a synchronized NODE
And DHCP advertises A and B as DNS resolvers
When A is stopped for maintenance
Then B continues answering managed and forwarded DNS
And proxied names answered by B resolve to B's Nginx IP
And B's Nginx continues routing to the configured origins
And no connection to A is required for B's data plane
```

### Transparent promotion

```gherkin
Given B has the latest valid snapshot from A
And A is offline
When the Owner promotes B to PRIMARY
Then B keeps its current CoreDNS and Nginx configuration active
And B keeps its node-local ingress IP
And the synchronized DNS, proxy, certificate, and user configuration becomes editable
And B accepts future nodes
```

### Safe stale-primary return

```gherkin
Given B was promoted after A became unavailable
When A returns with an older leadership generation
Then A does not overwrite B
And A does not silently resume writable-primary operation
And the UI requires an explicit recovery or re-enrollment decision
```

### Rejected snapshot

```gherkin
Given B is serving snapshot 41
When B receives snapshot 42 with invalid Nginx configuration or unusable secrets
Then B rejects snapshot 42
And B continues serving snapshot 41
And A reports B as out of sync with the rejection reason
```

## Explicit non-goals

- automatic leader election;
- consensus, quorum, or a witness node;
- automatic failback;
- conflict merging between two writable primaries;
- zero-second failover guarantees for every DNS client;
- virtual IP management;
- PostgreSQL physical or logical replication;
- full audit/log/job replication;
- active-session replication;
- WAN or Internet-facing cluster discovery;
- replacing PostgreSQL with SQLite as part of this work.

## Product risks

| Risk | Mitigation |
| --- | --- |
| Client caches the former node's DNS answer | Short proxied TTL, planned-maintenance drain procedure, clear non-zero failover expectation |
| Both installations become writable | Manual promotion warning, leadership generation, stale-primary startup guard |
| Secret cannot be decrypted after promotion | Resolve secret transport/key model in Phase 0 and test promotion offline |
| Node silently drifts | Version/hash acknowledgement and visible node status |
| Bad snapshot breaks both services | Candidate validation, atomic promotion, previous-snapshot rollback |
| Independent B configuration is lost on join | Archive plus explicit destructive confirmation |
| Version mismatch blocks synchronization | Versioned snapshots, compatibility status, node-first upgrade guidance |
| Primary apply waits on a failed node | Asynchronous node convergence; primary success remains independent |

## Success metrics

- A synchronized node continues DNS and proxy service for at least 24 hours with the primary offline.
- Planned maintenance requires no DNS/proxy reconfiguration on the node.
- Promotion requires one explicit owner workflow and no resource-by-resource recreation.
- Every node reports the exact snapshot version currently applied.
- Invalid snapshots cause zero active-data-plane replacements.
- A returning stale primary performs zero automatic writes to the promoted primary.

## Open decisions before implementation

1. Choose shared cluster-key versus node-local re-encryption for replicated secrets.
2. Choose primary-push versus node-pull transport; node-pull is preferred because it requires no inbound management endpoint on the node beyond its existing UI/API.
3. Define whether administrator password hashes are replicated or whether every node retains an emergency local Owner credential.
4. Define certificate renewal ownership while disconnected and after promotion.
5. Define the standalone-configuration archive retention period after joining.
6. Define whether NODE mode keeps PostgreSQL temporarily or uses a reduced local persistence model before the separate SQLite initiative.

## Review checklist

- [ ] The design provides maintenance continuity without claiming full HA.
- [ ] Both DNS servers use node-local ingress addresses for proxied records.
- [ ] Nginx configurations, certificates, auth material, and origins are equivalent across nodes.
- [ ] Node operation never depends on live access to the primary.
- [ ] Promotion preserves the already-running data plane.
- [ ] Node UI restrictions are enforced by the API.
- [ ] Secret replication supports offline promotion.
- [ ] Former-primary return cannot silently create or overwrite state.
- [ ] PostgreSQL replication and SQLite migration remain outside scope.
