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
- [x] **PNE-2B — CA-backed enrollment material**: persist a dedicated enrollment leaf/key, preserve SPKI across renewal, fail closed on corrupt established material, and add the additive migration. Focused tests and disposable PostgreSQL 17 migration/runtime verification passed.
  - File-disjoint review units and authored additions: A `apps/api/internal/acme/*` = 52; B `apps/api/internal/configuration/internal_ca.go` = 255; C `apps/api/internal/configuration/internal_ca_test.go` = 237; D schema/migration/task evidence = 76; aggregate = 620; each unit is below 400.
  - TDD evidence: RED `cd apps/api && go test ./...` (225 passed, 3 failed) plus `bun test packages/db/src/phase2-schema.test.ts` (1 passed, 1 failed); GREEN final Go (228 passed) and Drizzle (2 passed); TRIANGULATE/REFACTOR used PostgreSQL 17 runtime, `gofmt`, and final focused checks.
  - Exact validation: `cd apps/api && go test ./...`; `cd apps/api && go test ./internal/acme ./internal/configuration ./internal/secrets`; `bun test packages/db/src/phase2-schema.test.ts`; `bun run typecheck`; `git diff --check` — all final checks passed (228, 29, 2, typecheck, and diff check).
  - Runtime evidence: `master_key=$(openssl rand -base64 32 | tr -d '\n')`; `docker run --name proxycore-pne2b-pg17-final -e POSTGRES_USER=proxycore -e POSTGRES_PASSWORD=proxycore -e POSTGRES_DB=proxycore -p 55436:5432 -d postgres:17`; `DATABASE_URL="postgres://proxycore:proxycore@127.0.0.1:55436/proxycore" PROXYCORE_MASTER_KEY_BASE64="$master_key" bun run db:migrate`; `cd apps/api && DATABASE_URL="postgres://proxycore:proxycore@127.0.0.1:55436/proxycore" PHASE2_DATABASE_URL="postgres://proxycore:proxycore@127.0.0.1:55436/proxycore" PROXYCORE_MASTER_KEY_BASE64="$master_key" go test ./internal/configuration ./internal/secrets`; `docker rm -f proxycore-pne2b-pg17-final`; PostgreSQL-backed configuration/fail-closed/concurrency tests passed with a valid ephemeral master key.
  - Rollback boundary is limited to the allowlisted PNE-2B files and additive identity schema; no listener, proof routes, token/bootstrap, UI, Compose, or Nginx changes. Next unit: PNE-2C identity-proof contract.
- [ ] **PNE-2C — Identity-proof contract**: sign canonical attempt/nonce/URL/identity/generation/fingerprint proofs and verify tampering, expiry, roles, and hostname binding.
  - Review split: URL canonicalization is complete as the first file-disjoint subunit; proof signing/verification remains a separate follow-up subunit.
  - [x] **PNE-2C1 — URL canonicalization**: validate bounded HTTPS origin input with explicit host/port, DNS normalization, canonical IP literals, root-only paths, and no redirect-bearing URL components.
    - Strict TDD: RED captured the missing `CanonicalizePrimaryURL` API; GREEN covered canonical DNS/IPv4/IPv6/root/port forms; TRIANGULATE covered malformed schemes, authority, ports, userinfo, query/fragment, paths, DNS/IP syntax, escapes, zones, whitespace, controls, and bounds; REFACTOR preserved focused green tests.
    - Validation: `cd apps/api && go test ./internal/enrollment`; `cd apps/api && go test ./...`; `git diff --check` — all passed. No proof signing, listener, transport, trust bootstrap, or identity serialization was added.
    - Honest count: 234 authored lines across `url.go`, `url_test.go`, and this evidence (below the 400-line review budget); parent commit remains pending.
    - Rollback boundary: `apps/api/internal/enrollment/url.go`, `apps/api/internal/enrollment/url_test.go`, and this task evidence only.
  - [x] **PNE-2C2 — Identity proof signing and verification**: sign trusted primary identity with committed enrollment TLS material and expose pure tamper/expiry/binding verification.
    - Corrective strict-TDD RED: added direct lower/higher/zero generation, self-invalid CA, fresh-provider, and strict-expiry tests before implementation; the initial focused run failed against the stale constructor/signature API.
    - GREEN: provider-based signing and explicit CA self-signature validation passed the focused enrollment suite; TRIANGULATE covers all prior tamper, redaction, algorithm, role, expiry, and binding cases; REFACTOR preserved coverage.
    - Formatted file map and counts:
      - Unit A: `identity_proof.go` — 129 lines, public types, bounded request generation, and canonical framing.
      - Unit B: `identity_proof_crypto.go` — 141 lines, RSA/certificate parsing, CA self-signature, pins, hostname, and framing helpers.
      - Unit C: `identity_proof_signer.go` — 128 lines, fresh identity/material/time providers and leaf-only signing.
      - Unit D: `identity_proof_verifier.go` — 113 lines, exact generation, time-window, binding, and detached verification.
      - Unit E: `identity_proof_test.go` — 281 lines, preserved baseline round-trip/tamper/redaction coverage.
      - Unit F: `identity_proof_signer_test.go` — 160 lines, provider refresh, self-invalid CA, and expiry-boundary coverage.
      - Unit G: `identity_proof_verifier_test.go` — 18 lines, direct lower/higher/zero generation coverage.
      - Unit H: task evidence — 18 changed lines.
    - Dependency order: A → B → (C, D) → E → (F, G) → H; every unit is file-disjoint and below 400 changed lines.
    - Formatted honest count: 988 changed lines total (970 formatted source lines plus 18 task-evidence changes), reported by unit rather than compressed.
    - Validation: `cd apps/api && go test ./internal/enrollment` (77 passed); `cd apps/api && go test ./...` (303 passed); `gofmt -d apps/api/internal/enrollment/identity_proof*.go` clean; `git diff --check` passed.
    - Pure contract only: no handler, listener, TLS startup, trust bootstrap/export, UI, Compose, token/bootstrap grant, snapshot, redirect, or client transport changes.
    - Rollback boundary: the seven `apps/api/internal/enrollment/identity_proof*.go` files and this task evidence.
- [ ] **PNE-2D — Proof-only HTTPS listener**: serve only the proof mux over TLS 1.3 on port 3443, reject redirects/downgrades, and keep ordinary HTTP routes unavailable.
  - [x] **PNE-2D-A — Proof-only handler/mux**: enforce the exact POST route, bounded strict JSON, live provider signing, TLS/authority/SNI/SAN checks, non-leaking errors, and no-store proof responses. Excludes listener startup and real handshake coverage.
    - Correction evidence: streamed unknown-length coverage first exposed only an overbroad test assertion; after allowing the required generic `identity proof request` message, the unchanged handler passed, so no production correction was made.
    - Validation: streamed test passed; `cd apps/api && go test ./internal/enrollment ./internal/httpserver` (154 passed); `cd apps/api && go test ./...` (330 passed); handler `gofmt -d` clean; `git diff --check` passed.
    - Honest count: 396 prior additions plus 28 test-only correction lines = 424 cumulative; the correction unit remains below 400 and PNE-2D-B is untouched.
    - Rollback boundary: `apps/api/internal/httpserver/enrollment_identity.go`, its focused test, and this evidence only.
  - [ ] **PNE-2D-B — TLS listener**: add the dedicated TLS 1.3 server helper, current certificate provider, safe timeouts, and real CA/leaf handshake harness.
    - [x] **PNE-2D-B1 — Atomic TLS certificate provider**: validate leaf/key/CA material before publication, expose complete immutable snapshots, and preserve the last valid snapshot on failed replacement.
      - Strict TDD: correction RED `cd apps/api && go test ./internal/enrollment` failed behaviorally (95 passed, 3 failed) on ECDSA CA publication; GREEN the corrected focused package passed (98 tests); TRIANGULATE/REFACTOR preserved redaction, concurrency, uninitialized, ordered-chain rejection, accessor-state preservation, and RSA-boundary coverage.
      - Validation: first core split `cd apps/api && go test ./internal/enrollment ./internal/httpserver` (164 passed); final split focused packages (175 passed); `cd apps/api && go test ./...` (351 passed); test `gofmt -d` clean; `git diff --check` passed.
      - File mapping/dependency order: production unchanged → `tls_server_fixture_test.go` (249 shared fixture/helper lines) + `tls_server_test.go` (159 core publish/order/concurrency lines) → `tls_server_algorithm_test.go` (82 RSA/failure lines) → task evidence (7 lines). Tests/evidence total 497; aggregate 729; every test file remains below 400 without compression or omitted coverage.
      - Provider only: listener startup/lifecycle, TLS handshake integration, cmd/server, Compose, trust export, and enrollment transport remain deferred to PNE-2D-B2.
      - Rollback boundary: `apps/api/internal/enrollment/tls_server.go`, `apps/api/internal/enrollment/tls_server_test.go`, and this evidence only.
    - [ ] **PNE-2D-B2 — TLS listener and real handshake harness**: add the dedicated TLS 1.3 server helper, safe timeouts, graceful lifecycle, and the ephemeral CA/leaf integration harness.
- [ ] **PNE-2E — TLS startup and trust UX**: wire startup/shutdown and Compose exposure; provide CA fingerprint display plus public CA export/import flows and restart-stability verification.
- [ ] **PNE-3 — Sealed bootstrap grant**: implement ephemeral X25519/HKDF bootstrap sealing, node credential issuance, cluster-KEK transport, binding/AAD validation, exact idempotent retry, and redaction tests.
- [ ] **PNE-4 — Initial snapshot publication**: authenticate current node credentials, publish only latest applied immutable state, enforce revocation, and reject an unready PRIMARY.
- [ ] **PNE-5 — Safe NODE conversion**: archive prior standalone state, preserve node-local overlay, create the sync revision/apply job, wait for exact terminal apply, and atomically commit NODE lineage/state.
- [ ] **PNE-6 — Enrollment HTTP workflow**: add Owner token creation plus NODE draft/preview/confirm/recovery endpoints with explicit replacement confirmation and token re-entry.
- [ ] **PNE-7 — Minimal continuity UI**: add PRIMARY token creation and NODE preview/confirm/pending/recoverable/committed surfaces; remove ordinary editing controls after NODE commit.
- [ ] **PNE-8 — Lifecycle enforcement**: disable NODE renewal, acknowledge only exact successful apply tuples, and verify revocation plus offline data-plane continuity.
- [ ] **PNE-9 — Integrated verification**: exercise two isolated installations over HTTPS with disposable PostgreSQL, initial worker apply, redaction, revocation, failure recovery, and UI build/type checks.

## First Implementation Unit

PNE-1, PNE-2A, PNE-2B, and PNE-2C are complete as recorded above; continue with **PNE-2D-A** only. Keep implementation, tests, runtime evidence, and rollback boundary in the same work-unit commit. PNE-2D-B remains a separate follow-up after independent verification. If the honest unit still exceeds 400 authored changed lines, report the smallest coherent count before implementation rather than compressing or omitting tests.

## Verification Baseline

- Strict TDD is active from `openspec/config.yaml`; Go tests must show RED, GREEN, TRIANGULATE, and REFACTOR evidence.
- Focused Go tests run against disposable PostgreSQL so concurrency and transaction semantics are executed rather than skipped.
- Every review unit records the exact focused command, runtime harness result, changed-line count, and rollback boundary.
- Full repository tests/builds are reserved for integrated boundaries; each unit still runs its applicable focused tests and `git diff --check`.

## Recovery

Resume from the first unchecked review unit. Preserve completed commits and never infer enrollment completion from schema presence alone.
