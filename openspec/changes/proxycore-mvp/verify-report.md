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
| TDD suite | `pnpm test` | PASS — 20 files, 44 tests |
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
| PostgreSQL schema/migrations | Drizzle schema, generated migration, Pg auth/secret stores, persistent configuration/job stores, live migration and API apply | PASS | Live ACME/provider staging remains Docker/environment-dependent |
| CoreDNS rendering | `packages/renderers/src/coredns.test.ts`, authoritative and forwarding `dig` checks | PASS | Candidate verification used a temporary `example.test` zone |
| Nginx rendering | `packages/renderers/src/nginx.test.ts`, helper `nginx -t`, HTTP/HTTPS/TCP/UDP checks | PASS | TLS check used a one-day local self-signed certificate |
| Certificates/secrets | crypto, self-signed, fake DNS-01 tests | PASS | Live ACME/Cloudflare credentials and network are intentionally absent |
| Worker lifecycle | apply, rollback, protocol, poller, notification wake-up, stale-claim recovery, and control-client tests | PASS | Live ACME/provider jobs remain environment-dependent |
| Control helper | protocol tests, private socket, Docker stage/validate/promote/reload/health/rollback | PASS | Fixed operations only; no arbitrary command path |
| Web API/UI | API handler test, automatic DNS apply, advertised-ingress, and HTTPS certificate validation tests, PostgreSQL-backed production context, typecheck, production build | PASS | Host TDD explicitly uses in-memory adapters |
| Compose topology | config parse, all image builds, start, healthchecks, socket/mount inspection | PASS | Live ACME/provider staging remains environment-dependent |
| Secret redaction | crypto redaction and encrypted-store tests | PASS | No live log-volume audit |

## Known partial items

1. Live ACME issuance, Cloudflare API behavior, public HTTP-01 reachability,
   and certificate renewal are untested without user-provided credentials and
   domains.
2. `ASM-014` remains review: the Nginx base image is configurable by
   `NGINX_IMAGE`; a release digest must be frozen before production use.
3. The verification database still contains `proxy1.ggzdeveloper.com` as a
   TLS-proxied record without a certificate. Combined applies fail closed until
   that record receives a certificate or is disabled.

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
| Persistent API → worker apply | PASS | API-created `ggzdeveloper.com` zone/job moved from `queued` to `applied`; desired/applied revision pointers and job outputs were read back from PostgreSQL |
| Automatic DNS apply on save | PASS | Zone/record mutation transaction returned a queued `combined` apply and the worker processed it through PostgreSQL notification wake-up |
| Automatic LAN advertised address | PASS — initialization and rollback | Clearing the persisted address and authenticating with `Host: 192.168.9.74:3000` initialized and persisted `192.168.9.74`; container `172.x` addresses were not selected. Promotion was rejected by the pre-existing TLS-proxied `proxy1.ggzdeveloper.com` record without a certificate, so the previous active revision remained in service. |
| HTTPS certificate guard | PASS | The real record mutation API rejected HTTPS proxy save with HTTP 400 before enqueueing when no certificate was selected |
| PostgreSQL notification wake-up | PASS | Worker logged `reconciling every 300000ms`; a newly committed job was claimed and finished within milliseconds rather than waiting for reconciliation |
| CoreDNS managed answer | PASS | `dig @127.0.0.1 nginx.ggzdeveloper.com A` returned local authoritative `192.0.2.44`, not the Cloudflare answers |
| CoreDNS forwarding | PASS | `dig @127.0.0.1 example.com A` returned upstream answers |
| Nginx validation and HTTP/HTTPS | PASS | Helper `validate`, HTTP 308 redirect, HTTPS 200 with local self-signed certificate |
| Nginx TCP/UDP streams | PASS | TCP stream returned origin HTTP 200; UDP stream returned `udp-ok` |
| Apply and rollback through Docker helper | PASS | Nginx changed candidate returned 418 then rolled back to 200; CoreDNS changed answer `127.0.0.2` then rolled back to `127.0.0.1` |
| Web/worker restart persistence | PASS | After restarting both containers, the same user, zone, revision, applied job, and local DNS answer remained available |
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
5. The control client now closes each request cleanly after receiving its
   response, while the helper destroys half-closed sockets; this prevents
   leaked Unix-socket connections during notification/reconciliation handling.
