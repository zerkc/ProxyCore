export type JobLike = {
  id: string;
  status: string;
  createdAt: string;
  errorMessage?: string | null | undefined;
};

export type BarState =
  | { kind: "checking" }
  | { kind: "applying" }
  | { kind: "failed"; reason: string }
  | { kind: "pending" }
  | { kind: "live" };

export function selectFailedJob(
  jobs: readonly JobLike[] | undefined,
): JobLike | undefined {
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

// A failed job is only relevant to the current patch-bar state when it is
// the most recent job overall. Any newer job (queued, applying, applied,
// succeeded, …) supersedes the historical failure, otherwise the bar would
// keep nagging about a stale error forever after a successful retry.
export function selectRelevantFailedJob(
  jobs: readonly JobLike[] | undefined,
): JobLike | undefined {
  const failed = selectFailedJob(jobs);
  if (!failed) return undefined;
  if (!jobs) return failed;
  const failedTime = Date.parse(failed.createdAt);
  if (Number.isNaN(failedTime)) return undefined;
  for (const j of jobs) {
    if (j.id === failed.id) continue;
    const t = Date.parse(j.createdAt);
    if (!Number.isNaN(t) && t > failedTime) return undefined;
  }
  return failed;
}

export function deriveBarState(input: {
  loaded: boolean;
  pendingJob: boolean;
  inSync: boolean;
  failureReason?: string | null;
}): BarState {
  if (!input.loaded) return { kind: "checking" };
  if (input.pendingJob) return { kind: "applying" };
  if (input.failureReason !== undefined) {
    return { kind: "failed", reason: input.failureReason || "Apply failed" };
  }
  if (input.inSync) return { kind: "live" };
  return { kind: "pending" };
}
