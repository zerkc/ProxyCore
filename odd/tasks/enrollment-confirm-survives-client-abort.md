# Enrollment confirm survives client abort

## Objective
On a slow node (Pi 3) the first `POST /api/topology/enrollment/confirm` failed with `context canceled` after ~10s while the apply job kept running; the retry succeeded. Make the first attempt succeed.

## Evidence
- Node log: `apply_wait_failed ... msg="context canceled"`, `dur=10003ms`; retry committed.
- UI: `DashboardShell.tsx:344` mounts `<NodeContinuityView key={identityTuple}>`; identity changes during import, remount cleanup aborts the in-flight confirm (`NodeContinuityView.tsx:40-41`). Inferred, not reproduced.
- API: `enrollment_workflow.go:613` passes `r.Context()` to `Convert`.

## Tasks
- [x] T1 API: Convert runs detached from client cancellation (`context.WithoutCancel`, bounded by its own timeout), with a test where the request ctx is cancelled mid apply-wait and the conversion still commits.
- [x] T2 UI: unmount cleanup must not abort an in-flight confirm; test.
- [ ] T3 Node DNS answers the primary IP for proxied records (investigation, see notes below).
- [ ] T4 Version bump 0.6.10 in this PR.

## Acceptance
`go test` for enrollment/httpserver, `bun run typecheck/lint/test` green.
