# Primary/Node Enrollment

## Objective

Deliver secure one-time enrollment that converts a standalone ProxyCore installation into a usable NODE only after an authenticated PRIMARY exchange, validated initial snapshot, successful apply, and atomic role commit.

## Product Decisions

- ProxyCore provides integrated TLS for enrollment using its internal CA; the Owner explicitly confirms the PRIMARY fingerprint.
- Enrollment tokens are never persisted on the NODE. The Owner must re-enter the token after reload or recovery.
- A NODE relies only on replicated Owner credentials; it does not retain or create a separate local emergency Owner.
- Creating the first enrollment token atomically transitions `standalone-primary` to `primary`; the first committed NODE transitions it to `primary-with-nodes`.
- Enrollment publishes only the latest successfully applied PRIMARY revision. A PRIMARY without one returns `PRIMARY_NOT_READY`.
- Periodic synchronization, promotion, rejoin, and full node inventory are excluded from this feature.

## Constraints and Safety Invariants

- Enrollment, snapshot, and acknowledgement transport require HTTPS; redirects and insecure verification are rejected.
- Token plaintext is returned once; only hashes and lifecycle metadata are persisted.
- Node credentials, cluster KEKs, private keys, bootstrap grants, and token plaintext never enter logs, URLs, ordinary status, audit values, or reusable caches.
- Token authority, Owner session authority, and node credential authority remain separate.
- NODE role is committed only after prior-state archive, protected credential/KEK storage, exact successful apply, and one atomic database transaction.
- Failure before commit leaves the installation standalone and writable, or explicitly recoverable—never partially converted.
- Snapshot import overlays node-local ingress, node identity, and role instead of copying those values from PRIMARY.
- Revoked credentials fail every later snapshot and acknowledgement request.
- Committed NODEs never perform certificate renewal.

## Review Units

- [x] **PNE-1 — Token authority and PRIMARY activation**: implement one-time token generation, hash-only persistence, expiry/revocation/atomic consume semantics, first-token role activation, and PostgreSQL race tests.
  - Proposed commit split: PNE-1a owns `enrollment/token.go`, `enrollment/token_test.go`, and `configuration/phase2_store.go`; PNE-1b owns `identity/enrollment.go`, `identity/enrollment_test.go`, `enrollment/identity.go`, and `enrollment/identity_test.go`. PNE-1a builds without PNE-1b.
  - Evidence: baseline was GREEN; refactoring preserved final `go test ./...` (211 passes), focused packages (25 passes), and disposable PostgreSQL 17 coverage for lifecycle, cache activation, deterministic read blocking, and rollback.
  - Harness classification: the supplemental blank-PostgreSQL `PHASE2_DATABASE_URL` failure was setup-only because the prerequisite `users` relation was absent; the passing `DATABASE_URL` focused run is the candidate evidence.
  - Honest count: 712 product/test/store lines plus the 58-line task artifact = 770 total; the prior independent 389 + 55 = 444 count remains the pre-correction baseline. No <=400 claim is made.
  - No migration was required. Rollback covers `apps/api/internal/enrollment/`, the token methods/imports in `apps/api/internal/configuration/phase2_store.go`, and `apps/api/internal/identity/enrollment.go` plus its tests.
- [ ] **PNE-2 — Integrated TLS identity proof**: expose enrollment HTTPS with internal-CA identity proof, fingerprint confirmation data, redirect rejection, and hostname/identity binding tests.
- [ ] **PNE-3 — Sealed bootstrap grant**: implement ephemeral X25519/HKDF bootstrap sealing, node credential issuance, cluster-KEK transport, binding/AAD validation, exact idempotent retry, and redaction tests.
- [ ] **PNE-4 — Initial snapshot publication**: authenticate current node credentials, publish only latest applied immutable state, enforce revocation, and reject an unready PRIMARY.
- [ ] **PNE-5 — Safe NODE conversion**: archive prior standalone state, preserve node-local overlay, create the sync revision/apply job, wait for exact terminal apply, and atomically commit NODE lineage/state.
- [ ] **PNE-6 — Enrollment HTTP workflow**: add Owner token creation plus NODE draft/preview/confirm/recovery endpoints with explicit replacement confirmation and token re-entry.
- [ ] **PNE-7 — Minimal continuity UI**: add PRIMARY token creation and NODE preview/confirm/pending/recoverable/committed surfaces; remove ordinary editing controls after NODE commit.
- [ ] **PNE-8 — Lifecycle enforcement**: disable NODE renewal, acknowledge only exact successful apply tuples, and verify revocation plus offline data-plane continuity.
- [ ] **PNE-9 — Integrated verification**: exercise two isolated installations over HTTPS with disposable PostgreSQL, initial worker apply, redaction, revocation, failure recovery, and UI build/type checks.

## First Implementation Unit

Start with **PNE-1** only. Keep implementation, tests, runtime evidence, and rollback boundary in the same work-unit commit. If the honest unit still exceeds 400 authored changed lines, report the smallest coherent count before implementation rather than compressing or omitting tests.

## Verification Baseline

- Strict TDD is active from `openspec/config.yaml`; Go tests must show RED, GREEN, TRIANGULATE, and REFACTOR evidence.
- Focused Go tests run against disposable PostgreSQL so concurrency and transaction semantics are executed rather than skipped.
- Every review unit records the exact focused command, runtime harness result, changed-line count, and rollback boundary.
- Full repository tests/builds are reserved for integrated boundaries; each unit still runs its applicable focused tests and `git diff --check`.

## Recovery

Resume from the first unchecked review unit. Preserve completed commits and never infer enrollment completion from schema presence alone.
