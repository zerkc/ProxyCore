# ProxyCore Functional Requirements

Observable behavior for the reduced MVP. Later integrations are marked NEXT/LATER so a UI field alone does not count as done.

## Convention

- **MVP:** required for the first release.
- **NEXT:** designed or sketched, implemented after MVP.
- **LATER:** outside the current boundary.

## Actors

| Actor | Description |
| --- | --- |
| Owner | Installation admin: users, secrets, all resources, apply/rollback |
| Operator | Manages DNS and ingress and requests applies |
| Configuration worker | Renders, validates, applies, verifies CoreDNS/Nginx config |

## Authentication and users

| ID | Requirement |
| --- | --- |
| FR-AUTH-001 | One-time bootstrap MUST create the first local Owner. |
| FR-AUTH-002 | The system MUST support local username/password auth with secure password hashing, secure HttpOnly session cookies, and session revocation. |
| FR-AUTH-003 | Owner MUST be able to create, update, disable, and delete users (CRUD) and assign Owner or Operator. |
| FR-AUTH-004 | The last active Owner MUST NOT be removable or demotable without leaving another Owner. |
| FR-AUTH-005 | Every mutating action MUST be authorized server-side. |
| FR-AUTH-006 | Login, logout, failed auth, user changes, and authorization failures MUST be audited. |

### Deferred (NEXT)

OIDC, TOTP/MFA, recovery-code packs, progressive lockout policies, Auditor role, resource-level RBAC.

### Acceptance

- Fresh install requires bootstrap before administration.
- Operator cannot manage users or secrets.
- Revoked sessions cannot continue authenticated requests.

## Zones and DNS records

| ID | Requirement |
| --- | --- |
| FR-DNS-001 | Owner/Operator MUST CRUD internal zones (create, edit, disable, delete). |
| FR-DNS-002 | MVP record types MUST be A, AAAA, CNAME, TXT, MX, and SRV, with type-specific validation. Unsupported types MUST be rejected. |
| FR-DNS-003 | Records MUST support TTL (default 300; min 30; max 86400), enabled/disabled, comments, and audit history. |
| FR-DNS-004 | Names, values, TTL, type constraints, CNAME conflicts, zone containment, and proxied-record conflicts MUST be validated before save. |
| FR-DNS-005 | Wildcard records MUST follow DNS label rules and be shown distinctly. |
| FR-DNS-006 | UI/API MUST distinguish desired records from records currently served by CoreDNS. |
| FR-DNS-007 | CoreDNS MUST serve managed zones authoritatively. |
| FR-DNS-008 | Because ProxyCore is the sole homelab DNS server, queries outside managed zones MUST be forwarded to configured upstream DNS resolvers. |
| FR-DNS-009 | The MVP MUST support one default resolver pool (ordered DNS IP:port endpoints with sequential fallback) and optional normalized suffix-specific forward rules (most-specific wins). |
| FR-DNS-010 | Exhausting a resolver pool MUST surface a visible DNS health/error; it MUST NOT invent managed answers. |
| FR-DNS-011 | CoreDNS candidates MUST be rendered from a versioned snapshot (one zone file + `file` block per managed zone; root `forward` from configured pools/rules) and validated before promotion. |
| FR-DNS-012 | Failed CoreDNS validation/reload MUST leave the last known-good config active when the service permits it. |
| FR-DNS-013 | The MVP MUST NOT mirror records or policies to/from Pi-hole/AdGuard; those systems are plain forwarder endpoints when configured. |
| FR-DNS-014 | A, AAAA, and CNAME records MUST expose a Cloudflare-style `proxied` toggle. TXT, MX, and SRV MUST NOT. |
| FR-DNS-015 | When `proxied=false`, CoreDNS MUST answer with the record value as written and MUST NOT create Nginx HTTP config from that record. |
| FR-DNS-016 | When `proxied=true`, CoreDNS MUST answer with the installation proxy ingress address(es) for that name (not the origin), and Nginx HTTP config MUST be derived from the record’s proxy settings. |
| FR-DNS-017 | Enabling proxy without complete proxy settings, or without a configured installation ingress address for the needed address family, MUST be rejected before apply. |
| FR-DNS-018 | Disabling proxy MUST restore DNS-only answers and remove or disable the derived Nginx HTTP config for that hostname on apply. |
| FR-DNS-019 | At most one A, AAAA, or CNAME record for a canonical hostname MAY have `proxied=true` in the MVP. Enabling proxy on a second record for that name MUST be rejected until the existing proxied record is disabled. |
| FR-DNS-020 | Saving a zone or DNS record MUST persist the desired state and immediately enqueue one combined CoreDNS/Nginx apply in the same transaction; the mutation response MUST expose the queued job, and consecutive saves MUST NOT be implicitly debounced. |
| FR-DNS-021 | Proxied DNS answers MUST use the installation's advertised LAN/public address, automatically initialized when detectable and overridable from installation settings; they MUST NOT use the internal Docker container address. |

### Deferred (NEXT/LATER)

Reverse zones, PTR, NS, CAA, DNSSEC, Pi-hole/AdGuard product integrations.

### Acceptance

- Managed zone records resolve through CoreDNS.
- A name outside managed zones is answered via the default (or matching suffix) forwarder.
- Invalid records never replace the active CoreDNS revision.
- Wildcard behavior follows DNS semantics.
- A proxied A/AAAA name resolves to the ProxyCore ingress address; the same name DNS-only resolves to the configured origin/value.
- Multiple records for one hostname may remain DNS-only, but only one A/AAAA/CNAME record may be proxied at a time.
- Toggling proxy on a TXT record is rejected.
- Saving a valid zone or record returns queued apply metadata and the worker promotes it without a separate manual apply request.
- A proxied name resolves to the detected or explicitly configured host address, never the `172.x` address of the Nginx/CoreDNS container.

## Upstreams and proxied records

HTTP/HTTPS ingress is Cloudflare-style: configured on proxied DNS records. There is no separate top-level “HTTP route” product entity in the MVP.

### Upstreams / origin

| ID | Requirement |
| --- | --- |
| FR-PROXY-001 | An origin/upstream MUST be a literal IPv4/IPv6 address, port, and protocol. Hostnames are out of MVP. |
| FR-PROXY-002 | Docker socket discovery MUST NOT be required. |
| FR-PROXY-003 | Syntactically valid targets MUST NOT be rejected solely for being private, loopback, or Docker-network addresses. Unreachable targets produce visible health/request errors. |
| FR-PROXY-004 | For proxied A/AAAA records, the default origin IP is the record address; port and protocol come from proxy settings. |
| FR-PROXY-005 | For proxied CNAME records, an explicit literal origin IP:port MUST be provided in proxy settings. |
| FR-PROXY-006 | The installation MUST define proxy ingress IPv4/IPv6 address(es) used as CoreDNS answers for proxied names. |

### Proxy settings on proxied A/AAAA/CNAME

| ID | Requirement |
| --- | --- |
| FR-HTTP-001 | Operator MUST enable proxy on an A, AAAA, or CNAME record and configure its proxy settings (origin, TLS, paths, etc.). |
| FR-HTTP-002 | Proxy settings MUST declare TLS on/off (HTTPS on by default), certificate reference when TLS is on, redirect behavior, and optional path policy. |
| FR-HTTP-003 | Path rules MUST use exact or literal-prefix matches only. Exact matches take precedence over prefixes, then the longest prefix wins; duplicate or ambiguous patterns MUST be rejected. Rules MAY redirect with status 301, 302, 307, or 308, or proxy to the record origin with an optional path rewrite. Regular expressions and configuration blocks remain out of scope. |
| FR-HTTP-004 | WebSocket forwarding MUST be supported without hand-edited Nginx config. |
| FR-HTTP-005 | Safe default proxy headers MUST be set; operators MAY add validated server-level Nginx directives from the record dialog to override generated directive values or add directives such as `add_header`. |
| FR-HTTP-006 | HTTP Basic Auth MUST be available and MUST require TLS on the client-facing side. |
| FR-HTTP-007 | HTTP/2 MUST be configurable per proxied record (sensible default on for TLS). |
| FR-HTTP-008 | HTTP/3 (QUIC) MUST be configurable per proxied record (default off unless explicitly enabled). Enabling it MUST be rejected unless the selected Nginx binary exposes HTTP/3 support and the deployment publishes both TCP/443 and UDP/443. |
| FR-HTTP-009 | Cache MUST be off by default; opt-in only for public GET/HEAD without auth/cookies/WebSocket. |
| FR-HTTP-010 | Normal defaults: connect 5s, send/read 60s, client header/body 15s, body 10 MB. Long-lived/WebSocket profiles are explicit opt-in. |
| FR-HTTP-011 | Nginx candidates derived from proxied records MUST pass `nginx -t` and representative checks before reload. |
| FR-HTTP-012 | The UI MUST present proxy as a record-level control (Cloudflare-style), not as a disconnected route list as the primary workflow. |
| FR-HTTP-013 | Creating a DNS record through the UI MUST use a configuration dialog that asks whether client traffic is HTTP or HTTPS; HTTPS MUST present configured active certificates for selection and reject saving without a certificate. |
| FR-HTTP-014 | The record dialog MUST support create and edit, and when proxy is enabled MUST expose origin IP/port/protocol, client TLS, Nginx knobs (HTTP/2, HTTP/3, WebSocket, cache, backend TLS verify, timeouts), server-level Nginx directives, HTTP→HTTPS redirect, typed path redirects/rewrites, and Basic Auth in separate configuration tabs. |
| FR-HTTP-015 | Basic Auth MUST accept a username and password in the dialog, store only an encrypted password hash referenced by `passwordSecretId`, and materialize a versioned htpasswd file into the Nginx candidate. Editing MAY omit the password to keep the existing secret; plaintext passwords MUST never appear in snapshots, API responses, or rendered non-secret config. |
| FR-HTTP-016 | Custom Nginx input MUST be normalized, length-limited, and restricted to server-level directive lines. It MUST reject Nginx block braces and top-level `events`, `http`, `server`, `stream`, or `location` directives before a revision is created. |

### TCP/UDP streams

| ID | Requirement |
| --- | --- |
| FR-STREAM-001 | Operator MUST define TCP or UDP listeners mapped to explicit upstreams. |
| FR-STREAM-002 | Listener conflicts (address/port/protocol) MUST be rejected before apply. |
| FR-STREAM-003 | Any syntactically valid UDP IP/port is accepted; no application-protocol allowlist. |

### Acceptance

- Enabling proxy on an A record serves the ingress IP via DNS and proxies HTTP(S) to the origin.
- HTTPS works with optional HTTP/2; HTTP/3 works when enabled and network allows it.
- HTTP/3 candidates are rejected when the Nginx binary lacks HTTP/3 support or TCP/UDP 443 is not available.
- HTTPS can be disabled per proxied record when intentionally configured.
- Basic Auth over cleartext is rejected.
- Bad Nginx candidates leave the prior revision active.
- The DNS record dialog requires a certificate selection for HTTPS proxying and does not require one for HTTP proxying.
- Proxied records can set an explicit upstream port/protocol and path redirects from the dialog.
- Basic Auth stores only an encrypted hash reference; the worker materializes the htpasswd file for Nginx.

## Certificates and TLS

| ID | Requirement |
| --- | --- |
| FR-TLS-001 | Certificate model MUST track hostnames, issuer, challenge/type, status, expiry, renewal, and private-key reference. |
| FR-TLS-002 | MVP MUST support self-signed certificate generation and installation for proxied HTTP/HTTPS records. |
| FR-TLS-003 | MVP MUST support Let's Encrypt HTTP-01 for publicly reachable names. |
| FR-TLS-004 | MVP MUST support Let's Encrypt DNS-01 via the scoped Cloudflare adapter (`_acme-challenge` only). |
| FR-TLS-005 | Staging issuance MUST be available for Let's Encrypt tests. |
| FR-TLS-006 | Renewal MUST NOT replace a working cert with an unverified candidate. |
| FR-TLS-007 | Private keys MUST be encrypted at rest and never returned in ordinary API/log output. |
| FR-TLS-008 | Client-facing certs apply only to proxied HTTP/HTTPS records; DNS-only records do not imply ProxyCore TLS termination. |
| FR-TLS-009 | HTTPS upstream certificate verification MUST be an explicit per-record setting. It is off by default for homelab origins in the MVP; enabling it uses the system trust store. Client-facing validation remains separate. |

### Acceptance

- Self-signed route serves TLS with the generated cert.
- HTTP-01 and Cloudflare DNS-01 staging issuance succeed in the test environment.
- Failed renewal keeps the previous valid certificate.

## Configuration worker

| ID | Requirement |
| --- | --- |
| FR-JOB-001 | Mutations affecting CoreDNS or Nginx MUST create an identifiable job. |
| FR-JOB-002 | A job MUST record revision, actor, target, timestamps, validation/apply/health output, and final status. |
| FR-JOB-003 | Worker MUST render from a consistent snapshot, validate, then reload through a least-privileged path. |
| FR-JOB-004 | The Next.js web process MUST NOT execute privileged reload commands or hold the Docker socket. |
| FR-JOB-005 | Concurrent jobs for the same service MUST be serialized or safely coalesced. |
| FR-JOB-006 | Health failure after reload MUST trigger documented rollback or an explicit recovery action. |
| FR-JOB-007 | Worker-to-service control MUST go through a private Unix socket to the service-control helper and use fixed operations for staging, validation, promotion, reload, health, and rollback. The worker MUST NOT receive a raw Docker socket, user input MUST NOT become shell commands, and custom Nginx input MUST pass server-context validation before rendering. |

Dual per-service control sidecars are NEXT; the MVP worker may own staging + reload directly with constrained volume/socket access.

## Audit, health, observability

| ID | Requirement |
| --- | --- |
| FR-OPS-001 | Mutating actions MUST audit actor, resource, redacted before/after, timestamp, correlation id, result. |
| FR-OPS-002 | UI MUST show service health, last successful apply, current revision, queued/failed jobs, and certificate expiry risk. |
| FR-OPS-003 | Health MUST distinguish app, worker, CoreDNS, Nginx, upstream, and DNS forwarder health. |
| FR-OPS-004 | MVP MUST emit structured operational logs. Prometheus and webhook alert delivery are NEXT. |
| FR-OPS-005 | Operational logs, audit events, apply-job output, and rendered artifacts MUST follow configurable retention limits: default maximum age 7 days or maximum size 50 MB, with cleanup when either limit is reached. Current desired/applied state and active certificates MUST NOT be removed by retention cleanup. |

## Data / backup

Backup/restore is LATER/post-MVP. Development/staging migrations may be automatic; production-like migrations need preflight and an explicit Owner-acknowledged recovery path.

## External DNS provider (Cloudflare DNS-01 only)

| ID | Requirement |
| --- | --- |
| FR-PROVIDER-001 | Provider support uses an adapter interface. |
| FR-PROVIDER-002 | Cloudflare MVP adapter creates/observes/removes only `_acme-challenge` TXT records for certificate jobs. |
| FR-PROVIDER-003 | Credentials are encrypted and minimally scoped. |
| FR-PROVIDER-004 | Provider outage MUST NOT erase local desired state or break unrelated internal DNS/Nginx applies. |
| FR-PROVIDER-005 | Ordinary Cloudflare zone sync and Tunnel management are out of MVP. |

## Cross-cutting scenarios

| Scenario | Expected result |
| --- | --- |
| Invalid DNS record | Rejected before apply; CoreDNS unchanged |
| Unmanaged DNS name | Forwarded via default/suffix pool |
| Proxied without origin/ingress | Rejected before apply |
| Conflicting proxied hostname/path | Explained; Nginx unchanged |
| Worker down | Desired state visible; no false applied success |
| Reload failure | Prior revision remains or is restored |
| Cert renewal failure | Current cert stays active; risk visible |
| Unauthorized API call | Rejected and audited |

## Definition of done

MVP is complete when the MVP requirements and scenarios pass in staging. Storing config without validate/apply/observe/recover does not count as complete.
