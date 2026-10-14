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
