# multiarch-pull-default

## Objective
Install and update pull prebuilt images by default; CI publishes every image as multi-arch manifests so Raspberry Pi and x86 hosts never build on-device.

## Decision (user)
Support linux/amd64 + linux/arm64 for ALL images. linux/arm/v7 (Pi 2, Pi 3 on 32-bit OS) only for images whose base supports it (Go/alpine/nginx/coredns); bun-based worker/runtime is NOT supported on arm/v7 (Bun has no arm/v7). linux/386 is out of scope.

## Findings
- release.yml publishes only proxycore-api, no `platforms:` (amd64 only).
- compose.yaml uses `:local` tags + pull_policy: never for most services.
- updater already defaults to pull (updater.go UpdateModePull); install.sh always builds.

## Tasks
- [x] T1 release.yml: build+push all images (api, runtime/worker, updater, updater-bootstrap, migrate, nginx, coredns) multi-arch (amd64, arm64; arm/v7 where supported), cross-compile Go via BUILDPLATFORM
- [x] T2 Dockerfiles: BUILDPLATFORM/TARGETARCH cross-compile, UI stage on BUILDPLATFORM
- [x] T3 compose.yaml: image refs resolve to GHCR (overridable by env), pull default, build opt-in
- [x] T4 install.sh: pull by default, PROXYCORE_INSTALL_MODE=build opt-in, arch detection + clear arm/v7 worker message; update install.test.sh
- [x] T5 docs/runbooks/bootstrap.md: support matrix + modes
- [x] T6 updates always pull: remove build mode from updater (Go), updater-bootstrap.sh, compose env wiring, install.sh .env write, docs

## Route
Delegated direct: one writer (2+ non-trivial files).

## Evidence
- T1: 54488ba/4c34feb: release.yml adds QEMU + per-image multi-arch build-push (api/updater/updater-bootstrap/migrate/nginx/coredns amd64+arm64+arm/v7; runtime amd64+arm64); YAML parse ok; actionlint not installed
- T2: 54488ba: Go + UI stages on BUILDPLATFORM with GOOS/GOARCH/GOARM; runtime deps installed on BUILDPLATFORM; local buildx for updater (arm/v7+arm64) and migrate (arm/v7) succeeded; base images verified via imagetools (bun: amd64+arm64 only; golang/alpine/nginx: arm/v7 ok; coredns: arm/v7 ok)
- T3: compose commit: GHCR refs with PROXYCORE_IMAGE_REPO/PROXYCORE_VERSION/PROXYCORE_<NAME>_IMAGE, pull_policy never removed, update mode default pull; docker compose config -q rc=0
- T4: install.sh pull default, PROXYCORE_INSTALL_MODE=build opt-in, check_arch; bash/sh scripts/install.test.sh rc=0, install_env.test.sh rc=0
- T5: docs/runbooks/bootstrap.md support matrix + modes + low-memory note
- Updater: apps/updater defaults to pull (cmd/main.go envOr UpdateModePull), updater-bootstrap.sh defaults pull; compose env default fixed from build to pull; cd apps/updater && go test ./... ok
- T6: updater Go build mode/fetch-source/SourceDir/PROXYCORE_UPDATE_MODE removed; test proves env build value ignored; bootstrap sh always pulls (sh -n ok); compose env wiring removed; install.sh no longer writes PROXYCORE_UPDATE_MODE; go build/vet/test ok; install.test.sh + install_env.test.sh rc=0; compose config -q rc=0; rg shows only doc note + ignore-test hits; no api/web/packages references
