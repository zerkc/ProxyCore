# Tasks: control-plane-go-spa

Every task is a reviewable work unit with co-located tests. Docker-dependent
checks are last and must be reported honestly if blocked.

## Delivery forecast

Decision needed before apply: No (operator directed Go + Vite, keep Postgres)

Chained PRs recommended: Optional

400-line budget risk: High — `delivery.strategy: exception-ok`

## Phase 1 — SDD and platform

- [x] 1.1 Create OpenSpec change (proposal, exploration, design, specs, tasks, state).
- [x] 1.2 Scaffold Go module `apps/api` with `/api/health` and SPA static fallback tests.
- [x] 1.3 Scaffold Vite React app `apps/ui` with health pulse page.
- [x] 1.4 Add `Dockerfile.api` / `Dockerfile.migrate` / `Dockerfile.node-api`, switch Compose + `install.sh` from `web` to `api`.
- [x] 1.5 Update README, decision log (DEC-050), and `openspec/config.yaml` stack context.

Focused verification: `cd apps/api && go test ./...`. Rollback: api/ui/compose files only.

## Phase 2 — Auth (Go)

- [x] 2.1 Implement scrypt verify/hash + opaque token helpers compatible with Node.
- [x] 2.2 Implement PG auth store (users/sessions/audit) and Auth service.
- [x] 2.3 Implement `POST /api/auth/bootstrap|login|logout` with cookies.
- [x] 2.4 Implement `requireUser` middleware (cookie + Bearer, roles).

Focused verification: `go test ./...` (DB handler test skips without DATABASE_URL).

## Phase 3 — Configuration API (Go)

- [ ] 3.1 `GET /api/status` native in Go (currently proxied to node-api).
- [ ] 3.2 `GET|PUT /api/settings` + ingress initialization side effect.
- [ ] 3.3 Zones and records CRUD + apply job enqueue.
- [ ] 3.4 Streams CRUD + apply job enqueue.
- [ ] 3.5 Users CRUD (Owner guards).
- [ ] 3.6 `POST /api/apply` re-apply.
- [ ] 3.7 Certificates list/issue + `GET /api/acme-challenge/{token}`.
- [x] 3.8 Transitional `node-api` (tsx standalone) + Go reverse proxy for unmet routes.

Focused verification: handler tests + SQL against schema. Rollback: `internal/*` / node-api.

## Phase 4 — SPA

- [x] 4.1 Shared API client, auth pages (login/bootstrap).
- [x] 4.2 Dashboard shell + Pulse/status.
- [ ] 4.3 DNS, ingress, streams, certificates, operators views wired to Go API.

Focused verification: `pnpm --dir apps/ui build`. Rollback: `apps/ui/**`.

## Phase 5 — Cutover and verify

- [x] 5.1 Default Compose has no Next `web` service; nginx depends on `api`.
- [x] 5.2 Go tests + UI build + `docker compose config` (full up deferred/honest).
- [x] 5.3 Write `verify-report.md`; mark Next legacy in README/docs.

Focused verification: see verify-report.md.
