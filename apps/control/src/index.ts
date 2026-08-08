export const CONTROL_OPERATIONS = [
  "stage",
  "validate",
  "promote",
  "reload",
  "health",
  "rollback",
] as const;

export type ControlOperation = (typeof CONTROL_OPERATIONS)[number];

export * from "./protocol";
export * from "./service";
export * from "./transport";

if (process.argv[1]?.endsWith("/apps/control/src/index.ts")) {
  const socketPath = process.env.WORKER_SOCKET_PATH ?? "/run/proxycore/control.sock";
  const { FixedServiceControl } = await import("./service");
  const { startControlServer } = await import("./transport");
  const fixedOperation = async (request: import("./protocol").ControlRequest) => ({
    service: request.service,
    operation: request.operation,
    candidatePath: request.candidatePath,
    status: "accepted-by-fixed-boundary",
  });
  const control = new FixedServiceControl({
    coredns: {
      stage: fixedOperation,
      validate: fixedOperation,
      promote: fixedOperation,
      reload: fixedOperation,
      health: fixedOperation,
      rollback: fixedOperation,
    },
    nginx: {
      stage: fixedOperation,
      validate: fixedOperation,
      promote: fixedOperation,
      reload: fixedOperation,
      health: fixedOperation,
      rollback: fixedOperation,
    },
  });
  await startControlServer(socketPath, control);
  process.stdout.write(`proxycore-control listening on ${socketPath}\n`);
}
