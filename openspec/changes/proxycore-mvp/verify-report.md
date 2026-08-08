# ProxyCore MVP Verification Report

## Verification context

- Date: 2026-08-08
- Host: macOS Darwin 25.6.0
- Node: 24.14.1 (the repository requires 24.19.0 in production images)
- pnpm: 11.16.0
- Docker phase: BLOCKED — Docker daemon unavailable

## Normal verification (Docker-independent)

| Gate | Command | Result |
| --- | --- | --- |
| TDD suite | `pnpm test` | PASS — 17 files, 36 tests |
| TypeScript | `pnpm typecheck` | PASS |
| Compatible lint | `pnpm lint` | PASS — JavaScript/tooling config lint |
| Next production build | `pnpm build` | PASS — Next.js 16.3.0 |
| Drizzle schema | `pnpm db:generate` | PASS — no schema changes |

The TypeScript 7.0.2 decision is retained. `typescript-eslint` currently rejects
TypeScript 7, so the lint gate intentionally covers compatible configuration
files while `tsc` remains the TypeScript correctness gate. This limitation is
recorded in `docs/assumptions.md` and must be revisited when parser support
lands.

## Requirement matrix

| Area | Evidence | Result | Boundary |
| --- | --- | --- | --- |
| Auth/bootstrap/RBAC | `packages/domain/src/auth.test.ts`, API route test, crypto tests | PASS | PostgreSQL auth store is typechecked; live DB integration remains Docker-dependent |
| Domain invariants | DNS/proxy/job/snapshot tests | PASS | No production traffic |
| PostgreSQL schema/migrations | Drizzle schema, generated migration, Pg auth/secret stores | PARTIAL | No live PostgreSQL container run yet |
| CoreDNS rendering | `packages/renderers/src/coredns.test.ts` | PASS | CoreDNS process/query verification pending |
| Nginx rendering | `packages/renderers/src/nginx.test.ts` | PASS | `nginx -t` and HTTP/stream checks pending |
| Certificates/secrets | crypto, self-signed, fake DNS-01 tests | PASS | Live ACME/Cloudflare credentials and network are intentionally absent |
| Worker lifecycle | apply, rollback, protocol, retention/health tests | PASS | Long-running DB job polling is not yet wired into the worker entrypoint |
| Control helper | protocol tests and Docker boundary tests | PASS | Docker Engine execution pending |
| Web API/UI | API handler test, typecheck, production build | PASS | Current web context defaults to in-memory ports for host TDD |
| Compose topology | static files and Dockerfiles | PARTIAL | Compose config/build/start checks pending |
| Secret redaction | crypto redaction and encrypted-store tests | PASS | No live log-volume audit |

## Known partial items

1. The normal web test context uses in-memory configuration/auth ports. The
   PostgreSQL schema and auth/secret repositories exist, but production context
   selection and full desired-state repository contract tests remain follow-up
   work.
2. The worker has a tested orchestrator and heartbeat, but its entrypoint does
   not yet poll PostgreSQL jobs continuously.
3. Live ACME issuance, Cloudflare API behavior, public HTTP-01 reachability,
   and certificate renewal are untested without user-provided credentials and
   domains.
4. `ASM-014` remains review: the Nginx base image is configurable by
   `NGINX_IMAGE`; a release digest must be frozen before production use.

## Docker phase

| Check | Result | Evidence |
| --- | --- | --- |
| Docker daemon | BLOCKED | `docker info` could not connect to `unix:///Users/gustavog/.docker/run/docker.sock`; the daemon is not running |
| Compose topology parse | PASS | `PROXYCORE_MASTER_KEY_BASE64=<temporary verification value> POSTGRES_PASSWORD=verification docker compose config` |
| Compose image build | BLOCKED/N/A | Requires the unavailable daemon |
| Compose start and healthchecks | BLOCKED/N/A | Requires the unavailable daemon |
| PostgreSQL migration/integration | BLOCKED/N/A | Requires the unavailable daemon |
| CoreDNS `dig`/forwarding/proxied answers | BLOCKED/N/A | Requires the unavailable daemon |
| Nginx `nginx -t`, HTTP/HTTPS, streams | BLOCKED/N/A | Requires the unavailable daemon |
| Apply rollback through Docker helper | BLOCKED/N/A | Requires the unavailable daemon |
| Live ACME/Cloudflare staging | BLOCKED/N/A | Requires daemon plus user-provided credentials/domains |

No Docker-dependent check is claimed as passed beyond the static Compose
configuration parse.
