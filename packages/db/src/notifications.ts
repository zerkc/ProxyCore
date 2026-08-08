import type { Pool, PoolClient } from "pg";

export const JOB_NOTIFICATION_CHANNEL = "proxycore_jobs";

export type JobNotificationListenerOptions = {
  pool: Pool;
  signal?: AbortSignal;
  onJob: (jobId: string | undefined) => void;
  onError?: (error: unknown) => void;
  reconnectDelayMs?: number;
};

export async function runJobNotificationListener(
  options: JobNotificationListenerOptions,
): Promise<void> {
  const reconnectDelayMs = options.reconnectDelayMs ?? 1_000;
  while (!options.signal?.aborted) {
    let client: PoolClient | undefined;
    try {
      client = await options.pool.connect();
      await client.query(`LISTEN ${JOB_NOTIFICATION_CHANNEL}`);
      options.onJob(undefined);
      await waitForConnection(client, options);
    } catch (error) {
      if (!options.signal?.aborted) {
        options.onError?.(error);
      }
    } finally {
      client?.release(true);
    }
    if (!options.signal?.aborted) {
      await wait(reconnectDelayMs, options.signal);
    }
  }
}

async function waitForConnection(
  client: PoolClient,
  options: JobNotificationListenerOptions,
): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    let settled = false;
    const onNotification = (message: { channel: string; payload?: string }) => {
      if (message.channel === JOB_NOTIFICATION_CHANNEL) {
        options.onJob(message.payload);
      }
    };
    const onError = (error: Error) => finish(() => reject(error));
    const onEnd = () => finish(() => reject(new Error("PostgreSQL listener disconnected")));
    const onAbort = () => finish(resolve);
    const finish = (complete: () => void) => {
      if (settled) return;
      settled = true;
      client.removeListener("notification", onNotification);
      client.removeListener("error", onError);
      client.removeListener("end", onEnd);
      options.signal?.removeEventListener("abort", onAbort);
      complete();
    };

    client.on("notification", onNotification);
    client.once("error", onError);
    client.once("end", onEnd);
    options.signal?.addEventListener("abort", onAbort, { once: true });
  });
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
