export function workerIdentity(): string {
  return "proxycore-worker";
}

if (process.argv[1]?.endsWith("/apps/worker/src/index.ts")) {
  process.stdout.write(`${workerIdentity()}\n`);
}
