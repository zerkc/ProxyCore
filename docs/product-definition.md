# ProxyCore Product Definition

ProxyCore is a single-installation homelab control plane for local DNS and secure ingress. It combines a Next.js admin UI with PostgreSQL, CoreDNS, Nginx, and a configuration worker so operators can manage zones, records, certificates, and users without hand-editing service config.

The DNS experience is **Cloudflare-style**: manage a zone, add records, and toggle proxy on A/AAAA/CNAME. ProxyCore is also the **sole DNS server** on the homelab network: managed zones are authoritative, and every other query is forwarded to configured upstream resolvers.

## Quick path

1. Bootstrap the first Owner and create operators as needed.
2. Set installation proxy ingress address(es) (the IPs clients should hit for proxied names).
3. Create an internal zone and add records (including wildcards).
4. Configure default DNS forwarders for names outside managed zones.
5. On an A/AAAA/CNAME, enable proxy (Cloudflare-style), set origin port/protocol and TLS/certificate options.
6. Apply through the worker; verify DNS + HTTP; roll back if needed.

## Product boundary

| Plane | Responsibility |
| --- | --- |
| Control plane | Local auth, desired configuration, validation, certificates, audit, apply jobs. |
| Data plane | CoreDNS answers/forwards DNS; Nginx terminates and forwards traffic for proxied records and streams. |

The first release does **not**:

- operate public authoritative DNS on the Internet;
- manage Cloudflare Tunnel lifecycle or ordinary Cloudflare zone records;
- manage host firewall, NAT, fail2ban, or Docker discovery;
- provide OIDC, MFA, multi-tenant isolation, or high availability;
- promise backup-based host-loss recovery.

There is one installation boundary in the MVP. Users have an Owner or Operator role directly; Workspace/Membership and tenant isolation are not part of the product model.

## Product goals

1. **Cloudflare-like DNS UX** — zone, records, wildcards, and a proxy toggle on A/AAAA/CNAME.
2. **Understandable ingress** — proxied hostname, origin, certificate, and settings are visible on the record.
3. **Safe apply** — reject bad config; validate before reload; keep last good revision.
4. **Complete local DNS** — managed zones plus forwarding for everything else.
5. **Practical certificates** — self-signed for internal, Let's Encrypt when publicly challengable.
6. **Simple operators** — local users and roles without an identity platform.

## Personas

| Persona | Need | Permissions |
| --- | --- | --- |
| Owner | Bootstrap and administer the install | Full admin, users, secrets, apply/rollback |
| Operator | Manage DNS and ingress | Zones, records, proxy settings, streams, certificates, apply within policy |

## Primary journeys

### Bootstrap and users

1. Owner completes one-time bootstrap.
2. Owner sets proxy ingress addresses and creates users (Owner/Operator).
3. Users sign in with local username/password; sessions are revocable.

### Manage DNS (including proxy toggle)

1. Create an internal zone and typed records (A, AAAA, CNAME, TXT, MX, SRV), including wildcards.
2. Leave a record DNS-only, or enable proxy on A/AAAA/CNAME and configure origin + TLS/path/headers/HTTP2/HTTP3/Basic Auth.
3. Configure the default resolver pool (and optional suffix rules) for unmanaged queries.
4. Worker renders/validates/reloads CoreDNS and, for proxied records, Nginx.
5. UI shows desired vs applied state for DNS and proxy.

### DNS-only vs proxied

| Mode | What clients resolve | What serves HTTP |
| --- | --- | --- |
| DNS-only | The record value (origin/target) | Nothing from ProxyCore for that name |
| Proxied | ProxyCore ingress address(es) | Nginx → configured origin |

### Path policy

Path behavior is a typed policy on each proxied record:

- matches are exact paths or literal path prefixes; regular expressions are out of scope;
- exact matches win over prefixes, then the longest prefix wins;
- duplicate or ambiguous patterns are rejected before apply;
- a rule may redirect with status 301, 302, 307, or 308, or proxy to the record's configured origin with an optional path rewrite;
- arbitrary Nginx directives are not accepted.

### TCP/UDP streams

Stream listeners are a separate journey from the DNS proxy toggle (HTTP/HTTPS only on records).

### Certificates

1. On a proxied record with TLS enabled, choose self-signed, Let's Encrypt HTTP-01, or Let's Encrypt DNS-01 (Cloudflare).
2. Issue/renew; store private keys encrypted; reload Nginx only after validation.

## Initial scope (MVP)

- Local users CRUD, password auth, sessions, Owner/Operator roles
- Internal authoritative zones via CoreDNS
- Forwarding of unmanaged DNS queries to configured resolvers (required)
- Typed records A/AAAA/CNAME/TXT/MX/SRV and wildcards
- Cloudflare-style `proxied` toggle on A/AAAA/CNAME with derived Nginx HTTP config
- Explicit HTTP/HTTPS origins and TCP/UDP stream upstreams
- Proxy knobs on the record: HTTPS default (disableable), HTTP/2, HTTP/3, headers, path redirect/routing, Basic Auth over TLS
- Certificates: self-signed, LE HTTP-01, LE DNS-01 (Cloudflare challenge only)
- PostgreSQL + Drizzle; worker apply with validate/reload/rollback
- Structured logs and in-app job/health/certificate status

## Deferred

OIDC/MFA, dual control sidecars, webhook alerts, reverse DNS/PTR/DNSSEC, ordinary Cloudflare zone sync, Pi-hole/AdGuard product integrations, Docker discovery, backup/restore, HA, multi-tenant RBAC.

## Operating principles

1. Review/validate before apply.
2. Web process has no Nginx/CoreDNS reload privilege.
3. Origins/upstreams are explicit literals.
4. Managed DNS is authoritative; everything else forwards.
5. Proxied names resolve to ProxyCore ingress, never to the origin, while proxied.
6. No secret leakage in logs or API responses.
7. Every apply job is observable and reversible when possible.

## Success criteria

An operator can bootstrap users, resolve managed and forwarded DNS names, toggle proxy on an A/AAAA/CNAME like Cloudflare, publish HTTPS (and optional cleartext) with HTTP/2/HTTP/3 as configured, issue self-signed or Let's Encrypt certificates, and recover from a bad apply without hand-editing CoreDNS/Nginx.

## Terminology

| Term | Meaning |
| --- | --- |
| Desired state | Intended configuration stored by ProxyCore |
| Applied state | Last revision successfully rendered, validated, reloaded, and health-checked |
| Zone | Managed DNS namespace |
| Proxied record | A/AAAA/CNAME with proxy enabled (Cloudflare orange-cloud analogue) |
| DNS-only record | Record served as written; no Nginx HTTP config from it |
| Proxy ingress address | Installation IP(s) returned by CoreDNS for proxied names |
| Origin / upstream | Explicit backend IP/port/protocol |
| Resolver pool | Ordered upstream DNS endpoints for unmanaged queries |
| Stream | TCP/UDP listener → upstream (separate from DNS proxy toggle) |

## Next step

Use the functional requirements and architecture docs as the implementation contracts for this reduced MVP. Do not start application code until the documentation gate is accepted.
