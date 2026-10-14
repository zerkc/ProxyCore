# Feature: primary-node-role-conversion

## Objective
Let an owner convert an installation PRIMARY -> NODE and NODE -> PRIMARY from the UI, safely.

## Problem
- `PromoteToPrimary` exists in `apps/api/internal/identity/service.go` but has no HTTP route or UI.
- Transition matrix (`identity/service.go`, `cluster/state.go`) forbids `primary` / `primary-with-nodes` -> `node`.
- `NodeContinuityView` only renders when role is already `node`; a primary has no convert control.

## Decisions (user-confirmed unless noted)
- D1 (user): on promotion, the former primary's nodes are NOT auto-repointed; they must be re-enrolled manually against the new primary.
- D2 (default, PRD-aligned): rotate cluster key on promotion; invalidate old primary relationship and node credentials.
- D3 (default): PRIMARY -> NODE is a direct owner action behind an explicit confirmation; nodes of that primary lose their credentials first.
- D4 (default): promotion is allowed while the old primary is reachable (planned switchover) and offline (failover), always with a split-brain warning and expected-generation check.

## Scope
- apps/api: transition matrix, promote/demote workflow, HTTP routes, audit.
- apps/ui: Promote control on NODE surface, Convert-to-NODE control on PRIMARY surface.
- Tests alongside behavior.

## Tasks
- [x] T1 Backend: allow primary/primary-with-nodes -> node in both matrices; extend PromoteToPrimary into a transactional workflow (D2, D4); owner-guarded routes `POST /api/topology/promote` and `POST /api/topology/demote` with confirmation + expected generation; audit; Go tests (RED first).
- [x] T2 UI: Promote to PRIMARY control (warning dialog) on node surface; Convert to NODE entry on primary surface leading to the enrollment form; UI tests.
- [x] T3 Docs: update PRD status/decisions and README topology section.
- [x] T4 Backend: serve enrollment draft/preview/confirm/recover on the main API (:3000) behind owner session auth, reusing the existing workflow handler and its dependency wiring; the UI gets 404 today because these routes only exist on the TLS enrollment listener (:3443). Go tests (RED first).

- [ ] T5 E2E: stand up a PRIMARY and a NODE with Docker (isolated compose projects), run the real enrollment (token on primary -> draft/preview/confirm from the node) and fix the defects found. Known candidate: handleDraft/handleConfirm verify the token against the LOCAL token store (`workflow.VerifyToken = tokenAuthority.Verify`), but the token is minted by the primary -> 403 "Permission denied". Branch: fix/enrollment-routes-main-api (PR #17).

### Grant exchange (needed so enrollment can complete; ships in 0.5.8, same PR #17)
Decisions (defaults adopted from the explorer, owner may override):
- D5: eligible roles to enroll: `standalone-primary`, `primary`, and a demoted node/`stale-primary` only after explicit owner confirmation (verify role semantics first).
- D6: `--enable-node-converter` defaults ON with an explicit opt-out.
- D7: the draft cache keeps the raw token in memory only, bounded by the draft expiry; never written to disk or logs.

- [x] T6 SQLite `configuration.PrimaryGrantStore` (token consume + grant atomic; BEGIN IMMEDIATE; concurrent issue -> one grant; exact retry same payload; other binding denied). SECURITY. Done: `SQLitePhase2Store.WithPrimaryGrant` in `sqlite_primary_grant.go`; no new migration; tests in `sqlite_primary_grant_test.go` (commit pending).
- [x] T7 `NodeCredentialIssuer` + `POST /api/topology/enroll` (`Authorization: Enrollment <token>`, 201 sealed payload, uniform 401 ENROLLMENT_DENIED, no-store, size/time/rate limits, wired in main.go). SECURITY. Done (commit pending): handler `httpserver/enrollment_grant_exchange.go` (in-memory fixed-window IP/selector limiter, 429 ENROLLMENT_RATE_LIMITED), issuer `cmd/server/node_credential_issuer.go` (not in enrollment: sync tests import enrollment -> cycle), wired via `newEnrollmentWorkflowTLSSupervisorWithGrantExchange`.
- [ ] T8 Node-side exchange client (same pinned TLS h2 transport) + atomic KEK/credential persistence, zeroing of secrets. SECURITY.
- [ ] T9 `handleConfirm` orchestration: ephemeral key -> exchange -> OpenBootstrap -> persist -> strict validate -> Convert; failure leaves a recoverable standalone node.
- [ ] T10 Converter flag default + role eligibility (D5, D6), tests, then real Docker E2E (draft->preview->confirm->node applies, demote->re-enroll, promote).

## Acceptance
- Promote NODE -> PRIMARY works offline and online, bumps generation, old nodes must re-enroll.
- Demote PRIMARY -> NODE leads into the enrollment preview/confirm flow.
- Writes on former primary are blocked after promotion elsewhere.

## Routes
- T1: delegated (2+ non-trivial files, mapping done by explorer).

## Progress
- Branch: feat/primary-node-role-conversion
- T1 done: commit ce6c78f (go build/vet/test green; RED observed for cluster and identity SQLite only).
- T2 done (commit 28270ae): TopologyConversionPanel + promoteTopology/demoteTopology client; 13 new vitest tests; EnrollmentTrustPanel date test fails on base (pre-existing).
- T3 done: README + PRD status updated.
- Reopened: T4 (user report: POST /api/topology/enrollment/draft -> 404 on :3000; pre-existing wiring gap, confirmed by user choice 1 = mount routes on main API).
- T5 (partial, commit c8b7e96): real Docker E2E (pcprimary + pcnode). Fixed: (1) token no longer verified against the node local store on the owner-session main API; (2) FetchSnapshot argument order swapped (url/token); (3) dedicated TLS 1.3 listener now advertises ALPN h2 (snapshot client is h2-only); (4) draft/preview/recover validate without local KEK (confirm still fails closed); (5) PRIMARY mints its cluster KEK before first token activation. E2E: draft 200, preview 200, promote 200, demote 200. BLOCKED: confirm returns 409 "primary snapshot denied" because the grant exchange (PrimaryService.IssueGrant HTTP endpoint, SQLite PrimaryGrantStore, node-side BootstrapRecipient/OpenBootstrap, KEK/credential persistence, token consumption) is not wired anywhere; the converter also requires --enable-node-converter. Needs a design/scope decision.
