# Design — fix-patchbar-failed-apply-state

> Status: draft
> Phase: design
> Source proposal: Engram #2751 (`sdd/fix-patchbar-failed-apply-state/proposal`)
> Source spec: Engram #2752 (`sdd/fix-patchbar-failed-apply-state/spec`)
> Source explore: Engram #2750 (`sdd/fix-patchbar-failed-apply-state/explore`)
> Mirror: `openspec/changes/fix-patchbar-failed-apply-state/design.md`

## 1. Overview

The change makes `PatchBar` (in `apps/ui/src/dashboard/DashboardShell.tsx`) honest about apply failures by adding a fifth state — `Failed: <reason>` — and auto-retries once per distinct failed job id so a transient worker hiccup self-heals. The change is intentionally **PatchBar-only in `apps/ui`**: `/api/status` already returns `errorMessage` (`apps/api/internal/configuration/types.go:25`) and the `failed` job status (`apps/api/internal/httpserver/config_handlers.go:56`), so no API, worker, or DB contract change is required. The worker de-dupes on revision id, so the UI-triggered auto-retry is safe and idempotent at the system level.

The architectural delta is small but deliberate: a derived `failedJob` and `failureReason` are added to `DashboardContextValue` so every consumer (today only `DashboardShell`, but tomorrow potentially `IngressView` or a future toast) reads the same source of truth; a single `useRef` inside `DashboardProvider` enforces the once-per-failed-job-id auto-retry; and the 4-branch `label` ternary in `PatchBar` gains a fifth branch that takes precedence over both `Patch pending` and `Patch live`. A new pure module `apps/ui/src/dashboard/patch-bar-state.ts` carries the derivation logic so it can be unit-tested without React or DOM, which sidesteps the fact that `apps/ui` currently has **no test infrastructure installed** (no `vitest`, no `testing-library`, no happy-dom).

## 2. Component map

```
+--------------------------------------------------------------------+
| apps/ui/src/dashboard/dashboard-context.tsx (DashboardProvider)    |
|                                                                    |
|  - state: status, autoRetrying                                     |
|  - ref:   autoRetriedJobIdRef                                      |
|  - effect: every 3 s tick → refresh() → setStatus(...)             |
|  - NEW effect: on status.jobs change →                              |
|       if selectFailedJob(jobs) exists and                          |
|          its id is not in autoRetriedJobIdRef:                     |
|         set ref, set autoRetrying = true, call autoRetryApply()    |
|  - exposes via context: status, inSync, failedJob,                 |
|       failureReason, autoRetrying, apply(), ...                    |
+--------------------------------------------------------------------+
                  | useDashboard()
                  v
+--------------------------------------------------------------------+
| apps/ui/src/dashboard/DashboardShell.tsx                          |
|                                                                    |
|  - destructure new context fields (failedJob, failureReason,       |
|       autoRetrying)                                                |
|  - pass new props to <PatchBar>                                    |
|                                                                    |
|  PatchBar (lines 173-252)                                          |
|  - receives new props: failureReason, autoRetrying                 |
|  - 4-branch label ternary → 5-branch via deriveBarState()          |
|  - data-failed attribute on .pc-sync-bar                           |
|  - button.disabled = !loaded || autoRetrying                       |
+--------------------------------------------------------------------+

+--------------------------------------------------------------------+
| NEW apps/ui/src/dashboard/patch-bar-state.ts (pure, no React)      |
|                                                                    |
|  - selectFailedJob(jobs): pick newest failed by createdAt desc,   |
|       tie-break by id desc                                         |
|  - deriveBarState({ loaded, inSync, pendingJob, failureReason })   |
|       → BarState                                                    |
+--------------------------------------------------------------------+
```

The polling loop, the `apply()` helper, the `mutate("/api/apply", ...)` callers, and the `refresh()` chain are **not** restructured. Only a new effect and a new `useRef` are added inside `DashboardProvider`. The apply trigger file (`dashboard-context.tsx` is the only one — `apply` is not split out) gains one private helper `autoRetryApply()` that calls `mutate("/api/apply", ...)` directly without resetting the retry guard.

## 3. State machine

### Discriminated union (the new input contract for the bar)

```ts
// apps/ui/src/dashboard/patch-bar-state.ts
export type BarState =
  | { kind: "checking" }
  | { kind: "applying" }
  | { kind: "failed"; reason: string }
  | { kind: "pending" }
  | { kind: "live" };

export function deriveBarState(input: {
  loaded: boolean;
  pendingJob: boolean;
  inSync: boolean;
  failureReason?: string | undefined; // present ⇔ a failed job exists
}): BarState {
  if (!input.loaded) return { kind: "checking" };
  if (input.pendingJob) return { kind: "applying" };
  if (input.failureReason) return { kind: "failed", reason: input.failureReason };
  if (input.inSync) return { kind: "live" };
  return { kind: "pending" };
}
```

### Transition table (all inputs → outputs)

| loaded | pendingJob | failureReason (failed job exists) | inSync | `kind`           | Visible label             |
| :----: | :--------: | :-------------------------------: | :----: | :--------------: | :------------------------ |
| false  | any        | any                               | any    | `checking`       | `Checking patch state`    |
| true   | true       | any                               | any    | `applying`       | `Apply in progress`       |
| true   | false      | `"DNS validation timed out"`      | false  | `failed`         | `Failed: DNS validation timed out` |
| true   | false      | `"DNS validation timed out"`      | true   | `failed`         | `Failed: DNS validation timed out` |
| true   | false      | `undefined` (no failed job)       | true   | `live`           | `Patch live`              |
| true   | false      | `undefined` (no failed job)       | false  | `pending`        | `Patch pending`           |

Failed takes precedence over both `pending` and `live` to satisfy REQ-1.3.

### Helper for selecting the failed job (pure)

```ts
export type JobLike = {
  id: string;
  status: string;
  createdAt: string;
  errorMessage?: string | null | undefined;
};

export function selectFailedJob(jobs: readonly JobLike[] | undefined): JobLike | undefined {
  if (!jobs) return undefined;
  let best: JobLike | undefined;
  let bestCreated = -Infinity;
  for (const j of jobs) {
    if (j.status !== "failed") continue;
    const t = Date.parse(j.createdAt);
    if (Number.isNaN(t)) continue;
    if (
      t > bestCreated ||
      (t === bestCreated && best !== undefined && j.id > best.id)
    ) {
      best = j;
      bestCreated = t;
    }
  }
  return best;
}
```

Tie-break rule for REQ-5 / Q4: `createdAt` desc first; on identical `createdAt`, `id` desc as a deterministic fallback. `id > best.id` is a string comparison that is stable across renders without depending on array order or V8 sort quirks. REQ-5 S2.3 ("no flicker between two different reasons across reorders") is satisfied because (a) we always pick the newest createdAt, and (b) identical createdAt resolves to a fixed id, so the same input produces the same output.

### Persistence of the failed label

The `Failed` label is derived from `status.jobs`, which the polling loop refreshes every 3 s. As long as the failed job stays in `status.jobs` (which it does until either a new apply supersedes it or the worker prunes it — neither is on this change's path), the label persists across polls, navigations, and reloads within the SPA session. The only state we own is `autoRetriedJobIdRef`, which is **not** part of persistence — it gates the auto-retry, not the visibility of the failed label.

## 4. Auto-retry mechanism

### Storage location (Q2)

`useRef<string | null>(null)` named `autoRetriedJobIdRef`, declared **inside `DashboardProvider`**, not in module scope, not in `DashboardShell`, not exposed via context.

Why `useRef` and not `useState`:
- Mutating a ref does not trigger a re-render. The retry decision is a side effect on observed data, not something the UI itself reads.
- The value must persist across renders within the same mount (so an effect can read "what was the last id I retried?") but does not need to persist across remounts (a fresh mount = a fresh ref = auto-retry is allowed again, which is the safe default).
- A module-level `Map<jobId, true>` would leak across HMR reloads in dev and across tests, which is exactly what REQ-2 forbids.

Why inside `DashboardProvider` and not `DashboardShell`:
- The polling loop that observes `status.jobs` already lives in `DashboardProvider`. Putting the effect next to the data it watches keeps the trigger colocated with the source.
- `DashboardShell` is purely presentational in this codebase's pattern; pushing side effects into it would invert the layering.

### Guard logic (REQ-2)

```ts
const autoRetriedJobIdRef = useRef<string | null>(null);
const [autoRetrying, setAutoRetrying] = useState(false);

useEffect(() => {
  const failed = selectFailedJob(status?.jobs);
  if (!failed) return;
  if (autoRetriedJobIdRef.current === failed.id) return; // already retried

  autoRetriedJobIdRef.current = failed.id;
  setAutoRetrying(true);
  void autoRetryApply();
}, [status?.jobs]);
```

`autoRetryApply()` is a private helper in `DashboardProvider` that calls `mutate("/api/apply", {}, "Apply queued (auto-retry)")` **without** resetting `autoRetriedJobIdRef`. This is what makes REQ-2 S2.3 ("no chained retry when the auto-retry itself fails") work: even though `autoRetrying` will eventually flip back to `false` when a new poll sees a newer job (failed or not), `autoRetriedJobIdRef` still equals the original `failed.id`, and the next observation of the same id (or the chained `J2`) does not retrigger because:
- If J2 is observed while J1 is still the latest failed → `selectFailedJob` still returns J1 → guard rejects (J1 already in ref).
- If J2 supersedes J1 → `selectFailedJob` returns J2 → guard check: `autoRetriedJobIdRef.current === J2.id` is false (ref holds J1) → BUT the auto-retry already happened; what stops the trigger?

The fix: also short-circuit when the failed job's `createdAt` is older than the most recent job's `createdAt` of any status. I.e., the auto-retry only fires once per "failure episode", and an episode is bounded by a newer job appearing. Once J2 appears, the episode ends regardless of J2's status.

```ts
useEffect(() => {
  if (!status?.jobs) return;
  const failed = selectFailedJob(status.jobs);
  if (!failed) return;
  if (autoRetriedJobIdRef.current === failed.id) return;

  // Has any newer job (queued/validating/applying/succeeded/failed)
  // appeared since we last retried? If yes, a fresh apply is in flight
  // and this episode is over.
  const newerExists = status.jobs.some(
    (j) => Date.parse(j.createdAt) > Date.parse(failed.createdAt),
  );
  if (newerExists) return;

  autoRetriedJobIdRef.current = failed.id;
  setAutoRetrying(true);
  void autoRetryApply();
}, [status?.jobs]);
```

This also handles REQ-2 S2.4 cleanly: when a user-initiated `apply()` succeeds and produces J3, J3's `createdAt` is newer than the previously failed job's → the guard rejects automatically on the next observation. Then if J3 also fails, the effect runs again with `failed = J3`, the ref is `null` (was reset in `apply()`), `newerExists` is false → auto-retry fires for J3.

### Reset on every `apply()` (REQ-3)

```ts
async function apply() {
  autoRetriedJobIdRef.current = null; // reset guard so the next failure can be retried
  setAutoRetrying(false);
  await mutate("/api/apply", {}, "Apply queued");
}
```

`autoRetrying = false` here too so the manual button is enabled immediately after the operator clicks (and will re-disable itself on the next poll if a newer non-failed job hasn't appeared yet, satisfying REQ-4 S2.1).

### How REQ-4 (disabled manual button during auto-retry) is satisfied

`PatchBar` receives `autoRetrying: boolean` as a new prop. The button's `disabled` is:

```tsx
<button
  type="button"
  onClick={onApply}
  disabled={!loaded || autoRetrying}
  ...
>
```

`autoRetrying` is flipped back to `false` in the auto-retry effect when a newer job appears (a separate micro-effect is unnecessary — the same effect re-runs on the next `status?.jobs` change and we re-derive `autoRetrying` from a memo):

```ts
const autoRetrying = useMemo(() => {
  if (!status?.jobs) return false;
  const failed = selectFailedJob(status.jobs);
  if (!failed) return false;
  if (autoRetriedJobIdRef.current !== failed.id) return false;
  // We are the job that triggered the auto-retry. Are we still in flight?
  const newerExists = status.jobs.some(
    (j) => Date.parse(j.createdAt) > Date.parse(failed.createdAt),
  );
  return !newerExists;
}, [status?.jobs]);
```

This is simpler than dual effects: one memo computes the flag, one effect dispatches the auto-retry. Both read `autoRetriedJobIdRef.current` to stay in sync. The ref itself is **not** a state value, so changing it does not re-render — but it doesn't need to, because the memo recomputes whenever `status?.jobs` changes (which is when the auto-retry decision needs to be re-evaluated).

## 5. Persistence

REQ-3 says the failed state persists until the next `apply()`. **No `localStorage`, no `sessionStorage`, no extra React state slot for the failed label itself.** Persistence is achieved by the data:

1. `/api/status` returns the `failed` job with `errorMessage` until the worker prunes it or until a new apply supersedes it (creates a job with a newer `createdAt`).
2. The polling loop calls `refresh()` every 3 s and stores the result in `status` state.
3. `selectFailedJob(status.jobs)` returns the failed job (or undefined) on every render.
4. PatchBar's label is a pure function of that.

So the failed label is durable across navigations within the SPA (the provider does not unmount when `DashboardShell` children re-route), across polls (3 s loop), and across the 30 s timeout window specified in REQ-3 S2.3. It naturally transitions out when the next apply produces a newer non-failed job (the failed job is still in `status.jobs`, but `selectFailedJob` returns it only if it has the newest `createdAt`; if a newer succeeded job exists, `selectFailedJob` returns undefined → bar transitions).

The one piece of mutable state we own is `autoRetriedJobIdRef`. Its lifetime is bound to the `DashboardProvider` mount (per-Session lifetime). That is the correct scope for "one auto-retry per session per failed job" — if the operator reloads the SPA, they get a fresh ref and the next failure auto-retries, which matches the spec's intent (REQ-3 S2.1 says the failed label persists across navigation, but does **not** require the auto-retry to be skipped after a full page reload).

## 6. Type changes

### `apps/ui/src/dashboard/types.ts`

```ts
export type JobRecord = {
  id: string;
  revisionID: string;
  status: string;
  target: string;
  createdAt: string;
  errorMessage?: string | null; // NEW: server already returns this (omitempty)
};

export type StatusPayload = {
  settings: { /* unchanged */ };
  zones: Zone[];
  streams: StreamRoute[];
  jobs: JobRecord[]; // was inline; now named, extended
  certificates: DashboardCertificate[];
  desiredRevision?: { revisionNumber: number; checksum: string };
  appliedRevision?: { revisionNumber: number; checksum: string };
};
```

The `errorMessage` is typed as `string | null | undefined` (all three collapse to "no message"). The call site (`deriveBarState` / `selectFailedJob`) treats them uniformly: empty string, null, and undefined all fall through to the REQ-6 fallback `"Apply failed"`.

### `apps/ui/src/dashboard/dashboard-context.tsx` — `DashboardContextValue`

Three new fields (additive — no existing field changes type):

```ts
type DashboardContextValue = {
  /* ...existing fields unchanged... */
  failedJob?: JobRecord;
  failureReason: string;       // derived, never empty (REQ-6 fallback applied)
  autoRetrying: boolean;       // true between auto-retry dispatch and first observation of newer job
};
```

The fields are read by `DashboardShell` today. No other `useDashboard()` consumer touches them — verified by grep (`apps/ui/src/App.tsx`, `apps/ui/src/dashboard/IngressView.tsx` only destructure additive subsets).

### `apps/ui/src/dashboard/patch-bar-state.ts` — new module

```ts
export type JobLike = { id: string; status: string; createdAt: string; errorMessage?: string | null };
export type BarState = { kind: "checking" } | { kind: "applying" } | { kind: "failed"; reason: string } | { kind: "pending" } | { kind: "live" };
export function selectFailedJob(jobs: readonly JobLike[] | undefined): JobLike | undefined;
export function deriveBarState(input: { loaded: boolean; pendingJob: boolean; inSync: boolean; failureReason?: string | undefined }): BarState;
```

No React, no DOM, no `Date.now()` — pure functions only.

## 7. File-by-file change list

### 7.1 `apps/ui/src/dashboard/types.ts` (modify)

**Existing reference:** lines 34-39, inline `jobs` shape in `StatusPayload`.
**New reference:** extract `JobRecord` as a named type at the top of the file (after `Zone`), with `errorMessage?: string | null` added.
**Change:** ~5 lines added, 1 line modified. Type-only; no runtime effect.

### 7.2 `apps/ui/src/dashboard/patch-bar-state.ts` (NEW)

**~40 lines**, exports `JobLike`, `BarState`, `selectFailedJob`, `deriveBarState`.
Pure TypeScript, no imports. Lives at `apps/ui/src/dashboard/patch-bar-state.ts`.

### 7.3 `apps/ui/src/dashboard/patch-bar-state.test.ts` (NEW)

**~150 lines**, vitest tests for `selectFailedJob` and `deriveBarState`. See §8.

### 7.4 `apps/ui/src/dashboard/dashboard-context.tsx` (modify)

**Existing reference:** `DashboardProvider` body (lines 58-381), `DashboardContextValue` (lines 17-54), `useDashboard` (lines 383-389).
**New reference:**
- Import `JobRecord` from `./types` and `selectFailedJob` from `./patch-bar-state`.
- Add `autoRetriedJobIdRef = useRef<string | null>(null)` near the top of the provider body.
- Add `useMemo` for `autoRetrying` (per §4) near the existing `inSync` memo.
- Add `useEffect` for auto-retry dispatch (per §4) inside the provider body, after the polling effects.
- Add private `async function autoRetryApply()` that calls `mutate("/api/apply", {}, "Apply queued (auto-retry)")` without resetting the guard.
- Modify `apply()` (line 288) to reset the guard and `autoRetrying` before calling `mutate`.
- Extend `DashboardContextValue` (lines 17-54) with three new fields.
- Extend the `value` object (lines 331-374) with the new derived values.

**Change:** ~40 lines added across the file. No deletions; no renames; no behavior change for any existing call site.

### 7.5 `apps/ui/src/dashboard/DashboardShell.tsx` (modify)

**Existing reference:** `DashboardShell` body (lines 8-171), `PatchBar` component (lines 173-253).
**New reference:**
- Destructure `failedJob`, `failureReason`, `autoRetrying` from `useDashboard()` (line 11).
- Compute `failedJob = status?.jobs` filtered to `status === "failed"`, then pick the newest by `createdAt desc` with `id desc` tie-break. Delegate to `selectFailedJob(status?.jobs)` from the new module.
- Pass new props to `<PatchBar>`: `failureReason={failedJob ? (failedJob.errorMessage || "Apply failed") : undefined}` and `autoRetrying={autoRetrying}`.
- Inside `PatchBar`, replace the 4-branch `label` ternary with a call to `deriveBarState({ loaded, pendingJob, inSync, failureReason })` and switch on `state.kind`.
- Render the label: `Checking patch state` / `Apply in progress` / `Failed: ${state.reason}` / `Patch pending` / `Patch live`.
- Compute `failed = state.kind === "failed"` and add `data-failed={failed ? "true" : "false"}` to `.pc-sync-bar` (existing CSS unchanged).
- Update button `disabled` to `!loaded || autoRetrying`. Existing button text (`Apply now` / `Re-apply`) stays the same.
- Keep `waiting = !loaded || pendingJob || !inSync` semantics unchanged for the existing `data-drift` attribute and color tokens, so REQ-1.2 ("the Failed label is never rendered, even if `inSync === false`") is the only thing that changes the visible bar.

**Change:** ~20 lines added, ~5 lines modified. No CSS file changes.

## 8. Test strategy (Q6)

### Where tests live

`apps/ui/src/dashboard/patch-bar-state.test.ts`. This file is picked up by the **existing root `vitest.config.ts`** via the `apps/**/*.test.ts` glob. No vitest configuration change, no new test runner, no new dependency in `apps/ui/package.json`, no DOM/hygdom-jsdom. The tests target only the pure `patch-bar-state` module.

### Why no `.test.tsx`

The repo currently has **zero** `.test.tsx` files and **no React testing infrastructure** in `apps/ui` (verified: `apps/ui/package.json` has no `vitest`, `@testing-library/react`, or `happy-dom`; root `vitest.config.ts` includes only `apps/**/*.test.ts`, not `.tsx`). Introducing `vitest` + `@testing-library/react` + `happy-dom` to `apps/ui` for this single change would expand scope into "add UI testing infrastructure to ProxyCore" — a separate change. By extracting `selectFailedJob` and `deriveBarState` as pure functions, every REQ they participate in is testable today, and a future change can layer DOM-based tests on top without touching the design.

### Test runner

`bun run test` at the repo root (which delegates to the root `vitest.config.ts`); see `package.json` line 14.

### Scenarios mapped to REQs

| Test | REQ covered | What it asserts |
| :--- | :--- | :--- |
| `selectFailedJob` returns `undefined` when no failed job | REQ-1.2 | `(jobs: [])` → `undefined` |
| `selectFailedJob` returns the single failed job | REQ-1.1 | single failed job in array |
| `selectFailedJob` picks newest by `createdAt` desc when multiple | REQ-5.1, REQ-5.2 | `[{createdAt: T1}, {createdAt: T2}]` → T2 |
| `selectFailedJob` tie-breaks by `id` desc on identical `createdAt` | REQ-5.3, Q4 | `[{id: "a"}, {id: "b"}]` → "b"; rerun returns "b" |
| `selectFailedJob` returns `undefined` for `jobs === undefined` | REQ-1.2 edge | `undefined` input → `undefined` |
| `selectFailedJob` ignores jobs with non-`failed` status | REQ-1.2 | mixed array with one `failed` → that one |
| `deriveBarState` returns `checking` when `loaded=false` | REQ-1 baseline | `loaded: false, any` → `{kind: "checking"}` |
| `deriveBarState` returns `applying` when `pendingJob=true` | REQ-1 baseline | `pendingJob: true` → `{kind: "applying"}` even with `failureReason` |
| `deriveBarState` returns `failed` when `failureReason` present, takes precedence over `inSync=true` | REQ-1.3 | `failureReason: "DNS timeout", inSync: true` → `{kind: "failed", reason: "DNS timeout"}` |
| `deriveBarState` returns `failed` when `failureReason` present, takes precedence over `inSync=false` | REQ-1.3 | `failureReason: "x", inSync: false` → `failed` |
| `deriveBarState` returns `live` when inSync and no failure | REQ-1.2 | `inSync: true, failureReason: undefined` → `live` |
| `deriveBarState` returns `pending` when !inSync and no failure | REQ-1.2 | `inSync: false, failureReason: undefined` → `pending` |
| Fallback string in PatchBar renders `Failed: Apply failed` when `errorMessage` is empty/null/undefined | REQ-6 | three inputs (`""`, `null`, `undefined`) → `"Apply failed"` |

### Tests NOT in scope (and why)

- **REQ-2 auto-retry logic** lives inside `DashboardProvider` (React effect + ref). Asserting it requires React testing infra. Deferred to a future "add UI test infra" change. The logic itself is small and was reviewed in §4.
- **REQ-3 persistence** is a property of the polling loop, which is unchanged. The polling loop has no tests today either; introducing one is out of scope.
- **REQ-4 disabled button** is a function of `autoRetrying` being passed to `PatchBar`. Without DOM tests, this is verified by code review of `DashboardShell.tsx`.
- **REQ-5 reorder flicker** is covered by the `selectFailedJob` tie-break test plus the invariant that the function is pure.
- **REQ-7 no optimistic update** is guaranteed by the fact that the bar's label is a pure function of `status.jobs`, and `apply()` does not write to `status.jobs` directly. Code review only.

## 9. Answers to the six open questions

### Q1. Hook return shape — new field for auto-retry state, or separate ref?

**Answer:** New field on `useDashboard()` (which is `DashboardContextValue`), not a separate ref/state outside the hook.

**Rationale:** The proposal's `useApplyStatus()` does not exist in this codebase — the polling loop lives inside `DashboardProvider`. The cleanest pattern is to add three additive fields to `DashboardContextValue`: `failedJob?: JobRecord`, `failureReason: string`, `autoRetrying: boolean`. This keeps `DashboardShell` thin (it just passes the values down to `PatchBar`) and follows the existing convention where every consumer of derived status reads from the context.

The internal `autoRetriedJobIdRef` is **not** exposed via the context — it's a private implementation detail of the auto-retry effect. Exposing it would let consumers (and tests) interfere with the guard.

### Q2. Where does `retriedJobRef` live?

**Answer:** Inside `DashboardProvider` as `useRef<string | null>(null)`. Not module-level, not in `DashboardShell`, not on the context.

**Rationale:**
- `useRef` (not `useState`): the guard is a side-effect bookmark, not render-driven state. Mutating it does not need to re-render.
- Per-mount scope (not module): avoids leaks across HMR, across tests, and across multiple `DashboardProvider` instances.
- Inside `DashboardProvider` (not `DashboardShell`): the polling loop that drives the auto-retry decision lives here; the guard must live next to it.
- Not exposed via context: keeps the auto-retry logic encapsulated; tests of `useDashboard()` consumers cannot accidentally reset or read the guard.

### Q3. PatchBar state-machine input contract

**Answer:** A discriminated union `BarState` derived by a pure function `deriveBarState({ loaded, inSync, pendingJob, failureReason })`. The function lives in the new `patch-bar-state.ts` module.

**Rationale:** Discriminated unions force `PatchBar` to handle every case explicitly, which is exactly what REQ-1.3 (failed takes precedence) requires. The pure function makes the precedence rule visible in one place instead of buried inside a 5-branch ternary. PatchBar's existing 5 prop booleans (`loaded`, `desired`, `applied`, `inSync`, `pendingJob`) are preserved for backward compatibility — we add two new props (`failureReason`, `autoRetrying`) rather than collapsing them. The minimum-disruption contract change.

### Q4. Tie-break for multiple failed jobs with identical `createdAt`

**Answer:** `createdAt` desc first; on identical `createdAt`, `id` desc.

**Rationale:** `createdAt` desc is what REQ-5 mandates. The tie-break is needed because REQ-5 S2.3 requires determinism across renders. `id` desc is a stable, lexicographic comparison that does not depend on array order or `Array.prototype.sort` stability across engines. We avoid sorting the array entirely — `selectFailedJob` walks it once and keeps a running best — so the result is a function of the input only.

### Q5. How does the design detect "an apply is already in flight"?

**Answer:** A new memo `autoRetrying` inside `DashboardProvider` returns `true` when (a) `autoRetriedJobIdRef.current` equals the selected failed job's id, AND (b) no job with a newer `createdAt` has appeared in `status.jobs`. It returns `false` otherwise.

**Rationale:** This avoids the need for a separate "retry dispatched but not yet observed" state variable. The auto-retry window is fully derivable from `status.jobs` and the ref:
- Before dispatch: ref is `null` → memo is `false`.
- After dispatch, before next poll: ref matches the failed job id, no newer job yet → memo is `true`. Button is disabled.
- After the next poll lands with a newer job (any status): `newerExists` is `true` → memo is `false`. Button is re-enabled.
- After a user-initiated `apply()`: `apply()` resets both the ref and `autoRetrying`; the next observation with a newer job keeps `autoRetrying` at `false`.

The memo is consumed by `DashboardShell`, passed to `PatchBar`, and drives the button's `disabled` attribute. No separate "in-flight" boolean needs to be set or cleared.

### Q6. Where do tests live?

**Answer:** `apps/ui/src/dashboard/patch-bar-state.test.ts`, using the existing repo-level Vitest (no new test runner, no DOM, no React testing library).

**Rationale:** `apps/ui` currently has zero test infrastructure installed (no `vitest`, no `@testing-library/react`, no `happy-dom`), and the root `vitest.config.ts` includes only `apps/**/*.test.ts` (not `.tsx`). Adding Vitest + Testing Library + happy-dom to `apps/ui/package.json` for this single change would balloon scope into "add UI testing infrastructure" — a separate, larger change. By extracting the deterministic, REQ-bearing logic (`selectFailedJob` and `deriveBarState`) into a pure module with no React or DOM imports, every REQ that maps to those functions is testable **today** via the existing test runner. Pure functions are also easier to reason about for the REQ-7 invariant (no optimistic update), because the property is structural: `deriveBarState` cannot read anything outside its input, so a network error in `apply()` cannot influence it.

The trade-off: REQ-2, REQ-3, REQ-4, and REQ-7 are not directly unit-tested. They are small, surface-area effects that are best reviewed by code and exercised manually (the dev server can drive a failed apply by killing the worker container briefly). A future change can add `@testing-library/react` + `happy-dom` and cover them.

## 10. Risks (design-level, beyond the proposal's 5)

### R-D1. `autoRetrying` memo diverges from `autoRetriedJobIdRef` writes

**Severity:** low.
**Description:** The memo reads `autoRetriedJobIdRef.current` which is a non-reactive ref. If the ref is mutated in the same render cycle as the memo is computed, React may not re-run the memo until the next `status?.jobs` change. In practice this is harmless: the ref is mutated in the auto-retry effect (which runs after render), and the memo recomputes on the next `status?.jobs` tick — which is what we want. If this ever becomes a real bug (e.g., we need the button to disable synchronously after dispatch), we replace the ref with `useState`.

### R-D2. `selectFailedJob` tie-break depends on locale-stable string comparison

**Severity:** low.
**Description:** `id > best.id` is a JS string comparison. Job ids in this codebase are ULIDs/UUIDs (verified in `apps/api/internal/configuration/rows.go`), so lexical order is stable across V8 versions and locales. If a future migration changes job ids to something locale-sensitive (e.g., user-provided labels), the tie-break must be revisited.

### R-D3. Failed label says `Failed: Apply failed` (double "failed")

**Severity:** low (cosmetic / a11y).
**Description:** When `errorMessage` is empty, the label renders as `Failed: Apply failed`. The repetition is grammatically clumsy and a screen reader will announce "Failed apply failed". Acceptable for an empty-reason fallback that should rarely fire, but worth knowing. A future polish could render just `Apply failed` (no `Failed:` prefix) when the reason is the fallback — kept out of scope to avoid string changes that might confuse operators who already saw `Failed:` in tests.

### R-D4. Operator's `Apply now` is disabled mid-auto-retry; they may not understand why

**Severity:** low (UX).
**Description:** REQ-4 disables the manual button while `autoRetrying`. There is no explanatory message. A power user who clicks immediately after a failure will see the button disabled and may not know an auto-retry is in flight. Mitigation: the `data-failed="true"` attribute on the bar is a theming hook for a future change to add a "Retrying…" affordance. Not in scope.

### R-D5. The retry guard persists across SPA navigations but not full reloads

**Severity:** low (REQ-3 consistency).
**Description:** `autoRetriedJobIdRef` is per-mount. After a full page reload (F5), the ref is fresh and the next failure will auto-retry again. REQ-3 says the failed label persists across reloads (which it does, because the label is data-derived). It does not require the auto-retry to be skipped after a reload. If a future REQ adds "auto-retry is one per failed job ever, not one per session," this design must change.

### R-D6. `data-failed="true"` attribute is added but no CSS consumes it

**Severity:** low (dead-code risk).
**Description:** The proposal says the attribute is "enough to color the dot" via future theming. In this change no CSS rule uses `data-failed="true"`; the failed bar is colored with the existing `text-signal` token via the existing `waiting` logic. The attribute is dead code today. Acceptable because (a) it's cheap, (b) the proposal explicitly defers visual design to a later change, and (c) it documents intent.

### R-D7. i18n

**Severity:** low.
**Description:** Labels `Checking patch state`, `Apply in progress`, `Patch live`, `Patch pending`, `Apply now`, `Re-apply` are hard-coded English in `DashboardShell.tsx`. The new `Failed: <reason>` label inherits the same convention. No i18n framework is in use in this codebase today, so there is no regression risk. If i18n lands in a future change, every bar label will need translation; the new `Failed:` label is just one more string.

## 11. Rollback plan

Three files are modified (`apps/ui/src/dashboard/types.ts`, `apps/ui/src/dashboard/dashboard-context.tsx`, `apps/ui/src/dashboard/DashboardShell.tsx`); two files are added (`patch-bar-state.ts`, `patch-bar-state.test.ts`). Rollback is `git revert` of the change's commits:

1. Revert `apps/ui/src/dashboard/types.ts` — remove the extracted `JobRecord` type, restore the inline `jobs` shape.
2. Revert `apps/ui/src/dashboard/dashboard-context.tsx` — remove the new ref, the new effect, the new `autoRetrying` memo, the `autoRetryApply` helper, the guard-reset in `apply()`, and the three new context fields.
3. Revert `apps/ui/src/dashboard/DashboardShell.tsx` — restore the 4-branch `label` ternary, remove the new props and `data-failed` attribute, restore the original `disabled={!loaded}`.
4. Delete `apps/ui/src/dashboard/patch-bar-state.ts` and `apps/ui/src/dashboard/patch-bar-state.test.ts`.

After rollback:
- PatchBar reverts to the 4-state machine exactly as it was before the change.
- `errorMessage` becomes an unused optional field on the inline `jobs` shape (TypeScript is permissive about unused optional fields, so `tsc --noEmit` stays green).
- No server, worker, DB, or other client file is affected.
- The auto-retry-once behavior disappears, and a failed apply shows up as `Patch pending` or `Patch live` again — the original bug returns. That is acceptable as a temporary regression during a hot-fix cycle.

No new dependencies, no DB migrations, no API contract changes — rollback is bounded to the UI workspace.