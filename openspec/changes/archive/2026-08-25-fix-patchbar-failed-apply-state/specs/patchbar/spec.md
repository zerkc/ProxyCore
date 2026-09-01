## Purpose

PatchBar in the operator SPA (`apps/ui`) SHALL surface failed applies as a
distinct fifth state so an operator never mistakes a silent apply failure
for live or pending patch status. `/api/status` already returns
`errorMessage` and `failed` job status; this capability is UI-only.

## ADDED Requirements

### Requirement: Failed-state rendering

PatchBar SHALL render the label `Failed: <reason>` when at least one job in
`status.jobs` has `status === "failed"`, where `<reason>` is the selected
failed job's `errorMessage` (with the empty-reason fallback).

#### Scenario: Failed job shows reason

- **GIVEN** `status.jobs` contains a job with `status === "failed"` and `errorMessage === "DNS validation timed out"`
- **WHEN** PatchBar renders
- **THEN** the visible label is exactly `Failed: DNS validation timed out`

#### Scenario: No failed job never renders Failed

- **GIVEN** `status.jobs` contains no failed job
- **WHEN** PatchBar renders
- **THEN** the `Failed` label is never rendered, even if `inSync === false`

#### Scenario: Failed takes precedence over inSync

- **GIVEN** `status.jobs` contains a failed job and a non-failed job
- **WHEN** PatchBar renders
- **THEN** the `Failed` branch wins over `Patch pending` and `Patch live`

### Requirement: Auto-retry once per failed job

The dashboard SHALL auto-trigger `apply()` at most once per distinct
failed-job id, guarded by a ref so that observing the same failed job id
again on a later poll does NOT trigger another `apply()`.

#### Scenario: First observation of a failed job retries once

- **GIVEN** the latest job in `status.jobs` transitions to `failed` with id `J1`
- **WHEN** the polling hook observes `J1`
- **THEN** `apply()` is invoked exactly once and `J1` is recorded in the retry guard

#### Scenario: Same failed job is not retried again

- **GIVEN** `J1` is already recorded in the retry guard
- **WHEN** `J1` is observed again on a subsequent poll
- **THEN** `apply()` is NOT invoked

#### Scenario: Chained retry is forbidden

- **GIVEN** the auto-retry of `J1` produces another failed job `J2`
- **WHEN** `J2` is observed
- **THEN** `apply()` is NOT invoked again

#### Scenario: Distinct failed job id may retry once

- **GIVEN** a different failed job `J3` (id not in the retry guard) appears in `status.jobs`
- **WHEN** observed
- **THEN** `apply()` IS invoked exactly once for `J3`

### Requirement: Persistence until next apply

The failed state SHALL remain visible until a new `apply()` is invoked from
any caller (manual click, save record, save stream, or auto-retry);
navigating away from the dashboard and returning SHALL NOT clear it.

#### Scenario: Navigation does not clear failed state

- **GIVEN** PatchBar is showing `Failed: <reason>`
- **WHEN** the user navigates to another route and returns to the dashboard
- **THEN** PatchBar still shows `Failed: <reason>`

#### Scenario: New apply leaves the failed state

- **GIVEN** PatchBar is showing `Failed: <reason>`
- **WHEN** `apply()` is invoked from any caller
- **THEN** PatchBar transitions out of the failed state on the next poll that observes a non-failed job

#### Scenario: Idle time does not clear failed state

- **GIVEN** PatchBar is showing `Failed: <reason>`
- **WHEN** 30 s pass with no operator action
- **THEN** PatchBar still shows `Failed: <reason>`

#### Scenario: Failed state outlasts auto-retry

- **GIVEN** the auto-retry fires
- **WHEN** it completes with another `failed` job
- **THEN** PatchBar remains in the failed state

### Requirement: Disable manual apply during auto-retry

While an auto-retry `apply()` is in flight (between dispatch and the first
observation of the resulting job in `status.jobs`), the dashboard's manual
`Apply now` / `Re-apply` control SHALL be `disabled` so that a double-click
cannot enqueue a third apply for the same revision.

#### Scenario: Button disabled while auto-retry in flight

- **GIVEN** the dashboard has just auto-triggered `apply()` for failed job `J1` and the resulting job has not yet appeared in `status.jobs`
- **WHEN** the operator clicks `Apply now`
- **THEN** the control is `disabled` and no second `apply()` is dispatched

#### Scenario: Button re-enabled after queued job appears

- **GIVEN** the auto-retry has finished and a new job with status `queued` / `validating` / `applying` is visible in `status.jobs`
- **WHEN** the operator clicks `Apply now`
- **THEN** the control is enabled per the existing rules

#### Scenario: Existing debounce still applies when idle

- **GIVEN** no auto-retry is in flight
- **WHEN** the operator clicks `Apply now` twice in 200 ms
- **THEN** existing click-debounce/idempotency guarantees at most one user-initiated `apply()`

### Requirement: Multiple-failed-jobs ordering

When `status.jobs` contains more than one job with `status === "failed"`,
PatchBar SHALL pick the newest failed job by `createdAt` desc as the source
of `failureReason`. Identical `createdAt` values SHALL tie-break by `id`
desc so the choice is deterministic across renders.

#### Scenario: Newest createdAt wins

- **GIVEN** `status.jobs` contains failed jobs `J1` (createdAt = T1) and `J2` (createdAt = T2, T2 > T1)
- **WHEN** PatchBar renders
- **THEN** `failureReason` equals `J2.errorMessage`

#### Scenario: Reorder does not flicker

- **GIVEN** `status.jobs` contains three failed jobs with strictly increasing `createdAt`
- **WHEN** a new poll reorders them
- **THEN** the bar continues to render the newest one's `errorMessage` without flicker

#### Scenario: Identical createdAt is deterministic

- **GIVEN** `status.jobs` contains failed jobs with identical `createdAt`
- **WHEN** PatchBar renders
- **THEN** the newest `id` desc is selected consistently across renders

### Requirement: Empty errorMessage fallback

When the selected failed job's `errorMessage` is empty (null, undefined, or
`""`), PatchBar SHALL display the literal fallback string `Apply failed` so
the rendered label is exactly `Failed: Apply failed`.

#### Scenario: Empty string uses fallback

- **GIVEN** the selected failed job has `errorMessage === ""`
- **WHEN** PatchBar renders
- **THEN** the label is exactly `Failed: Apply failed`

#### Scenario: Null uses fallback

- **GIVEN** the selected failed job has `errorMessage === null`
- **WHEN** PatchBar renders
- **THEN** the label is exactly `Failed: Apply failed`

#### Scenario: Present reason is used as-is

- **GIVEN** the selected failed job has `errorMessage === "DNS timeout"`
- **WHEN** PatchBar renders
- **THEN** the label is `Failed: DNS timeout`

### Requirement: No optimistic update on failure

PatchBar SHALL NOT transition to `Failed` until the polling hook has
observed the failed job from `/api/status`; a transient error thrown by
`apply()` SHALL NOT cause an optimistic `Failed` render.

#### Scenario: Network error does not render Failed

- **GIVEN** the operator clicks `Apply now` and `apply()` throws a network error before any subsequent `/api/status` response carries a failed job
- **WHEN** PatchBar re-renders
- **THEN** the label is NOT `Failed: ...`

#### Scenario: Observed failed job renders Failed

- **GIVEN** a worker records a failed job within 1 s
- **WHEN** the next `/api/status` poll lands (≤ 3 s later)
- **THEN** PatchBar shows `Failed: <reason>`

#### Scenario: Applying job flipping to failed transitions on that poll

- **GIVEN** PatchBar is in the `Apply in progress` state
- **WHEN** that job completes `failed` per `/api/status`
- **THEN** the bar transitions to `Failed: <reason>` on that same poll cycle
