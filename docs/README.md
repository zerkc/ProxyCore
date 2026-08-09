# ProxyCore Design Documents

Pre-implementation contracts for the reduced MVP: local DNS (as the sole homelab resolver) plus Nginx ingress, certificates, and local users for one installation.

## Reading order

1. [Product definition](product-definition.md) — purpose, boundary, journeys
2. [Functional requirements](functional-requirements.md) — observable MVP behavior
3. [Domain model](domain-model.md) — entities and integrity
4. [Architecture](architecture.md) — control/data plane and Compose topology
5. [Security and operations](security-operations.md) — threats, secrets, TLS, runbooks
6. [Testing and roadmap](testing-and-roadmap.md) — environments, gates, phases
7. [Decision log](decision-log.md) — confirmed and deferred decisions

## Scope status

- MVP implemented as a Compose stack; product entry point is the root [README](../README.md)
- Contracts here remain the source of truth for boundary and deferred work (DEC-038, DEC-040–DEC-049)
- MVP: local users, internal DNS + **forward unmanaged queries**, **Cloudflare-style proxy toggle** on A/AAAA/CNAME (HTTPS default, HTTP/2/HTTP/3, headers, path, Basic Auth), TCP/UDP streams, certificates (self-signed, LE HTTP-01, Cloudflare DNS-01)
- Deferred: OIDC/MFA, dual control sidecars, webhook alerts, reverse DNS, ordinary Cloudflare sync, backup, HA

## Review path

Confirm the product boundary and DEC-038 first. Verify functional requirements, domain model, and architecture describe the same MVP—especially sole-DNS forwarding, the single-installation model, proxied-host conflicts, and HTTP/3 deployment.
