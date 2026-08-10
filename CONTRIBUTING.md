# Contributing to ProxyCore

Thanks for helping improve a single-host homelab DNS and ingress control plane.

By participating, you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## Quick path

1. Open an issue (bug or feature) before large work.
2. Fork and create a branch from `main`.
3. Make a focused change with tests.
4. Open a PR using the template — CI must be green.

## Development setup

Requirements:

- [Bun](https://bun.sh) `1.3.14` (see `packageManager` in `package.json`)
- Go matching `apps/api/go.mod`
- Docker Engine + Compose v2 (for local stack / compose validation)
- Optional: Postgres for Go DB-backed tests (`DATABASE_URL`)

```sh
bun install
bun run typecheck
bun run test
bun run lint
bun run build:ui

cd apps/api
go test ./...
# Full handler tests:
# export DATABASE_URL=postgres://proxycore:proxycore@localhost:5432/proxycore?sslmode=disable
# go test ./...
```

Local stack: see [docs/runbooks/bootstrap.md](docs/runbooks/bootstrap.md) and `.env.example`.

## What to change where

| Area | Path |
| --- | --- |
| Control plane API | `apps/api` |
| Dashboard SPA | `apps/ui` |
| Worker / apply | `apps/worker` |
| Service control helper | `apps/control` |
| Shared domain / renderers | `packages/*` |
| Compose / images | `compose.yaml`, `infra/compose` |
| Product decisions | `docs/decision-log.md` |

Keep the MVP boundary: single-host, local operators, no OIDC/MFA/HA in scope unless maintainers agree in an issue first.

## Pull requests

- One concern per PR when practical.
- Link the issue (`Fixes #123`).
- Prefer tests next to the behavior you change (Vitest under `packages/` / `apps/`, Go `*_test.go` under `apps/api`).
- Do not commit `.env`, secrets, PEMs, or Cloudflare tokens.
- Docs/runbooks go with behavior changes.

PRs cannot merge while the **CI** check is failing. The default branch ruleset also requires review approval.

## Releases

Maintainers ship versions as follows:

1. Bump `VERSION`, root `package.json` / `apps/ui/package.json` version, and the default in `apps/api/internal/version/version.go` in one PR.
2. Merge to `main`.
3. Tag `vX.Y.Z` (must match `VERSION`) **or** run the **Release** workflow with that version.
4. The release workflow runs tests, builds Linux `amd64`/`arm64` API binaries, publishes `ghcr.io/zerkc/proxycore-api`, and creates a GitHub Release.

## Security

Report vulnerabilities privately via
[GitHub Security Advisories](https://github.com/zerkc/ProxyCore/security/advisories/new).
Do not open a public issue for undisclosed security problems.

## License

Contributions are accepted under the [Apache License 2.0](LICENSE).
Unless you state otherwise, each contribution is licensed under the same terms.
