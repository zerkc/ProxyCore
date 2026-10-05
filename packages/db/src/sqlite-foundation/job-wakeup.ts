/** Cheap change cursor over apply_jobs for the injected `readCursor`. A JSON text
 * fingerprint compared by identity: count and max(rowid) catch inserts, the queued
 * count and the max claimed/started/finished timestamps catch claim, progress, finish
 * and requeue transitions. Best-effort hint only (a same-count swap with identical
 * maxima is missed); the worker's periodic reconciliation remains the safety net. */
export const SQLITE_JOB_CURSOR_SQL = `SELECT json_array(count(*), coalesce(max(rowid),0),
  coalesce(sum(status = 'queued'),0), coalesce(max(claimed_at),''),
  coalesce(max(started_at),''), coalesce(max(finished_at),'')) AS cursor FROM apply_jobs`;

export const DEFAULT_JOB_WAKEUP_POLL_INTERVAL_MS = 500;
export const DEFAULT_JOB_WAKEUP_ERROR_DELAY_MS = 1_000;

export type JobWakeupOptions = {
  signal?: AbortSignal;
  /** Always called with `undefined`: the cursor is opaque and carries no job id. */
  onJob: (jobId: string | undefined) => void;
  /** Opaque cursor, compared with Object.is (return a string/number/bigint). */
  readCursor: () => Promise<unknown> | unknown;
  /** Positive finite milliseconds; default 500. */
  pollIntervalMs?: number;
  /** Receives readCursor/onJob failures (never after abort). Errors it throws are ignored. */
  onError?: (error: unknown) => void;
  /** Wait after a readCursor failure, at least pollIntervalMs; default 1000 (notifications.ts). */
  errorDelayMs?: number;
};

export type JobWakeupDependencies = {
  /** Injected clock; must resolve when the delay elapses or the signal aborts. */
  sleep?: (milliseconds: number, signal?: AbortSignal) => Promise<void>;
};

export type JobWakeup = { run(options: JobWakeupOptions): Promise<void> };

/** Unwired cursor-polling replacement for pg_notify/LISTEN. Mirrors
 * runJobNotificationListener: onJob(undefined) after the first successful read and
 * after every recovery from a failed read, then once per cursor change, never when
 * unchanged. A readCursor failure is reported, waited out (no hot spin) and retried;
 * an onJob failure is reported and the loop continues with the cursor already
 * advanced. Polls never overlap; abort resolves cleanly, including mid-read. */
export function createSQLiteJobWakeup(dependencies: JobWakeupDependencies = {}): JobWakeup {
  const sleep = dependencies.sleep ?? abortableSleep;
  return {
    async run(options) {
      const pollMs = interval("pollIntervalMs", options.pollIntervalMs ?? DEFAULT_JOB_WAKEUP_POLL_INTERVAL_MS);
      const errorMs = Math.max(pollMs, interval("errorDelayMs", options.errorDelayMs ?? DEFAULT_JOB_WAKEUP_ERROR_DELAY_MS));
      const { signal } = options;
      const report = (error: unknown) => {
        if (signal?.aborted) return;
        try { options.onError?.(error); } catch { /* reporting must not kill the loop */ }
      };
      let cursor: unknown;
      let primed = false;
      while (!signal?.aborted) {
        let next: unknown;
        try {
          next = await abortable(Promise.resolve().then(options.readCursor), signal);
        } catch (error) {
          report(error);
          primed = false;
          await sleep(errorMs, signal);
          continue;
        }
        if (signal?.aborted) break;
        if (!primed || !Object.is(next, cursor)) {
          primed = true;
          cursor = next;
          try {
            // Not awaited: a slow or never-settling handler must not block polling or abort.
            const pending: unknown = options.onJob(undefined);
            if (isThenable(pending)) pending.then(undefined, report);
          } catch (error) {
            report(error);
          }
        }
        if (!signal?.aborted) await sleep(pollMs, signal);
      }
    },
  };
}

function interval(name: string, value: number): number {
  if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) {
    throw new RangeError(`${name} must be a positive finite number`);
  }
  return value;
}

const aborted = Symbol("aborted");

function isThenable(value: unknown): value is PromiseLike<unknown> {
  return typeof (value as { then?: unknown } | null | undefined)?.then === "function";
}

/** Settles with the work, or resolves early (value ignored; callers re-check the signal) on abort. */
function abortable<T>(work: Promise<T>, signal?: AbortSignal): Promise<T> {
  if (!signal) return work;
  work.catch(() => {}); // an abandoned in-flight read must not become an unhandled rejection
  return new Promise<T>((resolve, reject) => {
    const onAbort = () => resolve(aborted as never);
    if (signal.aborted) return onAbort();
    signal.addEventListener("abort", onAbort, { once: true });
    work.then(
      (value) => { signal.removeEventListener("abort", onAbort); resolve(value); },
      (error) => { signal.removeEventListener("abort", onAbort); reject(error); },
    );
  });
}

function abortableSleep(milliseconds: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal?.aborted) return resolve();
    const finish = () => {
      clearTimeout(timer);
      signal?.removeEventListener("abort", finish);
      resolve();
    };
    const timer = setTimeout(finish, milliseconds);
    signal?.addEventListener("abort", finish, { once: true });
  });
}
