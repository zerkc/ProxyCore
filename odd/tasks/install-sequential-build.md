# Feature: install-sequential-build

## Objective
Make `scripts/install.sh` usable on very small hosts (Raspberry Pi 3 B+: 1 GB RAM, 4x A53, SD/USB storage) by building images one at a time with visible progress, and remove a noisy, misleading pull error during `up`.

## Problem (evidence)
- User log on a Pi 3 B+: install took 2081 s (~35 min) with 5 images building concurrently. Trivial steps took minutes (`COPY VERSION` 374 s, `COPY apps/api/` 1148 s), `go build` of the updater took 1858 s vs 164 s for the api (same kind of work), `exporting layers` of the worker 1729 s: host starved of RAM/IO (swap on SD), not slow commands.
- `scripts/install.sh:485` runs `docker compose up -d --build`, which builds every service in parallel. The updater already serializes builds (`apps/updater/internal/updater/updater.go` `buildSequentially`, commit 0e60189) but the installer does not.
- `control` uses `image: proxycore-runtime:local` with no `build:`; `docker compose up` prints a non-fatal `pull access denied for proxycore-runtime` (reproduced with the real install.sh on Compose v5.5.1; exit 0, compose then builds the image).
- Release workflow publishes only the api image, so prebuilt multi-arch images are a separate, larger follow-up (option A, not in this feature).

## Scope
- `scripts/install.sh`: build services sequentially with `[n/N] building <svc>` progress and elapsed time, then `up -d --remove-orphans` without `--build`; keep `SKIP_BUILD=1` semantics; a failed build stops with a clear message naming the service.
- Disable default buildx attestations/provenance for local builds (overridable by the user's env).
- Low-memory guard: detect small RAM/swap and WARN with concrete guidance (never create swap or use sudo automatically).
- `compose.yaml`: `pull_policy: never` on `worker` and `control` (locally built runtime image). Do NOT touch `nginx` or the updater pull-mode design.
- Docs: low-memory / Raspberry Pi install notes in the install runbook/README.

## Out of scope
- Publishing multi-arch images to GHCR (option A, follow-up).
- Changing the updater's pull-mode behavior; Dockerfile compiler caps (already done in 8592577).

## Constraints
- POSIX `sh`, matching the style of `scripts/install.sh` and `scripts/install.test.sh`; existing script tests must keep passing; add tests first (RED) in `scripts/install.test.sh`.
- No sudo, no system changes, no secrets in output.
- Advisory heuristic ~400 authored changed lines per task.

## Checks
`sh scripts/install.test.sh`, `sh scripts/install_env.test.sh`, `docker compose config -q`, shellcheck if available, plus an isolated real run in a copy (project `pcseq`, free high ports, own volumes, never touching pre-existing resources).

## Tasks
- [x] **IS-1 — Sequential build + progress + attestation off + low-memory guard in install.sh (+ tests)** — 51a2063; RED (`deploy_stack: command not found`) then GREEN; install.test.sh and install_env.test.sh pass; isolated run exit 0 with [1/6]..[6/6]
- [x] **IS-2 — `pull_policy: never` on worker/control in compose.yaml (+ verify `up` is clean)** — 1dac4b9; `docker compose config -q` ok; no `pull access denied` in isolated run (only traefik/whoami pulled)
- [x] **IS-3 — Docs: low-memory / Pi install notes** — b5b4844 (README.md, docs/runbooks/bootstrap.md)

## Route declaration
- Exploration/diagnosis: delegated (Docker repro of install.sh; build log analysis inline).
- IS-1..IS-3: delegated, one bounded writer (2+ non-trivial files).

## Progress
- Branch `perf/install-sequential-build` from `origin/main` (d8bdd5d). IS-1..IS-3 done.

## Delivery
- ask-on-risk. Version bump decision deferred to PR time.

## Next step
Open PR (human decision); version bump deferred.
