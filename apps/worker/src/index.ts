import { writeFileSync } from "node:fs";

export function workerIdentity(): string {
  return "proxycore-worker";
}

export * from "./apply";
export * from "./operations";

if (process.argv[1]?.endsWith("/apps/worker/src/index.ts")) {
  const heartbeatPath = process.env.WORKER_HEARTBEAT_PATH ?? "/run/proxycore/worker.heartbeat";
  const heartbeat = () => {
    writeFileSync(heartbeatPath, new Date().toISOString());
  };
  heartbeat();
  process.stdout.write(`${workerIdentity()} ready\n`);
  setInterval(() => {
    heartbeat();
    process.stdout.write(`${new Date().toISOString()} worker heartbeat\n`);
  }, 60_000);
}
