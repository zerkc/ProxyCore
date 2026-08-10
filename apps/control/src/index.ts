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
export * from "./docker-control";

async function main() {
  const socketPath = process.env.WORKER_SOCKET_PATH ?? "/run/proxycore/control.sock";
  const { FixedServiceControl } = await import("./service");
  const { startControlServer } = await import("./transport");
  const fixedOperation = async (request: import("./protocol").ControlRequest) => ({
    service: request.service,
    operation: request.operation,
    candidatePath: request.candidatePath,
    status: "accepted-by-fixed-boundary",
  });
  const fallbackControl = new FixedServiceControl({
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
  const candidateRoot =
    process.env.CANDIDATE_ROOT ?? "/var/lib/proxycore/candidates";
  const corednsConfigRoot =
    process.env.COREDNS_CONFIG_ROOT ?? "/var/lib/proxycore/coredns-config";
  const control =
    process.env.CONTROL_BACKEND === "docker"
      ? (await import("./docker-control")).createDockerServiceControl({
          candidateRoot,
          corednsZonesRoot: process.env.COREDNS_ZONES_ROOT,
          corednsConfigRoot,
          containers: {
            nginx: process.env.NGINX_CONTAINER_NAME,
            coredns: process.env.COREDNS_CONTAINER_NAME,
          },
        })
      : fallbackControl;

  if (process.env.CONTROL_BACKEND === "docker") {
    const { seedLiveCorefileFromCandidates } = await import("./docker-control");
    const seeded = await seedLiveCorefileFromCandidates(
      candidateRoot,
      corednsConfigRoot,
    );
    if (seeded) {
      process.stdout.write(
        `proxycore-control restored persisted CoreDNS Corefile from ${seeded}\n`,
      );
      try {
        const Docker = (await import("dockerode")).default;
        const docker = new Docker({
          socketPath: process.env.DOCKER_SOCKET_PATH ?? "/var/run/docker.sock",
        });
        const name = process.env.COREDNS_CONTAINER_NAME ?? "proxycore-coredns";
        await docker.getContainer(name).restart({ t: 5 });
        process.stdout.write(`proxycore-control restarted ${name} to load Corefile\n`);
      } catch (error) {
        process.stderr.write(
          `proxycore-control could not restart CoreDNS after Corefile seed: ${
            error instanceof Error ? error.message : String(error)
          }\n`,
        );
      }
    }
  }

  await startControlServer(socketPath, control);
  process.stdout.write(`proxycore-control listening on ${socketPath}\n`);
}

if (process.argv[1]?.endsWith("/apps/control/src/index.ts")) {
  void main().catch((error) => {
    console.error(error);
    process.exitCode = 1;
  });
}
