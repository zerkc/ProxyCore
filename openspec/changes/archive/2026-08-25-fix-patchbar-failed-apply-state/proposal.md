# Proposal: PatchBar — register apply failures (5th state) instead of stuck loading

## Intent

Make PatchBar surface failed applies as `Failed: <reason>` and keep that state
visible until the operator starts a new apply, so an operator never sees a
loading bar that is in fact a silent failure.

## Scope

### In scope

- `apps/ui/src/dashboard/types.ts` — add `errorMessage?: string | null` to the
  `StatusPayload.jobs[number]` shape.
- `apps/ui/src/dashboard/dashboard-context.tsx` — derive `failedJob` and
  `failureReason` from `status?.jobs`; track a per-job `retriedJobRef`; trigger
  one auto-retry through the existing `apply()` helper; expose
  `failureReason` and `failedJobId` on the context; clear local failure memory
  inside `apply()` so a fresh apply resets the bar.
- `apps/ui/src/dashboard/DashboardShell.tsx` — extend the PatchBar label
  machine from 4 to 5 states (add `Failed: <reason>`), pipe `failureReason`
  and a new `onRetryFailed` prop, and gate the `Apply now` / `Re-apply`
  button so it still works while failed.
- `apps/ui/src/dashboard/DashboardShell.tsx::PatchBar` — render the reason
  inline and keep the bar in the failed state until the next `apply()`.

### Out of scope

- API surface: `/api/status` already returns `errorMessage` and the
  `failed` job status (verified `apps/api/internal/configuration/types.go:25`
  and `apps/api/internal/httpserver/config_handlers.go:56`). No server
  contract change.
- Worker (`apps/worker`): job lifecycle, retry policy, and `errorMessage`
  persistence stay as-is.
- DB schema (`packages/db/src/schema.ts:43-50`): `failed` already exists in
  the `proxycore_job_status` enum.
- Any new color/icon system: reuse existing `pc-sync-bar` / `text-signal`
  tokens; a `data-failed="true"` attribute is enough to color the dot.
- New copy outside the bar.

## Root cause summary

Cite: Engram observation **#2750** `[discovery] PatchBar failed apply root cause`.

- `apps/ui/src/dashboard/DashboardShell.tsx:188-195` — `label` ternary has no
  branch for failed; falls through to "Patch pending" (drift) or "Patch live"
  depending on revision checksum equality.
- `apps/ui/src/dashboard/DashboardShell.tsx:58-62` — `pendingJob` only matches
  `["queued","validating","applying"]`; a `failed` job returns `false`, so
  `waiting` stays driven by `!inSync` and the bar looks busy even after the
  worker is done.
- `apps/ui/src/dashboard/dashboard-context.tsx:78-81` — `inSync` is checksum
  equality, so a partial apply (desired advanced, applied stuck) reads as
  drift, masking the failure.
- `apps/ui/src/dashboard/types.ts:34-39` — `jobs` type omits
  `errorMessage`, so the bar could not read the reason even if it wanted to.

## Proposed change

### 5-state label machine (DashboardShell.tsx PatchBar)

```
!loaded                                 → "Checking patch state"
pendingJob  (queued/validating/applying)→ "Apply in progress"
failureReason loaded                    → `Failed: ${failureReason}`
inSync && loaded                        → "Patch live"
!inSync && loaded                       → "Patch pending"
```

The `Failed` branch takes precedence over `Patch pending` so a failed job
that leaves desired ≠ applied reads as failed, not as drift.

### `pendingJob` and new `failedJob` derivation

Compute both in `DashboardShell.tsx` so the context stays the single source
of truth for fetched state:

```ts
const pendingJob = status?.jobs.some(j =>
  ["queued","validating","applying"].includes(j.status)) ?? false;

const failedJob  = status?.jobs.find(j => j.status === "failed");
const failureReason = failedJob?.errorMessage ?? "Apply failed";
```

`failedJob` is the *latest* failed job; the spec will pin this down.

### Auto-retry-once (UI-only)

In `dashboard-context.tsx`:

- `useRef<string | null>(null)` `retriedJobRef` — remembers the ID of the
  failed job we already retried.
- `useEffect` watching `status?.jobs`: when a `failed` job appears whose ID
  is not in `retriedJobRef`, set the ref and call `apply()` once.
- `apply()` sets a local `suppressFailure` flag for the freshly created job
  so the effect doesn't immediately treat the new job as a second failure.
- If the new job also fails, `retriedJobRef` is updated again but `apply()`
  is NOT called (counter / set logic prevents a second retry).
- Failure memory is reset inside `apply()` on every explicit invocation
  (manual click or upstream `mutate("/api/apply", …)` call), satisfying
  the "stays visible until new apply" rule.

### Persistence of the failed state

`failureReason` is derived, but the *visible* failed label is anchored to
`apply()` invocation: clearing local `retriedJobRef` inside `apply()` plus
the 3 s poll naturally rolls the bar out of `Failed` once a new job enters
`queued`/`validating`/`applying`. No localStorage, no extra state slot.

### Reason flow

`/api/status` → `JobRecord.ErrorMessage *string` (`omitempty`) → already
serialized by the Go handler → typed in TS as `errorMessage?: string |
null` → consumed by PatchBar as `failureReason`.

## Acceptance criteria

- Given an apply that fails, when the operator views PatchBar, then it
  shows `Failed: <reason>` (where reason is `job.errorMessage`) and stays
  there.
- Given a failed apply and one auto-retry, when the retry also fails, then
  the bar still shows `Failed: <reason>` and does **not** trigger another
  auto-retry.
- Given a failed apply, when the operator triggers any new apply (manual
  click, save record, save stream, etc.), then PatchBar transitions out of
  the failed state.
- Given a successful apply, PatchBar behaves exactly as it does today
  (Checking → Apply in progress → Patch live / Patch pending).
- Given no failed job in `status.jobs`, the `Failed` branch is never
  rendered, even if `inSync === false`.

## Risks and tradeoffs

| Risk | Sev | Mitigation |
|------|-----|-----------|
| Auto-retry races the worker: if the worker is mid-claim we queue a second apply. | Med | Worker already de-dupes on revision id; the new apply targets the same desired revision, so the second run is a no-op beyond logging. |
| Operator double-click on `Apply now` while failed: could trigger a third apply. | Low | Button is gated by `!loaded` only today; spec will add `disabled` while the auto-retry is in flight. |
| 3 s poll latency between failure and bar update. | Low | Acceptable — same latency already exists for any status transition. Spec may allow a manual `Refresh` shortcut. |
| Other components that read `useDashboard()` (none currently consume `inSync` outside PatchBar per the explore grep). | Low | New fields are additive; no consumer breaks. |
| `errorMessage` may be empty when the worker fails before logging. | Low | Fallback string `"Apply failed"` is the proposal's default; spec confirms. |

## Open questions for `sdd-spec`

1. Should the `Failed` label include the job's `target` (e.g.
   `Failed (nginx): <reason>`) or just the reason? The current proposal says
   reason only.
2. If multiple failed jobs exist, which one wins — most recent (highest
   `createdAt`) or most recent in the visible `jobs` slice? Spec must pin
   this to avoid flapping.
3. Should the auto-retry respect a server-side cooldown, or is the
   `retriedJobRef` enough? Server doesn't expose cooldown; ref is enough.
4. Does the operator need a manual "retry" affordance beyond auto-retry, or
   is `Apply now` enough? Current proposal: no new button; `Apply now` keeps
   working while failed.
5. Should `data-failed="true"` on `pc-sync-bar` change color, or just
   inherit `text-signal` like other drift states? Current proposal: inherit;
   no design-system change.

## Non-goals

- No backend, worker, or DB changes.
- No new design tokens, colors, icons, or copy surfaces.
- No persistent (localStorage) failure memory across reloads.
- No change to apply semantics, retry policy, or job lifecycle.
- No new endpoints, no telemetry, no audit-log surface.

## Affected Areas

| Area | Impact | Description |
|------|--------|-------------|
| `apps/ui/src/dashboard/types.ts` | Modified | Add `errorMessage?: string \| null` to `StatusPayload.jobs[number]`. |
| `apps/ui/src/dashboard/dashboard-context.tsx` | Modified | Derive failed job, auto-retry once, expose new context fields, reset on new apply. |
| `apps/ui/src/dashboard/DashboardShell.tsx` | Modified | Extend PatchBar label machine to 5 states; render reason. |
| `apps/ui/src/dashboard/DashboardShell.tsx::PatchBar` | Modified | Accept `failureReason` and `data-failed` attribute. |

## Rollback plan

Revert the three files above. The previous 4-state machine and
`pendingJob` derivation are restored verbatim; `errorMessage` becomes an
unused optional field. No server, worker, or DB rollback needed because no
upstream contract changed.

## Success criteria

- PatchBar never shows `Apply in progress` or `Patch pending` when the
  latest job is `failed`.
- `Failed: <reason>` label persists across polls until the next `apply()`.
- A single auto-retry fires once per failed job, never twice in a row.
- Existing successful-apply flows are byte-identical to today.
- `apps/ui` production build passes; no new console warnings.