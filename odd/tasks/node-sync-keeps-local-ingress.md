# Node sync keeps the node's own ingress

## Objective
A node sync must never overwrite the node's persisted advertised ingress (`installation_settings.ingress_ipv4/ipv6`) with the primary's. Resolver pool and forwarding rules may still be replaced by the sync.

## Evidence
- `apps/api/internal/configuration/sqlite_import_materialize.go:54-57,156-160`: `sqliteReplaceSettings` always writes `ingress_*`, from `nodeLocal.ingress` (env-derived) or, if empty, the primary's `settings.ingress`.
- `apps/api/cmd/server/main.go:171-173`: the node sync `Ingress` closure reads only `cfg.ProxyIngressIPv4/IPv6` (env), never the persisted settings.
- `apps/worker/src/apply.ts:336`: worker keeps `settings.ingress` when `nodeLocal.ingress` is empty, so CoreDNS answers with the primary IP.

## Decision
Persisted node ingress is the source of truth. Env vars are only the initial value when nothing is persisted. The primary's ingress is never applied on a node. (Owner-confirmed.)

## Tasks
- [x] T1 Materializer: `sqliteReplaceSettings` stops writing `ingress_*`; test that an import leaves the node's ingress untouched while resolver/forwarding rules are replaced.
- [x] T2 Node sync `Ingress` closure resolves persisted settings ingress first, env second; test.
- [x] T3 Update existing tests that asserted the old behaviour.

## Routing
Delegated direct: one writer (2+ non-trivial files, preparation reading).

## Acceptance
`cd apps/api && go test ./internal/configuration/... ./cmd/server/... ./internal/sync/...` green.

## Progress
T1-T3 done (delegated writer; RED observed, then GREEN). `go build`, `go vet`, `go test ./internal/configuration/... ./cmd/server/... ./internal/sync/...` green; parent re-ran tests (cached, same inputs).

## Open follow-ups (out of scope, need owner decision)
- Worker `apps/worker/src/apply.ts:336` falling back to the primary ingress only when the node has no local ingress is intended (owner-confirmed); no change.
- ~~Enrollment `LocalIngress` closure reads env only~~ fixed: reuses `nodeSyncIngressResolver` (persisted first, env second).
