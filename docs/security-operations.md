# ProxyCore Security and Operations

Safe enough for a carefully exposed homelab ingress path, without pretending the host OS or network is risk-free. Goal: reduce blast radius, keep secrets out of ordinary paths, and make apply/rollback observable.

## Assumptions

- One trusted operator owns the host.
- Host OS, Docker, and physical network are outside ProxyCore's direct control.
- Upstreams may be private/loopback/Docker addresses by operator choice.
- Control plane may be Internet-reachable only through intentional TLS and auth.

## Assets and controls

| Asset | Control |
| --- | --- |
| Local passwords/sessions | Strong hashing, Secure/HttpOnly cookies, revocation, audit |
| Provider credentials / TLS keys | Encrypt at rest, master key outside Postgres, redaction |
| DNS/Nginx desired state | Typed validation, revisioning, validate-before-reload, rollback |
| Worker privileges | Separate from web; no raw Docker socket on web or worker; fixed helper operations only |
| Forwarded DNS | Visible forwarder health; managed zones never silently replaced by forward answers |

## Trust boundaries

1. Browser → control plane (untrusted input)
2. Control plane → PostgreSQL
3. Control plane → worker (job contract only)
4. Worker → service-control helper (private Unix socket; fixed operations)
5. Service-control helper → Nginx/CoreDNS (constrained validate/reload)
6. Nginx → upstreams
7. CoreDNS → configured forwarders
8. Control plane → Cloudflare DNS-01 (optional, scoped)

## Identity (MVP)

- Local username/password only
- One installation with Owner and Operator roles; server-side checks on every mutation
- Bootstrap creates the first Owner
- Owner manages user CRUD
- OIDC and MFA are deferred

## Secrets

- Master key outside PostgreSQL
- Encrypt credentials, Basic Auth password hashes, and certificate private keys at rest
- Never return plaintext secrets in API/log/audit/rendered non-secret config
- Basic Auth stores an encrypted `{SHA}` hash referenced by `passwordSecretId`; the worker writes htpasswd files into the Nginx candidate directory
- Fail closed if the key is unavailable

## Input safety

### DNS

Canonicalize names; type-validate values; enforce TTL bounds and CNAME rules; treat wildcards as DNS labels, not regexes; structure-render all CoreDNS output.

### Forwarding

Only explicit DNS IP:port endpoints; ordered fallback; managed zones authoritative first; forwarder failure is visible.

### Upstreams / Nginx

Literal IP/port/protocol only; typed policy fields plus explicitly bounded server-level Nginx directives from the Record dialog; exact/prefix path rules with deterministic precedence; `nginx -t` before reload; Basic Auth requires client TLS; HTTPS default on but disableable per proxied record; HTTP/2 and HTTP/3 are explicit proxied-record settings; HTTP/3 requires verified binary support plus TCP/UDP 443; proxied DNS answers must be ingress addresses (never origin) while proxy is on; trusted proxy CIDRs only when configured.

## TLS operations

| Mode | Use |
| --- | --- |
| Self-signed | Internal names / no public challenge |
| Let's Encrypt HTTP-01 | Publicly reachable hostnames |
| Let's Encrypt DNS-01 | Via Cloudflare `_acme-challenge` only |

Renewal failure keeps the current valid certificate and surfaces expiry risk. Backend HTTPS verification is explicit per proxied record and off by default for homelab origins; client-facing certs remain validated by ProxyCore policy.

## Logging and audit

- Operational logs for diagnosis
- Audit events for who changed what
- Correlation IDs; secret redaction
- In-app job/health/certificate status
- Configurable retention: 7 days or 50 MB by default, whichever limit is reached first
- No Prometheus or webhook alert system in MVP

## Backup

Post-MVP. Production-like migrations need preflight and Owner-acknowledged manual recovery until backup exists.

## Runbooks required before production-like use

- Bootstrap Owner and create operators
- Apply/roll back DNS change (managed + forwarder)
- Apply/roll back proxied A/AAAA/CNAME (DNS ingress answer + Nginx TLS on/off, HTTP/2, optional HTTP/3)
- Issue self-signed and staging LE certs (HTTP-01 and DNS-01)
- Recover from invalid Nginx or CoreDNS candidate
- Rotate master key / Cloudflare DNS-01 credential

## Security checklist

- [ ] Web cannot run arbitrary host commands or hold Docker socket
- [ ] Worker has no raw Docker socket
- [ ] Service-control helper socket is private to the worker
- [ ] Worker reload path is constrained
- [ ] Worker service-control operations are fixed and cannot execute user-supplied commands
- [ ] Mutations are server-authorized
- [ ] Secrets/keys encrypted and redacted
- [ ] Candidates validated before reload
- [ ] Unmanaged DNS forwarding works and fails visibly
- [ ] Basic Auth cannot enable on cleartext client routes
- [ ] HTTP/3 is enabled only after binary support and TCP/UDP 443 are verified
- [ ] CoreDNS not publicly exposed by default
- [ ] Trusted tunnel headers cannot be spoofed on the direct path
