# Exploration: ProxyCore MVP

## Current state

The repository contains the approved product, functional, domain, architecture,
security, testing, and decision documents, but no application code. The
implementation therefore starts from a contract-only baseline.

## Confirmed boundary

- One installation with local Owner and Operator users.
- CoreDNS is authoritative for managed zones and forwards every unmanaged query.
- A, AAAA, and CNAME records may enable a Cloudflare-style HTTP/HTTPS proxy.
- TXT, MX, and SRV remain DNS-only.
- Nginx derives HTTP/HTTPS configuration from proxied records and separately
  handles explicit TCP/UDP streams.
- Certificates include self-signed, Let's Encrypt HTTP-01, and Cloudflare
  DNS-01 for `_acme-challenge` only.
- Web has no service-control privilege; worker/helper operations are fixed and
  auditable.

## Main implementation risks

1. The helper transport and privileged backend need a narrow protocol and tests.
2. Desired/applied revisions must be immutable enough to support rollback.
3. DNS and Nginx renderers must stay deterministic and must not leak secrets.
4. Live ACME/provider behavior cannot be proven without environment credentials.
5. Docker verification must be delayed until normal tests and type checks pass.

## Delivery choice

The user explicitly accepted `size:exception` for this full MVP. The change is
still split into work-unit commits so each behavior has a focused rollback and
verification record. No pull requests or remote pushes are part of this task.
