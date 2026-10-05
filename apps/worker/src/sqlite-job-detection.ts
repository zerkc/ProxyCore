import type { Database } from "bun:sqlite";

/** Read-only observation, not ownership. No watermark, cache, LIMIT, or writes. */
export function* queuedSQLiteJobIDs(db: Database): Generator<string> {
  const statement = db.prepare<{ id: string }, []>(
    "SELECT id FROM apply_jobs WHERE status = 'queued'",
  );
  try {
    for (const row of statement.iterate()) {
      yield row.id;
    }
  } finally {
    statement.finalize(); // Release partially iterated cursors after callback failure/abort.
  }
}

type SyncCallback<T> = (value: T) => void;
export type SQLiteJobDetectionOptions = {
  db: Database;
  /** Synchronous/nonblocking: notify a wakeup; never await job processing here. */
  onJob: SyncCallback<string>;
  /** Successful complete scans only, including empty scans. */
  onScan?: () => void;
  /** Terminal errors are also rejected by done. Handler failure replaces the error. */
  onError?: SyncCallback<unknown>;
  intervalMs?: number;
  signal?: AbortSignal;
};

function synchronous(result: unknown): void {
  if (result && typeof (result as { then?: unknown }).then === "function") {
    // Consume rejected promises/thenables locally without awaiting processing.
    void Promise.resolve(result).catch(() => {});
    throw new TypeError("Detector callbacks must be synchronous/nonblocking");
  }
}

/**
 * Unwired scheduler independent of an asynchronously suspended processor.
 * Startup scans immediately; repeated IDs are intentional and restart-safe.
 * Query/callback failures terminate explicitly (no silent empty-queue fallback).
 * 1s default leaves headroom for controlled <=5s observation, not a hard SLA:
 * SQLite waits, query duration, callback work and event-loop stalls add latency.
 * Caller owns the database. stop/abort resolves done and removes the timer/listener.
 */
export function startSQLiteJobDetection(options: SQLiteJobDetectionOptions): {
  stop: () => void;
  done: Promise<void>;
} {
  const interval = options.intervalMs ?? 1_000;
  if (!Number.isInteger(interval) || interval <= 0 || interval > 4_000) {
    throw new RangeError("intervalMs must be a positive finite integer <= 4000");
  }
  let active = true;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let resolve!: () => void;
  let reject!: (error: unknown) => void;
  const done = new Promise<void>((yes, no) => { resolve = yes; reject = no; });
  const cleanup = () => {
    active = false;
    clearTimeout(timer);
    options.signal?.removeEventListener("abort", stop);
  };
  const stop = () => {
    if (!active) return;
    cleanup();
    resolve();
  };
  const scan = () => {
    if (!active) return;
    try {
      for (const id of queuedSQLiteJobIDs(options.db)) {
        if (!active) break;
        synchronous(options.onJob(id));
        if (!active) break; // Do not advance the SQLite cursor after callback cancellation.
      }
      if (active && options.onScan) synchronous(options.onScan());
      if (active) timer = setTimeout(scan, interval);
    } catch (error) {
      if (!active) return;
      cleanup();
      try {
        if (options.onError) synchronous(options.onError(error));
      } catch (handlerError) {
        error = handlerError;
      }
      reject(error);
    }
  };
  if (options.signal?.aborted) stop();
  else {
    options.signal?.addEventListener("abort", stop, { once: true });
    scan();
  }
  return { stop, done };
}
