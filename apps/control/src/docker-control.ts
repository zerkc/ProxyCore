import Docker from "dockerode";
import { access, stat } from "node:fs/promises";
import { FixedServiceControl, type FixedHandlers } from "./service";
import type { ControlRequest, ControlService } from "./protocol";

export type DockerControlOptions = {
  socketPath?: string;
  candidateRoot?: string;
  containers?: Partial<Record<ControlService, string>>;
};

export function createDockerServiceControl(
  options: DockerControlOptions = {},
): FixedServiceControl {
  const docker = new Docker({ socketPath: options.socketPath ?? "/var/run/docker.sock" });
  const candidateRoot = options.candidateRoot ?? "/var/lib/proxycore/candidates";
  const containers = {
    coredns: options.containers?.coredns ?? "proxycore-coredns",
    nginx: options.containers?.nginx ?? "proxycore-nginx",
  };
  const execute = (request: ControlRequest) =>
    executeDockerOperation(docker, request, candidateRoot, containers[request.service]);
  const handlers: FixedHandlers = {
    coredns: fixedHandlers(execute),
    nginx: fixedHandlers(execute),
  };
  return new FixedServiceControl(handlers);
}

function fixedHandlers(
  execute: (request: ControlRequest) => Promise<unknown>,
): NonNullable<FixedHandlers["coredns"]> {
  return {
    stage: execute,
    validate: execute,
    promote: execute,
    reload: execute,
    health: execute,
    rollback: execute,
  };
}

async function executeDockerOperation(
  docker: Docker,
  request: ControlRequest,
  candidateRoot: string,
  containerName: string,
): Promise<unknown> {
  assertCandidatePath(request.candidatePath, candidateRoot);
  const container = docker.getContainer(containerName);
  switch (request.operation) {
    case "stage":
      await stat(request.candidatePath);
      return { status: "staged", candidatePath: request.candidatePath };
    case "validate":
      if (request.service === "nginx") {
        await exec(container, [
          "nginx",
          "-t",
          "-c",
          `${request.candidatePath}/nginx.conf`,
        ]);
      } else {
        await access(`${request.candidatePath}/Corefile`);
      }
      return { status: "validated", checksum: request.checksum };
    case "promote":
      if (request.service === "nginx") {
        await exec(container, ["cp", "/etc/nginx/nginx.conf", "/etc/nginx/.proxycore.previous.conf"]);
        await exec(container, ["cp", `${request.candidatePath}/nginx.conf`, "/etc/nginx/nginx.conf"]);
      } else {
        await exec(container, ["cp", "/etc/coredns/Corefile", "/etc/coredns/.proxycore.previous.Corefile"]);
        await exec(container, ["cp", `${request.candidatePath}/Corefile`, "/etc/coredns/Corefile"]);
        await exec(container, ["cp", "-R", `${request.candidatePath}/zones/.`, "/etc/coredns/zones"]);
      }
      return { status: "promoted", revisionId: request.revisionId };
    case "reload":
      if (request.service === "nginx") {
        await exec(container, ["nginx", "-s", "reload"]);
      } else {
        await exec(container, ["kill", "-HUP", "1"]);
      }
      return { status: "reloaded" };
    case "health":
      if (!(await container.inspect()).State?.Running) {
        throw new Error(`${request.service} container is not running`);
      }
      return { status: "healthy" };
    case "rollback":
      if (request.service === "nginx") {
        await exec(container, ["cp", "/etc/nginx/.proxycore.previous.conf", "/etc/nginx/nginx.conf"]);
      } else {
        await exec(container, ["cp", "/etc/coredns/.proxycore.previous.Corefile", "/etc/coredns/Corefile"]);
      }
      return { status: "rolled-back" };
  }
}

export function assertCandidatePath(candidatePath: string, candidateRoot: string): void {
  if (
    !candidatePath.startsWith(`${candidateRoot}/`) ||
    candidatePath.includes("..") ||
    candidatePath.includes("\0")
  ) {
    throw new Error("Candidate path is outside the fixed worker root");
  }
}

async function exec(container: Docker.Container, command: string[]): Promise<void> {
  const process = await container.exec({
    Cmd: command,
    AttachStdout: true,
    AttachStderr: true,
  });
  const stream = await process.start({ hijack: false, stdin: false });
  await new Promise<void>((resolve, reject) => {
    stream.on("end", resolve);
    stream.on("error", reject);
  });
  const result = await process.inspect();
  if (result.ExitCode !== 0) {
    throw new Error(`Fixed service operation failed with exit ${result.ExitCode ?? "unknown"}`);
  }
}
