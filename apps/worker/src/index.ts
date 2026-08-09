import { mkdirSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";
import { loadConfig } from "@proxycore/config";
import {
  createDatabase,
  PgJobStore,
  PgRevisionStore,
  PgSecretStore,
  runJobNotificationListener,
} from "@proxycore/db";
import { ApplyOrchestrator } from "./apply";
import { UnixSocketControlClient } from "./control-client";
import { runWorkerLoop, WorkerWakeup } from "./poller";

export function workerIdentity(): string {
  return "proxycore-worker";
}

export * from "./apply";
export * from "./operations";
export * from "./control-client";
export * from "./poller";
export * from "./render";

async function main(): Promise<void> {
  const config = loadConfig();
  const heartbeatPath =
    process.env.WORKER_HEARTBEAT_PATH ?? "/run/proxycore/worker.heartbeat";
  const candidateRoot =
    process.env.WORKER_CANDIDATE_ROOT ?? "/var/lib/proxycore/candidates";
  const reconciliationIntervalMs = positiveInteger(
    process.env.WORKER_RECONCILIATION_INTERVAL_MS ??
      process.env.WORKER_POLL_INTERVAL_MS,
    300_000,
  );
  const leaseMs = positiveInteger(process.env.WORKER_JOB_LEASE_MS, 120_000);
  mkdirSync(dirname(heartbeatPath), { recursive: true });
  const heartbeat = () => {
    writeFileSync(heartbeatPath, new Date().toISOString());
  };
  heartbeat();
  const heartbeatTimer = setInterval(heartbeat, 30_000);
  const database = createDatabase(config.databaseUrl);
  const jobs = new PgJobStore(database.db);
  const revisions = new PgRevisionStore(database.db);
  const secretStore = config.masterKeyBase64
    ? new PgSecretStore(database.db, config.masterKeyBase64)
    : undefined;
  const orchestrator = new ApplyOrchestrator({
    jobs,
    revisions,
    control: new UnixSocketControlClient(config.workerSocketPath),
    candidateRoot,
  });
  const controller = new AbortController();
  const wakeup = new WorkerWakeup();
  const shutdown = () => controller.abort();
  process.once("SIGINT", shutdown);
  process.once("SIGTERM", shutdown);
  const listenerPromise = runJobNotificationListener({
    pool: database.pool,
    signal: controller.signal,
    onJob: () => wakeup.notify(),
    onError: (error) => {
      process.stderr.write(
        `${new Date().toISOString()} worker notification listener error: ${
          error instanceof Error ? error.message : String(error)
        }\n`,
      );
    },
  });
  process.stdout.write(
    `${workerIdentity()} listening for jobs; reconciling every ${reconciliationIntervalMs}ms\n`,
  );

  try {
    await runWorkerLoop({
      jobs,
      revisions,
      orchestrator,
      leaseMs,
      reconciliationIntervalMs,
      wakeup,
      signal: controller.signal,
      renderOptions: {
        candidateRoot,
        secretStore,
        acmeUpstream: process.env.NGINX_ACME_UPSTREAM,
        capabilities: {
          http3Module: process.env.NGINX_HTTP3_MODULE === "1",
          tcp443Published:
            process.env.NGINX_NETWORK_MODE === "host" ||
            process.env.NGINX_TCP_443_PUBLISHED === "1",
          udp443Published:
            process.env.NGINX_NETWORK_MODE === "host" ||
            process.env.NGINX_UDP_443_PUBLISHED === "1",
        },
      },
      heartbeat,
      onError: (error) => {
        process.stderr.write(
          `${new Date().toISOString()} worker poll error: ${
            error instanceof Error ? error.message : String(error)
          }\n`,
        );
      },
    });
  } finally {
    clearInterval(heartbeatTimer);
    controller.abort();
    wakeup.notify();
    process.removeListener("SIGINT", shutdown);
    process.removeListener("SIGTERM", shutdown);
    await listenerPromise;
    await database.pool.end();
  }
}

function positiveInteger(value: string | undefined, fallback: number): number {
  const parsed = Number(value);
  return Number.isInteger(parsed) && parsed > 0 ? parsed : fallback;
}

if (process.argv[1]?.endsWith("/apps/worker/src/index.ts")) {
  void main().catch((error) => {
    console.error(error);
    process.exitCode = 1;
  });
}
