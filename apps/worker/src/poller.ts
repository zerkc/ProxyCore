import type { JobRecord, JobStore, RevisionStore } from "@proxycore/db";
import { ApplyOrchestrator } from "./apply";
import { renderJobCandidates, type WorkerRenderOptions } from "./render";

export type PollOnceOptions = {
  jobs: JobStore;
  revisions: RevisionStore;
  orchestrator: ApplyOrchestrator;
  renderOptions?: WorkerRenderOptions;
  leaseMs?: number;
  now?: () => Date;
};

export async function pollOnce(
  options: PollOnceOptions,
): Promise<JobRecord | undefined> {
  await options.jobs.recoverStale(
    options.leaseMs ?? 120_000,
    options.now?.() ?? new Date(),
  );
  const job = await options.jobs.claimNext();
  if (!job) return undefined;

  const revision = await options.revisions.get(job.revisionId);
  if (!revision) {
    return options.jobs.update(job.id, {
      status: "failed",
      errorMessage: `Revision not found: ${job.revisionId}`,
      finishedAt: options.now?.() ?? new Date(),
    });
  }

  return options.orchestrator.apply(
    job.id,
    revision.snapshot,
    async (snapshot, claimedJob) =>
      renderJobCandidates(snapshot, claimedJob, options.renderOptions),
  );
}

export type WorkerLoopOptions = PollOnceOptions & {
  reconciliationIntervalMs?: number;
  /** @deprecated Use reconciliationIntervalMs. */
  pollIntervalMs?: number;
  wakeup?: WorkerWakeup;
  signal?: AbortSignal;
  heartbeat?: () => void;
  onError?: (error: unknown) => void;
};

export class WorkerWakeup {
  private pending = false;
  private resolveWait?: () => void;

  notify(): void {
    if (this.resolveWait) {
      const resolve = this.resolveWait;
      this.resolveWait = undefined;
      resolve();
      return;
    }
    this.pending = true;
  }

  wait(milliseconds: number, signal?: AbortSignal): Promise<void> {
    if (this.pending) {
      this.pending = false;
      return Promise.resolve();
    }
    return new Promise((resolve) => {
      let settled = false;
      const finish = () => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        signal?.removeEventListener("abort", onAbort);
        if (this.resolveWait === finish) {
          this.resolveWait = undefined;
        }
        resolve();
      };
      const timer = setTimeout(finish, milliseconds);
      const onAbort = finish;
      this.resolveWait = finish;
      signal?.addEventListener("abort", onAbort, { once: true });
    });
  }
}

export async function runWorkerLoop(options: WorkerLoopOptions): Promise<void> {
  const intervalMs =
    options.reconciliationIntervalMs ?? options.pollIntervalMs ?? 300_000;
  const signal = options.signal;

  while (!signal?.aborted) {
    options.heartbeat?.();
    try {
      let processed: JobRecord | undefined;
      do {
        processed = await pollOnce(options);
      } while (processed && !signal?.aborted);
    } catch (error) {
      options.onError?.(error);
    }
    if (signal?.aborted) break;
    if (options.wakeup) {
      await options.wakeup.wait(intervalMs, signal);
    } else {
      await wait(intervalMs, signal);
    }
  }
}

function wait(milliseconds: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(resolve, milliseconds);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        resolve();
      },
      { once: true },
    );
  });
}
