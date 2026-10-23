# Hide enrollment on a linked node

## Objective
Once a NODE is linked to a PRIMARY, the dashboard must not show the "Connect this NODE" enrollment form, so the user is not confused into enrolling again.

## Evidence
- `apps/ui/src/dashboard/DashboardShell.tsx:342-347`: for `role === "node"` the shell always renders `<NodeContinuityView key={identityTuple} />`, which restarts at the entry form (token + PRIMARY URL) on every identity change.
- `apps/ui/src/dashboard/types.ts:105-119`: the topology sync status already exposes `bound` and `unlinked`; only `NodeSyncStatus` fetches it today.

## Decision (owner-confirmed)
Linked = `bound === true && unlinked !== true`. Linked: hide `NodeContinuityView`; keep `NodeSyncStatus` and `TopologyConversionPanel`. Not bound, or unlinked by the PRIMARY: show the enrollment form so the node can be re-enrolled. While the status is loading, do not render the form (no flash). If the status cannot be loaded, keep the current behavior (show the form).

## Tasks
- [x] T1 Expose the sync status to the shell (shared hook or lifted state) and gate `NodeContinuityView` on the linked rule; tests for linked / unbound / unlinked / loading / load-failure.
- [x] T2 Keep `NodeSyncStatus` behavior and tests intact.

## Routing
Delegated direct: one writer (2+ non-trivial files).

## Acceptance
`bun run typecheck`, `bun run lint`, and the UI tests for `DashboardShell`, `NodeSyncStatus`, `NodeContinuityView` green.

## Progress
T1-T2 done (delegated writer; RED: 2 gate tests failed with the gate disabled, then GREEN). `bun run typecheck`, `bun run lint`, full `bun run test` (349 passed, 7 skipped) green; parent re-ran typecheck and the 3 dashboard test files (42 passed).
