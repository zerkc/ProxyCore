export function workerIdentity(): string {
  return "proxycore-worker";
}

export * from "./apply";
export * from "./operations";

if (process.argv[1]?.endsWith("/apps/worker/src/index.ts")) {
  process.stdout.write(`${workerIdentity()}\n`);
}
