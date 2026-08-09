# Exploration: Go API + Vite SPA control plane

## Current state

- MVP is implemented with Next.js App Router (`apps/web`), Node worker,
  Node control helper, PostgreSQL/Drizzle, CoreDNS, Nginx.
- `install.sh` and Compose previously built three heavy Node images; web ran
  `pnpm build` (Next), which OOMs on 1 GB builders.
- Partial WIP already exists for `apps/api` (stdlib Go server + health tests)
  and `apps/ui` (Vite React shell).

## Confirmed decisions for this change

- Control plane HTTP + UI: **Go API + Vite/React SPA**.
- Persistence for this attempt: **PostgreSQL** (no SQLite yet).
- Worker + control: remain Node until a later change.
- Cloudflare DNS-01 credentials: UI-submitted, encrypted at rest (not env).
- Operator password minimum: 5 characters.

## Risks

1. Porting `ConfigurationService` / certificate issuance to Go is large; JSON
   contract drift would break the SPA.
2. Password/session crypto must stay byte-compatible with existing rows.
3. Node worker/control **image builds** can still stress 1 GB hosts even after
   Next is gone.
4. ACME HTTP-01 challenge store today is process-local in Next; Go must own an
   equivalent reachable via host-network Nginx → `:WEB_PORT`.

## Delivery choice

`delivery.strategy: exception-ok` remains. Work proceeds in reviewable units
with tests co-located. No PR requirement unless requested.
