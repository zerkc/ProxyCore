# Proposal: Migrate control plane from Next.js to Go API + Vite SPA

## Intent

Replace the Next.js control-plane process with a **Go HTTP API** that serves a
**static Vite + React SPA**, so ProxyCore can build and run on small hosts
(≈1 GB RAM) without a Next.js compile or Node UI runtime. Product behavior of
the reduced MVP stays the same. PostgreSQL remains the durable store for this
change. The Node worker and service-control helper remain temporarily.

## Motivation

- Observed failure on a 1 GB Proxmox CT was **Docker image build** (`pnpm
  install` + `next build`), not proven runtime OOM.
- Next.js is the heaviest control-plane build and a large runtime resident set.
- Vite static build + a single Go binary is dramatically lighter to build and
  serve while preserving the React operator UI.

## Scope

### In scope

- OpenSpec change artifacts (explore → propose → spec → design → tasks → apply → verify).
- Go API (`apps/api`) owning `/api/*` JSON contracts currently served by Next.
- Vite + React SPA (`apps/ui`) replacing Next pages (bootstrap, login, dashboard).
- Compose/`install.sh`: `api` service replaces `web`; SPA assets baked into the
  API image; Drizzle migrate remains a Node one-shot until Go owns migrations.
- Cookie/Bearer session auth compatible with existing `users`/`sessions` rows
  (same scrypt and opaque-token hashing).
- Retire Next from the default Compose path; keep `apps/web` only as legacy
  reference until archive.

### Out of scope (this change)

- SQLite (explicitly deferred).
- Porting worker or control helper to Go.
- OIDC/MFA and other deferred MVP items.
- Changing CoreDNS/Nginx product behavior or apply semantics.
- Publishing prebuilt registry images (optional follow-up).

## Approach

1. Freeze contracts in delta specs (auth, configuration API, deployment).
2. Scaffold Go server + Vite SPA with health and SPA fallback (TDD).
3. Port auth, then read APIs, then mutation APIs against the existing schema.
4. Move dashboard UI into `apps/ui` against the same JSON shapes.
5. Switch Compose and install script; verify with Go tests + UI build + Compose
   health (Docker reported honestly if blocked).

## Success criteria

- Default Compose stack has **no Next.js service**.
- `GET /api/health` and SPA `/` work from the Go process.
- Bootstrap, login, logout, and authenticated status work end-to-end.
- Remaining MVP API routes used by the SPA are implemented in Go or explicitly
  listed as blocked in the verify report.
- Worker/control continue to apply desired state from PostgreSQL.
- Focused Go tests pass; `apps/ui` production build succeeds.

## Rollback plan

- Revert the Compose/`install.sh` commit to restore `web` (Next) service.
- Leave PostgreSQL data intact (schema unchanged).
- SPA/API code can be removed without touching worker/control/data plane.

## Affected areas

- `apps/api`, `apps/ui` (new).
- `apps/web` (legacy; removed from default Compose).
- `infra/compose/Dockerfile.api`, `Dockerfile.migrate`, `compose.yaml`.
- `scripts/install.sh`, root `README.md`, `docs/decision-log.md`,
  `openspec/config.yaml`.
