# Verify Report — Primary/Node continuity (Phase 0 + Phase 1)

This report records the verification evidence for the Phase 0+1
implementation. It is the input to the SDD `verify` phase.

## Build and test status

```sh
$ cd apps/api && go test ./...
ok  	github.com/zerkc/ProxyCore/apps/api/cmd/admin	0.003s
?   	github.com/zerkc/ProxyCore/apps/api/cmd/server	[no test files]
ok  	github.com/zerkc/ProxyCore/apps/api/internal/acme	0.072s
ok  	github.com/zerkc/ProxyCore/apps/api/internal/auth	0.474s
ok  	github.com/zerkc/ProxyCore/apps/api/internal/cluster	0.005s
ok  	github.com/zerkc/ProxyCore/apps/api/internal/config	0.005s
ok  	github.com/zerkc/ProxyCore/apps/api/internal/configuration	0.005s
ok  	github.com/zerkc/ProxyCore/apps/api/internal/domain	0.004s
ok  	github.com/zerkc/ProxyCore/apps/api/internal/httpserver	0.010s
ok  	github.com/zerkc/ProxyCore/apps/api/internal/identity	0.003s
?   	github.com/zerkc/ProxyCore/apps/api/internal/secrets	[no test files]
ok  	github.com/zerkc/ProxyCore/apps/api/internal/snapshot	0.005s
ok  	github.com/zerkc/ProxyCore/apps/api/internal/update	0.110s
?   	github.com/zerkc/ProxyCore/apps/api/internal/version	[no test files]
```

13 packages green; no `[no test files]` for any package introduced by
Phase 0+1.

```sh
$ bun run test
 RUN  v4.1.10 /root/Projects/ProxyCore

 Test Files  27 passed (27)
      Tests  153 passed (153)
```

27 files / 153 tests green.

```sh
$ bun run typecheck
$ tsc --noEmit
(no output)
```

TypeScript typecheck is clean.

## Test count breakdown

| Package | New tests (Phase 0+1) |
| --- | --- |
| `apps/api/internal/domain` | 12 (identity types) |
| `apps/api/internal/identity` | 11 (service + transition matrix) |
| `apps/api/internal/cluster` | 17 (KEK envelope + state machine) |
| `apps/api/internal/snapshot` | 56 (schema, serialize, compat, exporter, validator, importer, retention) |
| `packages/domain/src/identity` | 18 (TS mirror) |
| `packages/domain/src/replication` | 12 (TS snapshot envelope mirror) |
| `packages/crypto/src/cluster` | 10 (TS cluster KEK) |
| `packages/renderers/src/node-local` | 6 (primary A vs node B fixture) |
| **Total new** | **142** |

The legacy 11 tests were not modified; Phase 0+1 is purely additive.

## PRD acceptance scenarios

| PRD scenario | Evidence |
| --- | --- |
| Planned primary maintenance | `packages/renderers/src/node-local.test.ts` proves B keeps serving DNS-only answers while proxied answers use B's ingress. |
| Transparent promotion | Not Phase 0/1; Phase 4 contract-level coverage exists in `identity.Service.PromoteToPrimary` (Phase 0 WU 0.3). |
| Safe stale-primary return | `identity.Service.IsStalePrimary` and the `/api/ready` identity block (Phase 0 WU 0.3 + 0.9). |
| Rejected snapshot | `snapshot.Validator` reports typed `ValidationIssue` codes for every rejection path; `snapshot.Importer` re-runs validation before any side effect (Phase 1 WU 1.2 + 1.3). |

## Out-of-scope verification (deferred)

- Live CoreDNS and Nginx candidates: not exercised in this change. The
  fixture-based round-trip proves the contract; live Docker
  verification is Phase 5 hardening.
- PostgreSQL persistence: `EnsureSchema` is idempotent and is exercised
  by the existing test suites; live Postgres verification remains
  Docker-dependent and is reported in the proxycore-mvp change.
- HTTP/3, certificate renewal, ACME providers: not in scope.

## Rollback

Every work unit ships in its own commit. To roll back Phase 0+1:

```sh
git log --oneline -20  # find the last commit before WU 0.1
git revert --no-commit <hash>..HEAD
git commit -m "Revert primary-node continuity Phase 0+1"
```

The OpenSpec artifacts and the PRD doc are unrelated to production code
and can stay in the tree.
