# Proposal: Implement the ProxyCore MVP

## Intent

Turn the approved ProxyCore contracts into a locally testable and Compose
deployable homelab control plane. An operator should be able to bootstrap the
installation, manage local DNS and forwarding, toggle a record-level proxy,
issue certificates, apply configuration safely, and inspect or recover from
failures without editing CoreDNS or Nginx by hand.

## Scope

### In scope

- Next.js App Router UI and authorized route-handler API.
- PostgreSQL/Drizzle desired state, revisions, jobs, audit, secrets, and
  retention records.
- Local Owner/Operator auth with revocable sessions.
- CoreDNS zones, typed records, wildcards, default/suffix forwarding, and
  proxied DNS answers.
- Nginx HTTP/HTTPS proxy policies, typed path rules, headers, WebSocket,
  Basic Auth, HTTP/2, gated HTTP/3, and TCP/UDP streams.
- Encrypted certificate keys, self-signed certificates, ACME HTTP-01, and
  Cloudflare DNS-01 adapter boundaries.
- Worker apply lifecycle, helper protocol, validation, health, rollback, and
  structured operations.
- Docker Compose topology and final service verification.

### Out of scope

OIDC, MFA, multi-tenant isolation, backup/restore, HA, reverse DNS/PTR/DNSSEC,
ordinary Cloudflare record synchronization, tunnel lifecycle, Docker discovery,
Prometheus, and webhook alert delivery.

## Approach

1. Establish the hybrid OpenSpec/Engram artifacts and toolchain.
2. Build pure domain rules and persistence ports with TDD.
3. Add auth, DNS, proxy, certificates, worker, and operations as independently
   verifiable work units.
4. Expose the control plane through Next.js and add the functional UI.
5. Add Compose and run Docker-dependent checks last.

## Success criteria

- All non-Docker tests, type checks, and builds pass.
- Every mutation is server-authorized and auditable.
- Invalid desired state cannot replace the last applied revision.
- Managed DNS, unmanaged forwarding, DNS-only answers, and proxied answers are
  distinguishable in tests and renderers.
- Nginx/CoreDNS candidates are deterministic, validated, and reversible.
- Secrets/private keys are encrypted or redacted at every ordinary boundary.
- Docker verification is either green or explicitly reported as blocked by the
  daemon/environment.

## Rollback plan

Each work-unit commit includes its tests and can be reverted independently.
Before apply, revisions remain desired-only. Validation or health failure keeps
the previous applied revision and records a failed or rolled-back job. If the
Compose deployment is invalid, remove only the deployment commit and retain the
control-plane artifacts.

## Affected areas

- `apps/web`: UI and HTTP API.
- `apps/worker`: job orchestration and retention.
- `apps/control`: fixed helper protocol and runtime.
- `packages/domain`, `packages/db`, `packages/config`, `packages/crypto`,
  `packages/renderers`, `packages/certificates`.
- `infra/compose`, `infra/coredns`, `infra/nginx`.
- `docs/assumptions.md` and operational runbooks.
