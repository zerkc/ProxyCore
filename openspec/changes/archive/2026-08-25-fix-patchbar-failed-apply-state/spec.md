# Spec — fix-patchbar-failed-apply-state

> Status: draft
> Phase: spec
> Source proposal: Engram #2751 (`sdd/fix-patchbar-failed-apply-state/proposal`)
> Source explore: Engram #2750 (`sdd/fix-patchbar-failed-apply-state/explore`)

## 1. Purpose

PatchBar in `apps/ui/src/dashboard/DashboardShell.tsx` currently has no state for failed applies. After a failed apply, the bar falls through to "Patch pending" or "Patch live" depending on revision checksums, leaving the operator staring at what looks like a live or pending bar that is in fact a silent failure. This delta introduces a 5th state (`Failed: <reason>`), persists it until a new apply starts, and adds a single auto-retry on entry into the failed state. Scope is PatchBar in `apps/ui` only — `/api/status` already returns `errorMessage` and the `failed` job status, so no API, worker, or DB change is required.

## 2. Requirements

### REQ-1: Failed-state rendering

- **Statement:** PatchBar SHALL render the label `Failed: <reason>` when at least one job in `status.jobs` has `status === "failed"`, where `<reason>` is the selected failed job's `errorMessage` (with the empty-reason fallback in REQ-6).
- **Rationale:** Operators must see that an apply failed and why, instead of a misleading "Patch pending" or "Patch live" state.
- **Scenarios:**
  1. Given `status.jobs` contains a job with `status === "failed"` and `errorMessage === "DNS validation timed out"`, when PatchBar renders, then the visible label is exactly `Failed: DNS validation timed out`.
  2. Given `status.jobs` contains no failed job, when PatchBar renders, then the `Failed` label is never rendered, even if `inSync === false`.
  3. Given `status.jobs` contains a failed job and a non-failed job, when PatchBar renders, then the `Failed` branch wins over `Patch pending` and `Patch live` (failed takes precedence over `inSync`).

### REQ-2: Auto-retry once per failed job

- **Statement:** The dashboard SHALL auto-trigger `apply()` at most once per distinct failed-job id, guarded by a ref so that observing the same failed job id again on a later poll does NOT trigger another `apply()`.
- **Rationale:** A single auto-retry covers transient worker hiccups without spamming the system with retries or racing the worker on the same revision.
- **Scenarios:**
  1. Given the latest job in `status.jobs` transitions to `failed` with id `J1`, when the polling hook observes `J1`, then `apply()` is invoked exactly once and `J1` is recorded in the retry guard.
  2. Given `J1` is already recorded in the retry guard, when `J1` is observed again on a subsequent poll, then `apply()` is NOT invoked.
  3. Given the auto-retry of `J1` produces another failed job `J2`, when `J2` is observed, then `apply()` is NOT invoked again (no chained retry; auto-retry is strictly one-per-distinct-failed-job-id).
  4. Given a different failed job `J3` (id not in the retry guard) appears in `status.jobs`, when observed, then `apply()` IS invoked exactly once for `J3`.

### REQ-3: Persistence until next apply

- **Statement:** The failed state SHALL remain visible until a new `apply()` is invoked from any caller (manual click, save record, save stream, or auto-retry); navigating away from the dashboard and returning SHALL NOT clear it.
- **Rationale:** Operators need persistent failure feedback so they can act, not a flicker that hides the failure across navigation or poll cycles.
- **Scenarios:**
  1. Given PatchBar is showing `Failed: <reason>`, when the user navigates to another route and returns to the dashboard, then PatchBar still shows `Failed: <reason>`.
  2. Given PatchBar is showing `Failed: <reason>`, when `apply()` is invoked from any caller, then PatchBar transitions out of the failed state on the next poll that observes a non-failed job (e.g., `queued`, `validating`, `applying`, `succeeded`).
  3. Given PatchBar is showing `Failed: <reason>`, when 30 s pass with no operator action, then PatchBar still shows `Failed: <reason>`.
  4. Given the auto-retry of REQ-2 fires, when it completes, then PatchBar remains in the failed state if the new job is also `failed` (the failed state must outlast the auto-retry attempt itself).

### REQ-4: Disable manual apply during auto-retry

- **Statement:** While an auto-retry `apply()` is in flight (between dispatch and the first observation of the resulting job in `status.jobs`), the dashboard's manual `Apply now` / `Re-apply` control SHALL be `disabled` so that a double-click cannot enqueue a third apply for the same revision.
- **Rationale:** A double-click on `Apply now` while the auto-retry is still running must not produce a third apply for the same revision id.
- **Scenarios:**
  1. Given the dashboard has just auto-triggered `apply()` for failed job `J1` and the resulting job has not yet appeared in `status.jobs`, when the operator clicks `Apply now`, then the control is `disabled` and no second `apply()` is dispatched.
  2. Given the auto-retry has finished and a new job with status `queued` / `validating` / `applying` is visible in `status.jobs`, when the operator clicks `Apply now`, then the control is enabled per the existing rules.
  3. Given no auto-retry is in flight, when the operator clicks `Apply now` twice in 200 ms, then the existing click-debounce/idempotency guarantees at most one user-initiated `apply()` (REQ-2 + REQ-4 combine to guarantee ≤ 1 auto + ≤ 1 manual user apply per failed job id).

### REQ-5: Multiple-failed-jobs ordering

- **Statement:** When `status.jobs` contains more than one job with `status === "failed"`, PatchBar SHALL pick the newest failed job by `createdAt` desc as the source of `failureReason`.
- **Rationale:** Prevents the bar from flapping between different failure reasons as polls return, and guarantees the most recent failure wins.
- **Scenarios:**
  1. Given `status.jobs` contains failed jobs `J1` (createdAt = T1) and `J2` (createdAt = T2, T2 > T1), when PatchBar renders, then `failureReason` equals `J2.errorMessage`.
  2. Given `status.jobs` contains three failed jobs with strictly increasing `createdAt`, when a new poll reorders them, then the bar continues to render the newest one's `errorMessage` without flicker between two different reasons.
  3. Given `status.jobs` contains failed jobs with identical `createdAt`, when PatchBar renders, then the choice is deterministic across renders (any consistent tie-break is acceptable for the spec).

### REQ-6: Empty errorMessage fallback

- **Statement:** When the selected failed job's `errorMessage` is empty (null, undefined, or `""`), PatchBar SHALL display the literal fallback string `Apply failed` so the rendered label is exactly `Failed: Apply failed`.
- **Rationale:** An empty reason would render as `Failed: `, which is uglier and less informative than a deterministic fallback.
- **Scenarios:**
  1. Given the selected failed job has `errorMessage === ""`, when PatchBar renders, then the label is exactly `Failed: Apply failed`.
  2. Given the selected failed job has `errorMessage === null`, when PatchBar renders, then the label is exactly `Failed: Apply failed`.
  3. Given the selected failed job has `errorMessage === "DNS timeout"`, when PatchBar renders, then the label is `Failed: DNS timeout` (fallback string is unused).

### REQ-7: No optimistic update on failure

- **Statement:** PatchBar SHALL NOT transition to `Failed` until the polling hook has observed the failed job from `/api/status`; a transient error thrown by `apply()` (e.g., network failure) SHALL NOT cause an optimistic `Failed` render.
- **Rationale:** The 3 s poll latency is acceptable and avoids misleading bars when `apply()` itself errors before the worker has recorded anything.
- **Scenarios:**
  1. Given the operator clicks `Apply now` and `apply()` throws a network error before any subsequent `/api/status` response carries a failed job, when PatchBar re-renders, then the label is NOT `Failed: ...` (no optimistic failure render).
  2. Given a worker records a failed job within 1 s, when the next `/api/status` poll lands (≤ 3 s later), then PatchBar shows `Failed: <reason>`.
  3. Given PatchBar is in the `Apply in progress` state (a `queued` / `validating` / `applying` job exists), when that job completes `failed` per `/api/status`, then the bar transitions to `Failed: <reason>` on that same poll cycle.

## 3. Non-functional requirements

- No new dependencies in `apps/ui/package.json`.
- The fix MUST NOT change the `/api/status` payload shape.
- TypeScript strict mode stays green (`pnpm -F ui tsc --noEmit`).
- Production build (`pnpm -F ui build`) MUST pass with no new console warnings.
- No design-token additions: reuse existing `pc-sync-bar` / `text-signal` classes; a `data-failed="true"` attribute on the bar is sufficient for theming.
- Existing successful-apply flows SHALL be byte-identical to today (no regression in `Checking` → `Apply in progress` → `Patch live` / `Patch pending` for non-failed jobs).

## 4. Out of scope

- API surface: `/api/status` already returns `errorMessage` and the `failed` job status. No server contract change.
- Worker (`apps/worker`): job lifecycle, retry policy, and `errorMessage` persistence stay as-is.
- DB schema: `failed` is already a valid value in the `proxycore_job_status` enum.
- New color / icon system or design tokens.
- New copy surfaces outside the bar.
- localStorage persistence of failure memory across full reloads.
- New apply endpoints, telemetry, or audit-log surface.
- Worker-side retry policy changes (the auto-retry in REQ-2 is purely UI-triggered).

## 5. Open questions for `sdd-design`

1. Exact storage mechanism for the retry guard in REQ-2: `useRef<string[]>` in `dashboard-context.tsx` vs a module-level `Map<jobId, true>` keyed on job id. Spec requires ≤ 1 auto-retry per failed job id but does not pin the mechanism.
2. Exact hook return shape for failed-state consumers: separate `failureReason: string` and `failedJobId: string` fields vs a single `failedJob: JobRecord | null` object. Spec treats either as acceptable.
3. Whether the auto-retry `disabled` window in REQ-4 applies only to the dashboard's `Apply now` button or to all `apply()` callers (save record, save stream). Spec assumes the dashboard button at minimum; design should confirm whether other callers can race the auto-retry and, if so, where the dedup lives.
4. Tie-break rule for REQ-5 scenario 3 when `createdAt` is identical across multiple failed jobs. Spec accepts any deterministic tie-break; design should pick one (e.g., highest `id`, or first in array order) and pin it.
5. Whether the `Failed` label should include the job's `target` (e.g., `Failed (nginx): <reason>`) or just the reason. Proposal currently says reason only; design should confirm or extend.
6. Whether REQ-7 scenario 1 (network error from `apply()`) should surface a separate transient indicator in the bar (e.g., a non-blocking "Apply request failed, will retry on next poll") or remain silent. Spec currently requires silent; design should confirm.