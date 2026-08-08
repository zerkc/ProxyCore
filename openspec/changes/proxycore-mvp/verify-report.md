# ProxyCore MVP Verification Report

## Verification context

- Date: 2026-08-08
- Host: macOS Darwin 25.6.0
- Node: 24.14.1 (the repository requires 24.19.0 in production images)
- pnpm: 11.16.0
- Docker: Docker Desktop 29.6.2, LinuxKit arm64 backend

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
| PostgreSQL schema/migrations | Drizzle schema, generated migration, Pg auth/secret stores, live migration and 16-table query | PASS | Production web context still defaults to in-memory desired-state ports |
| CoreDNS rendering | `packages/renderers/src/coredns.test.ts`, authoritative and forwarding `dig` checks | PASS | Candidate verification used a temporary `example.test` zone |
| Nginx rendering | `packages/renderers/src/nginx.test.ts`, helper `nginx -t`, HTTP/HTTPS/TCP/UDP checks | PASS | TLS check used a one-day local self-signed certificate |
| Certificates/secrets | crypto, self-signed, fake DNS-01 tests | PASS | Live ACME/Cloudflare credentials and network are intentionally absent |
| Worker lifecycle | apply, rollback, protocol, retention/health tests | PASS | Long-running DB job polling is not yet wired into the worker entrypoint |
| Control helper | protocol tests, private socket, Docker stage/validate/promote/reload/health/rollback | PASS | Fixed operations only; no arbitrary command path |
| Web API/UI | API handler test, typecheck, production build | PASS | Current web context defaults to in-memory ports for host TDD |
| Compose topology | config parse, all image builds, start, healthchecks, socket/mount inspection | PASS | Live ACME/provider staging remains environment-dependent |
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

The Docker daemon was available for the final verification pass. Temporary
verification values were used for PostgreSQL and the master key; no repository
secret was created. A temporary Nginx origin and UDP echo origin were attached
only to the Compose data network.

| Check | Result | Evidence |
| --- | --- | --- |
| Docker daemon | PASS | `docker info`; Docker Desktop server 29.6.2 |
| Compose topology parse | PASS | `PROXYCORE_MASTER_KEY_BASE64=<temporary verification value> POSTGRES_PASSWORD=verification docker compose config` |
| Compose image build | PASS | `docker compose build`; web, worker, control, CoreDNS, and Nginx images built |
| Compose start and healthchecks | PASS | `docker compose up -d`; PostgreSQL, web, worker, control, and Nginx healthy; CoreDNS answering |
| PostgreSQL migration/integration | PASS | `docker compose exec -T web pnpm db:migrate`; 16 public tables queried |
| CoreDNS managed answer | PASS | `dig @127.0.0.1 app.example.test A` returned authoritative `127.0.0.1` |
| CoreDNS forwarding | PASS | `dig @127.0.0.1 example.com A` returned upstream answers |
| Nginx validation and HTTP/HTTPS | PASS | Helper `validate`, HTTP 308 redirect, HTTPS 200 with local self-signed certificate |
| Nginx TCP/UDP streams | PASS | TCP stream returned origin HTTP 200; UDP stream returned `udp-ok` |
| Apply and rollback through Docker helper | PASS | Nginx changed candidate returned 418 then rolled back to 200; CoreDNS changed answer `127.0.0.2` then rolled back to `127.0.0.1` |
| Socket and Docker privilege boundary | PASS | Web had no mounts; worker had no Docker socket; only control had read-only Docker socket |
| Live ACME/Cloudflare staging | NOT RUN | Requires user-provided credentials, publicly reachable domains, and challenge routing |

## Docker fixes discovered during verification

1. CoreDNS uses a minimal non-root image, so promotion now writes the Corefile
   through the Docker archive API and synchronizes zone files through a
   control-only shared volume. Its fixed reload operation restarts CoreDNS so
   changed zone contents are loaded deterministically.
2. CoreDNS renderer `file` directives use the container-absolute
   `/etc/coredns/zones` path.
3. Docker `exec` output is drained before completion, and the Unix-socket
   server tolerates half-closed clients without terminating the helper.
4. The web image runs the generated Next standalone server instead of emitting
   the `next start`/standalone incompatibility warning.
