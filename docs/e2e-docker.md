# Docker endpoint e2e

`scripts/e2e-docker-endpoints.sh` drives a real PRIMARY + NODE pair (api, worker
and control per installation, no nginx or CoreDNS) and calls **every** HTTP
endpoint at least once, asserting status codes and response shapes.

```sh
bash scripts/e2e-docker-endpoints.sh      # needs docker, curl, jq, openssl, python3
E2E_KEEP=1 bash scripts/e2e-docker-endpoints.sh   # keep containers for inspection
```

It builds the images from the working tree, uses compose projects
`pcep-primary` / `pcep-node`, the network `pcep-net` and images
`pcep-e2e-*:local`, and removes all of them on exit. Ports can be changed with
`E2E_PRIMARY_PORT`, `E2E_NODE_PORT`, `E2E_PRIMARY_TLS_PORT`, `E2E_NODE_TLS_PORT`.
Expect roughly 4 minutes after the images are built.

## What it covers

| Phase | Coverage |
| --- | --- |
| E1 | public routes (health, ready, updates, acme-challenge, SPA), bootstrap 201/409, login/logout, change-password, status, settings, enrollment hostnames, zones, records, streams, certificates (self-signed issue/list/ca/renew/delete), apply, users, backup export/import (dry run), updates/apply without an updater (503) |
| E2 | enrollment trust and CA download, v2 tokens (defaults, create, list, revoke, delete 204/409), join using only the token (draft, preview, confirm), the `:3443` listener, nodes view and heartbeat, sync status and retry, NODE read-only matrix (`403 TOPOLOGY_READ_ONLY` vs exempt routes), unlink then `unlinked` on the node, re-enroll with a fresh token, promote and demote |
| E3 | `scripts/lib/e2e-coverage.sh` compares `routes()` in `apps/api/internal/httpserver/server.go` plus the `:3443` endpoints with the requests the run made, and fails when one was not hit |

Print the route inventory with `bash scripts/lib/e2e-coverage.sh --list`.
Adding a route to `routes()` without a request in the e2e makes the run fail.

## Helpers

`scripts/lib/e2e-docker.sh` holds the shared harness (`req`, `treq` for the
`:3443` listener, `expect`, `jqexpect`, summary, cleanup). The older
`scripts/e2e-node-ca-trust.sh` keeps its own copy and is independent.

## Notes

- A standalone-primary has no enrollment listener, so the first token requires
  promoting the installation to PRIMARY first (the suite asserts the
  `ENROLLMENT_TRUST_NOT_READY` answer before promoting).
- ACME issuance needs the internet and the updater needs its sidecar, so those
  paths are asserted by their documented error status only.
- The unlink 410 `NODE_UNLINKED` is observed through the NODE, which holds the
  credential: its sync status reports `unlinked: true`.
- CI: job `e2e-docker-endpoints` in `.github/workflows/ci.yml` runs on push to
  `main` and on manual dispatch. It is not part of the required `CI` gate.
