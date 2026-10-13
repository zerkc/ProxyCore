# efficient-on-device-build

## Objective
Make the on-device (build mode) update fast and memory-safe on small nodes (1 GB RAM, 2 cores)
without publishing per-architecture images: build stays on-the-fly, any Docker host works.

## Problem
The updater runs one `docker compose build api worker control nginx` (apps/updater/internal/updater/updater.go ~L292,
services default at ~L138). Compose builds them in parallel: Vite + 3 go builds + 2 bun installs at once on 1 GB.
worker and control Dockerfiles are near-identical (same deps, differ only in CMD).

## Scope (authorized)
- T1 Serialize builds: updater builds one service at a time, in order, instead of one multi-service build.
- T3 Merge worker + control into a single runtime image (one build, command differs per service).
- T4 Single `go build` invocation for the three Go binaries in Dockerfile.api + build resource caps (GOMAXPROCS/GOGC).

Out of scope: registry/pre-built images, UI bundle prebuild (T2, deferred), swap (T5, deferred).

## Constraints
- No per-arch assumptions; no new publish pipeline.
- Artifacts (code, comments, commits) in English. Conventional Commits, no AI attribution.
- ~400 changed-line planning heuristic per task (advisory).

## Tasks
- [x] T1 serialize per-service build in updater (+ tests, RED first) - 0e60189; RED: 2 tests failed (single multi-service build); GREEN: go test ./... ok
- [x] T3 unify worker/control image (Dockerfile + compose + references/docs) - 65a6b06; Dockerfile.worker renamed Dockerfile.runtime, Dockerfile.control removed, control reuses proxycore-runtime:local; compose config -q ok; `compose build control` => warning 'No services to build', exit 0; no other references found
- [x] T4 single go build + resource caps in Dockerfile.api (and updater Dockerfile if applicable) - 8592577; api/updater/migrate images built OK, three binaries at same paths; admin/convert do not link version pkg so -X is a no-op for them

## Acceptance
- T1: build step issues N sequential `compose build <svc>` calls in service order; failure stops at first failing service and reports it; pull mode unchanged. Go tests pass.
- T3: `docker compose config` valid; worker and control both start from one image; no leftover references to the removed Dockerfile.
- T4: `docker build -f infra/compose/Dockerfile.api` still yields the same three binaries at the same paths.

## Checks
- `go test ./...` in apps/updater (T1, RED then GREEN)
- `docker compose -f compose.yaml config -q`
- `docker build` of api image if Docker available; otherwise state not run.

## Routing (per task)
- T1/T3/T4: delegated direct, one writer (2+ non-trivial files; reading prepares write).

## Progress / Evidence
- Branch: perf/efficient-on-device-build (from main be52c3f)
- Delivery strategy: ask-on-risk; forecast < 400 changed lines.

## Next step
Delegate writer for T1 → T3 → T4 with one commit per task.
