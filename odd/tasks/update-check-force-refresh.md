# update-check-force-refresh

## Objective
The dashboard "check for updates" button must query GitHub again instead of returning the in-memory cached result.

## Problem
`Checker.Check` (apps/api/internal/update/checker.go) returns the cached result until the TTL (6 h, `DefaultTTL`) expires;
failures are cached for `failureTTL` (<= 5 min). `GET /api/updates` has no way to bypass it, and the UI button
(`refreshUpdate`, apps/ui/src/dashboard/dashboard-context.tsx) only sets `cache: "no-store"` for the browser.

## Scope (authorized)
- T1 Backend: `Checker.Refresh(ctx)` that bypasses the cache, keeps single-flight (`inFlight`) semantics, and enforces a minimum
  interval between forced refreshes (~10 s) returning the cached result when throttled. `GET /api/updates?refresh=1` uses it.
- T2 UI: the explicit button (and Retry) forces refresh; the automatic initial load / polling keeps using the cache.
- Tests: Go tests (RED first) for checker + handler; UI test only if a runner already covers the touched component.

Out of scope: version bump, TTL changes, release/tagging.

## Constraints
- English artifacts; Conventional Commits; no AI attribution lines in commits.
- Branch: fix/update-check-force-refresh (from main bb774d4). Do not push, do not open PR.
- ~400 changed-line heuristic per task (advisory).

## Tasks
- [x] T1 backend Refresh + `?refresh=1` handler + Go tests (commit c1f018e)
- [x] T2 UI button forces refresh (auto polling unchanged) (commit 0a14f3c)

## Acceptance
- A forced request hits the GitHub endpoint even with a fresh cache (observed in test via counting stub server).
- Concurrent forced requests share one upstream call; a second forced request inside the min interval does not call GitHub.
- A plain `GET /api/updates` still serves from cache.
- UI: button path calls `/api/updates?refresh=1`; periodic/initial path calls `/api/updates`.

## Checks
- `cd apps/api && go test ./internal/update/... ./internal/httpserver/...`
- `bun run typecheck` and `bun run lint` (UI), `bun run test` if applicable

## Routing
- T1/T2: delegated direct, one writer (2+ non-trivial files).

## Progress / Evidence
- Branch created from main bb774d4 (VERSION 0.5.1).
- T1 RED: `go test ./internal/update/... ./internal/httpserver/...` failed to build (`checker.Refresh undefined`); handler test saw 1 upstream call, wanted 2.
- T1 GREEN: same command passes; `go build ./...`, `go vet` on update and httpserver pass; `go test -race` on Checker/Updates tests passes; gofmt clean.
- T2: `bun run typecheck` exit 0; `bun run lint` exit 0; `bun run test` 1 failed (EnrollmentTrustPanel timezone-dependent assertion, pre-existing, unrelated); `TZ=UTC bun run test` 227 passed.
- T2 has no new UI test: the suite has no DOM environment and the only render test uses a ~150-line hand-built fake DOM. A provider test was written then removed.

## Open risks
- VersionStatus polls `onRetry` every 5 s while applying, so forced upstream calls run during the apply poll (throttled to 10 s).
- Plain `GET /api/updates` does not throttle; only forced calls are throttled.
- No UI test covers the forced URL; needs jsdom or a pure helper to add one.

## Next step
Maintainer review of T1/T2 commits. Push, PR, and version bump remain out of scope.
