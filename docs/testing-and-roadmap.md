# ProxyCore Testing Strategy and Roadmap

Test ProxyCore as a control plane and as a config generator for real CoreDNS/Nginx behavior.

## Environments

| Environment | Purpose |
| --- | --- |
| Development | Fast feedback; disposable Postgres; fake Cloudflare DNS-01; no production secrets |
| Homelab staging | Real CoreDNS/Nginx on the LAN; LE staging; restricted credentials |
| Production-like | Pinned images; real domain/certs; approved migrations; documented recovery |

## Test layers

### Domain / validation

Zone/record rules, wildcards, TTL, CNAME conflicts, proxied vs DNS-only answer rules, one-proxied-record-per-hostname conflicts, resolver-pool ordering, suffix matching, literal origins, listener conflicts, exact/prefix path precedence, cert coverage, RBAC, upstream TLS policy, job transitions.

### Persistence

Schema/migrations, constraints, encrypted secrets, audit append, revision concurrency, and retention cleanup by age or size without deleting current desired/applied state.

### Renderer

Deterministic CoreDNS/Nginx fixtures; disabled resources omitted; secrets not plaintext; stable checksums.

### Service integration

- CoreDNS candidate on isolated port: SOA, records, wildcards, **forwarded unmanaged names**, fallback/failure
- `nginx -t`, HTTP/HTTPS, WebSocket, Basic Auth, exact/prefix path redirects and rewrites, HTTP/2, HTTP/3 when enabled with TCP/UDP 443, TCP/UDP streams, rollback
- HTTP/3 tests must verify the selected binary exposes HTTP/3 support. Browser validation with self-signed certificates is not sufficient; use a protocol client or a trusted certificate.
- Worker/service-control helper apply path without web-process privileges or a raw Docker socket
- Retention cleanup honors the configured age/size limit without deleting current state or active certificates

### Application integration

Bootstrap, user CRUD, auth/sessions, desired revisions, jobs, certificate lifecycles (self-signed, HTTP-01, DNS-01 fake adapter), audit redaction, forwarder CRUD.

### End-to-end journeys

1. Bootstrap Owner; access the dashboard through the LAN address so the proxy
   advertised address is initialized; create Operator
2. Create zone + DNS-only records; query via CoreDNS
3. Query unmanaged name via default forwarder
4. Enable proxy on an A record (Cloudflare-style); confirm DNS returns ingress IP, not origin
5. Confirm Nginx proxies HTTPS to origin (HTTP/2 on)
6. Disable proxy; confirm DNS returns origin again and Nginx vhost is gone/disabled
7. Toggle HTTPS off on a proxied record; confirm cleartext behavior where allowed
8. Enable HTTP/3 when staging network supports it
9. Issue self-signed cert; issue LE staging HTTP-01 and DNS-01 on proxied names
10. Reject a second proxied record for the same hostname
11. Bad config rejected/rolled back

### Security

Authz bypass attempts, secret leakage checks, config injection, spoofed forwarded headers, Basic Auth over cleartext, public CoreDNS exposure checks, worker privilege checks.

## Release gates

### Documentation gate

- Reduced MVP scope approved (DEC-038)
- Forwarding for unmanaged DNS is first-class
- Self-signed + LE HTTP-01 + Cloudflare DNS-01 explicit
- Deferred items (OIDC/MFA/sidecars/webhooks/backup) not accidental MVP

### Phase gate

Domain + persistence + renderer/service tests; one failure path; audit on mutations; no secret leakage.

### Production-like gate

Staging E2E green; CoreDNS managed+forward paths verified; Nginx validate/rollback verified; cert flows exercised; local recovery/bootstrap verified.

## Roadmap

### Phase 0 — Contracts

Approved product/FR/domain/architecture/security/test/decision docs for the reduced MVP.

### Phase 1 — Control plane

Next.js shell, bootstrap, local users/roles/sessions, Postgres/Drizzle, revisions/jobs, encrypted secrets, status UI.

### Phase 2 — Internal DNS + forwarding

Zones/records/wildcards, default + suffix resolver pools, CoreDNS render/validate/apply/rollback.

### Phase 3 — Proxied records + certificates

Cloudflare-style proxy toggle on A/AAAA/CNAME, derived Nginx HTTP config (headers, path, Basic Auth, HTTP/2, HTTP/3, TLS on/off), TCP/UDP streams, self-signed + LE HTTP-01 + Cloudflare DNS-01, Nginx validate/rollback.

Phases 1–3 are the MVP.

### Post-MVP

| Phase | Content |
| --- | --- |
| Backup | Optional encrypted S3 backup/restore |
| Identity | OIDC, MFA, richer RBAC |
| Hardening | Dual control sidecars, webhooks/metrics exports |
| DNS+ | Reverse/PTR, more record types, public provider sync |
| Integrations | Pi-hole/AdGuard product adapters, Tunnel helpers |

## Fixtures

Apex/subdomain/wildcard records; multiple DNS-only values with one proxied record; rejected duplicate proxied hostnames; exact/prefix path policies; forwarder success and failure; HTTP/HTTPS/TCP/UDP upstreams; self-signed and ACME cert states; job retry/rollback. No real production tokens or keys in fixtures.
