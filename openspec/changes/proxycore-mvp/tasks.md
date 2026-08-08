# ProxyCore MVP Tasks

Every task is intended to be completed with its focused tests and committed as
one reviewable work unit. Docker-dependent tasks are intentionally last.

## Delivery forecast

Decision needed before apply: No

Chained PRs recommended: No

400-line budget risk: High

The user explicitly accepted `size:exception`; the implementation will still
use small commits and no pull requests.

## Phase 1 — SDD and baseline

- [ ] 1.1 Create OpenSpec configuration, active change state, proposal,
  exploration, delta specs, design, and assumptions.
- [ ] 1.2 Create the pnpm/TypeScript/Next.js/Vitest baseline and scripts.
- [ ] 1.3 Add configuration parsing, safe environment defaults, and a passing
  smoke test.

Focused verification: `pnpm test -- --runInBand` where supported and
`pnpm typecheck`. Rollback boundary: baseline/configuration files only.

## Phase 2 — Domain and persistence

- [ ] 2.1 Implement typed identifiers, installation settings, user roles,
  revision/job state machines, and audit contracts.
- [ ] 2.2 Implement DNS names, TTLs, record values, wildcard, CNAME, and
  canonical-host validation.
- [ ] 2.3 Implement resolver pools/rules, literal upstreams, streams, and
  path policy precedence.
- [ ] 2.4 Implement Drizzle PostgreSQL schema and initial migration.
- [ ] 2.5 Implement repositories, transaction ports, in-memory adapters, and
  retention-safe artifact operations.

Focused verification: domain unit tests plus repository contract tests.
Runtime harness: `N/A` for pure domain; PostgreSQL contract is final Docker
verification. Rollback boundary: `packages/domain`, `packages/db`, and tests.

## Phase 3 — Authentication

- [ ] 3.1 Implement scrypt password hashing, opaque sessions, cookie policy,
  revocation, and expiry.
- [ ] 3.2 Implement bootstrap and Owner/Operator authorization services.
- [ ] 3.3 Implement user CRUD, last-Owner protection, and auth audit events.

Focused verification: auth service and route contract tests. Rollback boundary:
auth/crypto modules and related schema tables.

## Phase 4 — DNS and CoreDNS

- [ ] 4.1 Implement zone/record commands and desired revision creation.
- [ ] 4.2 Implement forwarding pool/rule commands and longest-suffix selection.
- [ ] 4.3 Implement deterministic RFC 1035 zone-file and Corefile rendering.
- [ ] 4.4 Add fixtures for DNS-only/proxied answers and validation failures.

Focused verification: renderer snapshots and domain tests. Runtime harness:
`N/A` until final CoreDNS container verification. Rollback boundary: DNS
commands, renderer, and fixtures.

## Phase 5 — Proxy and Nginx

- [ ] 5.1 Implement proxied-record policy validation and origin derivation.
- [ ] 5.2 Implement typed headers, path rules, redirects, rewrites, auth,
  WebSocket, cache, timeout, and body-limit policies.
- [ ] 5.3 Implement Nginx HTTP/HTTPS and TCP/UDP stream rendering.
- [ ] 5.4 Implement HTTP/3 capability/publication gate and listener conflicts.

Focused verification: renderer fixtures and policy tests. Runtime harness:
`N/A` until final Nginx verification. Rollback boundary: proxy policies and
Nginx renderer.

## Phase 6 — Certificates and secrets

- [ ] 6.1 Implement AES-GCM secret encryption, key references, and redaction.
- [ ] 6.2 Implement self-signed certificate generation and metadata parsing.
- [ ] 6.3 Implement ACME HTTP-01 and Cloudflare DNS-01 adapter ports.
- [ ] 6.4 Implement configurable real adapters and deterministic fake adapters.
- [ ] 6.5 Implement certificate renewal safety and challenge cleanup.

Focused verification: crypto, certificate, and fake-provider tests. Runtime
harness: live ACME is final environment-dependent verification. Rollback
boundary: certificate package and secret service.

## Phase 7 — Worker, control, and operations

- [ ] 7.1 Implement worker job claim/serialization and immutable snapshots.
- [ ] 7.2 Implement JSON-lines Unix-socket helper protocol and allowlist.
- [ ] 7.3 Implement candidate stage/validate/promote/reload/health/rollback
  orchestration with evidence.
- [ ] 7.4 Implement structured logs, health aggregation, and job output.
- [ ] 7.5 Implement retention cleanup by age/size with live-state protection.

Focused verification: orchestration and protocol tests with fake control
adapter. Runtime harness: final Compose service-control checks. Rollback
boundary: worker/control/operations packages.

## Phase 8 — Web API and UI

- [ ] 8.1 Implement app composition and request/session context.
- [ ] 8.2 Implement bootstrap/login/logout/session and users route handlers.
- [ ] 8.3 Implement settings, DNS, forwarding, proxy, streams, certificates,
  jobs, and health route handlers.
- [ ] 8.4 Implement functional admin pages for all primary journeys.
- [ ] 8.5 Add route authorization/redaction tests and accessible empty/error
  states.

Focused verification: handler tests, typecheck, and build. Runtime harness:
local Next.js server without Docker where possible. Rollback boundary: web
routes/pages and composition.

## Phase 9 — Deployment and final verification

- [ ] 9.1 Add Compose services, Dockerfiles, images, mounts, healthchecks,
  migrations, and environment template.
- [ ] 9.2 Add CoreDNS/Nginx runtime configuration and service-control wiring.
- [ ] 9.3 Add bootstrap/apply/rollback/certificate/rotation/recovery runbooks.
- [ ] 9.4 Run all normal tests, typecheck, lint, and build before Docker.
- [ ] 9.5 If Docker is available, run Compose, CoreDNS, Nginx, stream,
  rollback, and certificate staging checks.
- [ ] 9.6 Write the SDD verification matrix and close/archive the change.

Focused verification: `pnpm test`, `pnpm typecheck`, `pnpm lint`, `pnpm build`.
Docker harness: `docker compose config` followed by the documented integration
scenario. Rollback boundary: deployment/runbook files and final verification
artifact.
