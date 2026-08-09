# Verify report: control-plane-go-spa

Date: 2026-08-09

## Commands

| Check | Command | Result |
| --- | --- | --- |
| Go tests | `cd apps/api && go test ./...` | PASS |
| UI build | `pnpm --dir apps/ui build` | PASS |
| Compose config | `docker compose config` | PASS |
| Full Compose up/build on 1GB host | — | Not run in this session (report honestly) |

## Spec coverage

| Requirement | Status |
| --- | --- |
| DEPLOY-001 api without Docker socket; no default Next `web` | Met in `compose.yaml` |
| DEPLOY-002 `/api/health` on api | Met (Go handler + healthcheck) |
| DEPLOY-003 Dockerfile.api has no `next build` | Met |
| AUTH-001/002 Go bootstrap/login/logout + cookies | Met (unit tests; DB integration skipped without DATABASE_URL) |
| API-001 full Go route parity | **Partial** — configuration routes proxied to transitional `node-api` |
| API-002 apply queue semantics | Met via proxied Node handlers + existing worker |

## Residual work

- Port proxied configuration routes from `node-api` into Go (tasks 3.x).
- Port full dashboard desks into `apps/ui` (tasks 4.3).
- Retire `node-api` service.
- Live Compose build verification on a 1GB target (worker/node-api `pnpm install` may still be heavy).

## Rollback

Restore previous Compose `web` service from git history; PostgreSQL data remains compatible.
