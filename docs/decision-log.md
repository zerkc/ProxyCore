# ProxyCore Decision Log

This log records product decisions. It is intentionally separate from implementation details so that a later code change cannot silently redefine the product boundary.

## Confirmed decisions

| ID | Decision | Rationale |
| --- | --- | --- |
| DEC-001 | ProxyCore is a single-homelab control plane for internal DNS and ingress. | Production-like safety without starting as a multi-tenant platform. |
| DEC-002 | The first deployment target is Docker Compose on one Linux host. | Matches the homelab operating model. |
| DEC-003 | The application stack is Next.js, TypeScript, App Router, and Tailwind. | One modern web runtime for UI and control-plane API. |
| DEC-004 | PostgreSQL with Drizzle ORM is the durable application store. | Desired state, identity, jobs, and audit need transactional persistence. |
| DEC-005 | CoreDNS is authoritative for managed zones and forwards unmanaged queries to configured resolvers. | ProxyCore is the sole homelab DNS server; non-managed names must still resolve. |
| DEC-006 | Public zones remain with external DNS providers; ProxyCore is not a public authoritative nameserver in the first release. | Public authority is outside the first control-plane scope. |
| DEC-007 | Ordinary public DNS provider synchronization is deferred. | Keeps public DNS ownership external. |
| DEC-008 | Cloudflare is the first DNS-01 challenge adapter only; general Cloudflare record sync is deferred. | Supports Let's Encrypt DNS-01 without provider-wide zone ownership. |
| DEC-009 | Nginx manages HTTP/HTTPS and TCP/UDP stream forwarding. | One explicit ingress data plane. |
| DEC-010 | Upstreams are explicit literal IP/port/protocol; Docker socket discovery is excluded. | Explicit targets reduce privilege and are enough for a homelab operator. |
| DEC-011 | A dedicated worker renders, validates, applies, and verifies Nginx/CoreDNS changes. | The Next.js request process must not control data-plane services. |
| DEC-012 | MVP authentication is local username/password with user CRUD and bootstrap of the first Owner. OIDC and MFA are deferred. | Matches the product ask; identity federation can come later. |
| DEC-013 | **Superseded by DEC-040:** One installation was modeled as one workspace with simple roles: Owner and Operator. | The original decision established a single installation, but the Workspace/Membership abstraction was removed from the MVP. |
| DEC-014 | Host firewall, NAT, and fail2ban are not managed. | Avoid host-wide privilege in the first release. |
| DEC-015 | Naming strategy is a real domain with split-DNS where useful. | Supports valid public certificates when needed. |
| DEC-016 | Certificate issuers in the MVP are self-signed, Let's Encrypt HTTP-01, and Let's Encrypt DNS-01 via Cloudflare. | Covers offline/internal names and public challenge workflows. |
| DEC-017 | Cloudflare Tunnel, when used, is operated outside ProxyCore. | Tunnel lifecycle is an external deployment concern. |
| DEC-018 | Migrations are automatic in development/staging and gated in production-like use with an explicit recovery path. Backup-based recovery is post-MVP. | Fast iteration early; explicit risk later. |
| DEC-019 | Reliability target is one host with validate-before-reload, health checks, and configuration rollback. High availability and backup-based host-loss recovery are out of MVP. | Matches a single Compose host. |
| DEC-020 | Route protection uses HTTP Basic Auth over TLS. | Simple and sufficient for the first “blocking” interpretation. |
| DEC-021 | Development, staging, and production-like environments stay separate. | Real credentials must not validate development behavior. |
| DEC-022 | Application baseline: Node.js `24.19.0`, pnpm `11.16.0`, Next.js `16.3.0`, React/React DOM `19.2.8`, TypeScript `7.0.2`, Tailwind CSS `4.3.3`, `@tailwindcss/postcss` `4.3.3`, Drizzle ORM `0.45.2`, Drizzle Kit `0.31.10`. | Stable pins as of 2026-08-07. |
| DEC-023 | MVP DNS types: A, AAAA, CNAME, TXT, MX, SRV. TTL default 300s (bounds 30–86400). Reverse zones, PTR, NS, CAA, and DNSSEC are deferred. | Covers the Cloudflare-like zone use case without reverse/DNSSEC complexity. |
| DEC-024 | CoreDNS uses one RFC 1035 zone file per managed zone with an explicit `file` block, plus `forward`, `cache`, `errors`, `health`, `ready`, `loop`, and `reload`. Candidates are validated (`coredns` startup + representative `dig` queries) before promotion. | Clear ownership and safe apply without a separate CoreDNS control sidecar. |
| DEC-025 | Use official `nginx:1.30.4-alpine-slim` (digest-pinned at scaffold). Require HTTP TLS/proxy/auth and compiled stream TCP/UDP. Apply via the worker after `nginx -t` and controlled reload; no custom modules and no Docker socket on the web process. HTTP/3 support is governed by DEC-044. | Stable image with the needed capabilities and a separated control path. |
| DEC-026 | The worker owns candidate staging, validation, and apply orchestration for Nginx and CoreDNS. The service-control helper executes fixed reload operations; dual per-service control sidecars are deferred. The web process still has no service-control privileges. | Same least-privilege goal with fewer moving parts for a single-host MVP. |
| DEC-027 | Secrets and certificate private keys are encrypted at rest with a master key kept outside PostgreSQL. Plaintext never appears in API responses, logs, or rendered non-secret config. Elaborate envelope/KEK-compromise runbooks stay as implementation detail, not MVP product scope. | Protect credentials without identity/ops platform overhead. |
| DEC-028 | OIDC, TOTP MFA, recovery-code packs, and progressive lockout policies are deferred. MVP uses secure password hashing, session cookies, and Owner-managed user CRUD. | Avoid identity overengineering relative to the product ask. |
| DEC-029 | OIDC provider contracts (Authentik, embedded issuer, groups mapping) are deferred with OIDC itself. | No half-specified federation surface in the MVP. |
| DEC-030 | Proxy defaults on proxied records: HTTPS on by default (can be disabled); HTTP/2 and HTTP/3 configurable; cache off unless opted in; Basic Auth only over TLS; safe default headers with narrow overrides; normal timeouts connect 5s / send-read 60s / client 15s / body 10 MB. Trusted proxy CIDRs are explicit when an external tunnel is used. | Matches the requested proxy knobs without enterprise policy surface. |
| DEC-031 | Origins/upstreams are literal IPv4/IPv6 + port + protocol. No hostname upstreams. No CIDR allow/deny of private ranges. HTTPS upstream certificate verification follows DEC-045. ProxyCore issues client-facing certs only for proxied HTTP/HTTPS records. | Homelab reachability over destination policy. |
| DEC-032 | Backup/restore is optional and post-MVP. | Do not promise host-loss recovery in the first release. |
| DEC-033 | MVP observability is structured logs plus in-app job/health/certificate status. No Prometheus endpoint and no webhook alert system in the MVP. | Enough to operate without an observability product. |
| DEC-034 | Cloudflare in MVP is limited to `_acme-challenge` TXT create/observe/delete for DNS-01. No ordinary record sync, drift, or Tunnel management. | Minimal provider coupling for certificates. |
| DEC-035 | Because ProxyCore is the sole homelab DNS server, unmanaged queries MUST be forwarded. The MVP supports one default resolver pool (ordered DNS endpoints with sequential fallback) and optional suffix-specific forward rules (most-specific wins). Managed zones stay authoritative. Endpoints are plain DNS IP:port targets (Pi-hole/AdGuard/public); no product integration or policy mirroring. | Required for complete-homelab DNS. |
| DEC-036 | TCP/UDP stream routes accept any explicit literal IP/port. No UDP application-protocol catalog. | Generic homelab forwarding. |
| DEC-037 | Post-MVP backup remains disabled until destination, key, schedule, retention, and one isolated restore succeed. | Unchanged future policy. |
| DEC-038 | MVP scope is deliberately reduced: local users, internal DNS + forwarding, Nginx proxy (including HTTP/2/HTTP/3), self-signed and Let's Encrypt certificates, worker apply/rollback. Deferred: OIDC, MFA, dual control sidecars, webhook alerts, reverse DNS, public DNS sync, backup, resource-level RBAC. | User chose to remove overengineering while keeping sole-DNS forwarding. |
| DEC-039 | HTTP/HTTPS proxy follows a Cloudflare-style model: A, AAAA, and CNAME records have an explicit proxied toggle. DNS-only records are served as written by CoreDNS. Proxied records resolve to configured installation ingress addresses and create/update the matching Nginx HTTP configuration from proxy settings on that record (upstream/origin, TLS, headers, typed path policy, cert, HTTP/2, HTTP/3, Basic Auth, etc.). TXT/MX/SRV have no proxy toggle. TCP/UDP streams remain separate resources. | Matches the Cloudflare DNS-zone mental model requested for the product. |
| DEC-040 | ProxyCore has one installation scope. User roles, zones, forwarding configuration, ingress addresses, upstreams, revisions, and jobs are installation-wide; users store Owner/Operator directly and Workspace/Membership is not modeled. | Removes tenant abstractions that provide no MVP behavior for a single homelab. |
| DEC-041 | At most one A, AAAA, or CNAME record for a canonical hostname may be proxied in the MVP. Other records for that name may remain DNS-only. | Prevents duplicate derived Nginx virtual hosts and ambiguous default origins without adding multi-origin routing. |
| DEC-042 | Path policies use exact or literal-prefix matches. Exact wins over prefixes, then longest prefix wins; duplicate patterns are rejected. Rules support 301/302/307/308 redirects or proxying to the record origin with an optional path rewrite; regexes and Nginx configuration blocks are excluded. | Covers path redirection/routing with deterministic, typed configuration. |
| DEC-043 | **Superseded by DEC-047:** The worker-to-service control contract is limited to staging, validation, promotion, reload, health, and rollback. Transport was left as a constrained local implementation detail; user input cannot become shell commands or unvalidated Nginx configuration blocks. | The contract remains valid, but the transport is now explicitly selected. |
| DEC-044 | HTTP/3/QUIC is enabled only when the selected Nginx binary exposes HTTP/3 support and the deployment publishes both TCP/443 and UDP/443. | Makes the HTTP/3 requirement testable in the pinned Compose deployment. |
| DEC-045 | HTTPS upstream verification is an explicit per-record setting, off by default for homelab origins in the MVP, and uses the system trust store when enabled. | Supports common self-signed/private homelab origins while making insecure backend TLS an intentional, visible choice. |
| DEC-046 | Operational logs, audit events, apply-job output, and rendered artifacts use configurable retention with a default maximum age of 7 days or maximum size of 50 MB; cleanup runs when either limit is reached. Current desired/applied state and active certificates are retained. | Keeps the homelab footprint bounded without losing the live configuration needed for operation. |
| DEC-047 | A single service-control helper receives fixed operations over a private Unix socket. The worker writes and validates candidates but has no raw Docker socket; the helper controls only the named CoreDNS/Nginx services and cannot execute arbitrary commands. | Balances least privilege and MVP simplicity without adding two per-service control sidecars. |
| DEC-048 | Proxied DNS answers use the installation's advertised LAN/public address. ProxyCore detects a non-loopback LAN address on native startup or initializes it from a literal LAN host used for the first authenticated Compose request; the persisted value remains editable in installation settings and Docker container addresses are never used. | Removes unnecessary per-record ingress setup while preserving correct routing for LAN, NAT, and public deployments. |
| DEC-049 | The Record dialog is organized into tabs and includes an optional server-level Nginx directives textarea. The input is normalized and bounded, rejects braces and top-level context directives, and matching generated directive names are omitted so operators can override values such as `client_max_body_size`. | Provides direct control for advanced Nginx settings without allowing configuration blocks or requiring a new UI field for every directive. |

## Superseded earlier emphasis

Earlier design sessions expanded identity (OIDC/MFA), dual control sidecars, webhook alerts, and reverse-zone/DNSSEC boundaries. DEC-038 supersedes those as MVP requirements. The deferred items may return as post-MVP phases without rewriting the core DNS/proxy model.

An earlier open modeling question treated HTTP routes as a primary standalone entity. DEC-039 makes the DNS record the primary UX; Nginx HTTP config is derived from proxied records rather than managed as a separate top-level “route” product concept.

DEC-040 supersedes the earlier Workspace/Membership modeling in DEC-013. Installation-wide settings remain persisted configuration, not a multi-tenant boundary.

## Decision rules for implementation

- A phase cannot silently expand a deferred decision into MVP scope.
- Security-boundary changes require a new recorded decision.
- Product requirements take precedence over convenience of a particular image or library.
- Every recovery-affecting decision needs a test or runbook.
- Provider-specific work must stay behind an adapter boundary.

## Review checklist

- [ ] Product and non-goals match the intended homelab use.
- [ ] Unmanaged DNS forwarding is first-class and tested.
- [ ] Cloudflare-style proxy-on-record (DEC-039) is reflected in FR/domain/architecture.
- [ ] Single-installation roles and settings (DEC-040) are reflected without Workspace/Membership tables.
- [ ] Duplicate proxied hostnames (DEC-041) are rejected.
- [ ] Path precedence and allowed actions (DEC-042) are deterministic.
- [ ] Self-signed, HTTP-01, and Cloudflare DNS-01 are not conflated.
- [ ] The web process cannot reload Nginx/CoreDNS.
- [ ] The worker reaches Nginx/CoreDNS only through the private service-control helper (DEC-047).
- [ ] HTTP/3 is gated by binary support and TCP/UDP 443 (DEC-044).
- [ ] Upstream TLS verification behavior is explicit (DEC-045).
- [ ] Retention cleanup honors the configured age/size limits without deleting current state (DEC-046).
- [ ] OIDC/MFA/sidecars/webhooks are not accidental MVP commitments.
- [ ] Open decisions have owners before schema or service code is written.
