# Primary/Node Enrollment

## Objective

Deliver secure one-time enrollment that converts a standalone ProxyCore installation into a usable NODE only after an authenticated PRIMARY exchange, validated initial snapshot, successful apply, and atomic role commit.

## Product Decisions

- ProxyCore provides integrated TLS for enrollment using its internal CA; the Owner explicitly confirms the PRIMARY fingerprint.
- The Owner configures exact enrollment DNS names/IP addresses from the ProxyCore UI before certificate issuance; ProxyCore never guesses the host LAN address or reuses proxy ingress implicitly.
- Initial CA trust supports both manual CA fingerprint entry and public CA certificate export/import.
- Enrollment TLS is exposed directly on port 3443, separate from Nginx and the existing HTTP UI/API.
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
- [x] **PNE-2A — Enrollment address configuration**: persist exact DNS/IP SANs, add Owner-only API/UI configuration, validate canonical names, and prevent certificate issuance until configured.
  - Added the authenticated Continuity route with Owner GET/PUT wiring, canonical response display, validation errors, and explicit TLS/token scope copy.
  - Mounted component harness result: `bun test apps/ui/src/dashboard/ContinuityView.test.tsx apps/ui/src/dashboard/DashboardShell.test.tsx` — 8 passed, including 6 mounted ContinuityView cases.
  - Strict TDD: RED captured the missing DOM test environment before harness adjustment; GREEN captured 8 focused passes; TRIANGULATE covers configured states, accessibility, validation, operator denial, and NODE/stale-primary read-only states.
  - Validation: `bun run test` (29 files/171 tests), `bun run typecheck`, `bun run build:ui`, and `git diff --check` passed.
  - UI-specific tsc baseline: 8 errors, including 3 candidate errors in `ContinuityView.test.tsx` and 5 pre-existing errors in four unrelated UI files.
  - UI-specific tsc after correction: candidate errors are 0; the exact 5 unrelated pre-existing errors remain.
  - Buildable split: unit 1 shared API/types = 38 authored additions; unit 2 component = 200; unit 3 route/nav/shell = 36; unit 4 mounted tests/evidence = 384; apply in order 1 → 2 → 3 → 4, each under 400.
  - Rollback boundary: the UI/API helper/type/nav/route files and this task evidence; no certificate, token, listener, or deployment changes.
- [ ] **PNE-2B — CA-backed enrollment material**: persist a dedicated enrollment leaf/key, preserve SPKI across renewal, fail closed on corrupt established material, and add the additive migration.
- [ ] **PNE-2C — Identity-proof contract**: sign canonical attempt/nonce/URL/identity/generation/fingerprint proofs and verify tampering, expiry, roles, and hostname binding.
- [ ] **PNE-2D — Proof-only HTTPS listener**: serve only the proof mux over TLS 1.3 on port 3443, reject redirects/downgrades, and keep ordinary HTTP routes unavailable.
- [ ] **PNE-2E — TLS startup and trust UX**: wire startup/shutdown and Compose exposure; provide CA fingerprint display plus public CA export/import flows and restart-stability verification.
- [ ] **PNE-3 — Sealed bootstrap grant**: implement ephemeral X25519/HKDF bootstrap sealing, node credential issuance, cluster-KEK transport, binding/AAD validation, exact idempotent retry, and redaction tests.
- [ ] **PNE-4 — Initial snapshot publication**: authenticate current node credentials, publish only latest applied immutable state, enforce revocation, and reject an unready PRIMARY.
- [ ] **PNE-5 — Safe NODE conversion**: archive prior standalone state, preserve node-local overlay, create the sync revision/apply job, wait for exact terminal apply, and atomically commit NODE lineage/state.
- [ ] **PNE-6 — Enrollment HTTP workflow**: add Owner token creation plus NODE draft/preview/confirm/recovery endpoints with explicit replacement confirmation and token re-entry.
- [ ] **PNE-7 — Minimal continuity UI**: add PRIMARY token creation and NODE preview/confirm/pending/recoverable/committed surfaces; remove ordinary editing controls after NODE commit.
- [ ] **PNE-8 — Lifecycle enforcement**: disable NODE renewal, acknowledge only exact successful apply tuples, and verify revocation plus offline data-plane continuity.
- [ ] **PNE-9 — Integrated verification**: exercise two isolated installations over HTTPS with disposable PostgreSQL, initial worker apply, redaction, revocation, failure recovery, and UI build/type checks.

## First Implementation Unit

PNE-1 is complete as two buildable review units: **PNE-1A** token lifecycle/persistence (`1ceb36a`) and **PNE-1B** live PRIMARY activation/cache integration (`a02d843`). Continue with **PNE-2A** only. Keep implementation, tests, runtime evidence, and rollback boundary in the same work-unit commit. If the honest unit still exceeds 400 authored changed lines, report the smallest coherent count before implementation rather than compressing or omitting tests.

## Verification Baseline

- Strict TDD is active from `openspec/config.yaml`; Go tests must show RED, GREEN, TRIANGULATE, and REFACTOR evidence.
- Focused Go tests run against disposable PostgreSQL so concurrency and transaction semantics are executed rather than skipped.
- Every review unit records the exact focused command, runtime harness result, changed-line count, and rollback boundary.
- Full repository tests/builds are reserved for integrated boundaries; each unit still runs its applicable focused tests and `git diff --check`.

## Recovery

Resume from the first unchecked review unit. Preserve completed commits and never infer enrollment completion from schema presence alone.
