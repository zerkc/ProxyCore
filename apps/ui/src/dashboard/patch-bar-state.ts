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
    if (t > bestCreated || (t === bestCreated && best !== undefined && j.id > best.id)) {
      best = j;
      bestCreated = t;
    }
  }
  return best;
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
