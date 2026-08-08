import Docker from "dockerode";
import {
  access,
  chmod,
  copyFile,
  mkdir,
  readFile,
  readdir,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
import { join } from "node:path";
import { FixedServiceControl, type FixedHandlers } from "./service";
import type { ControlRequest, ControlService } from "./protocol";

export type DockerControlOptions = {
  socketPath?: string;
  candidateRoot?: string;
  corednsZonesRoot?: string;
  containers?: Partial<Record<ControlService, string>>;
};

export function createDockerServiceControl(
  options: DockerControlOptions = {},
): FixedServiceControl {
  const docker = new Docker({ socketPath: options.socketPath ?? "/var/run/docker.sock" });
  const candidateRoot = options.candidateRoot ?? "/var/lib/proxycore/candidates";
  const corednsZonesRoot = options.corednsZonesRoot ?? "/var/lib/proxycore/coredns-zones";
  const containers = {
    coredns: options.containers?.coredns ?? "proxycore-coredns",
    nginx: options.containers?.nginx ?? "proxycore-nginx",
  };
  const execute = (request: ControlRequest) =>
    executeDockerOperation(
      docker,
      request,
      candidateRoot,
      corednsZonesRoot,
      containers[request.service],
    );
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
  corednsZonesRoot: string,
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
        await promoteCoreDns(container, request.candidatePath, candidateRoot, corednsZonesRoot);
      }
      return { status: "promoted", revisionId: request.revisionId };
    case "reload":
      if (request.service === "nginx") {
        await exec(container, ["nginx", "-s", "reload"]);
      } else {
        await container.restart({ t: 10 });
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
        await rollbackCoreDns(container, candidateRoot, corednsZonesRoot);
      }
      return { status: "rolled-back" };
  }
}

async function promoteCoreDns(
  container: Docker.Container,
  candidatePath: string,
  candidateRoot: string,
  zonesRoot: string,
): Promise<void> {
  const previousCorefileArchive = await readArchive(container, "/etc/coredns/Corefile");
  const previousCorefilePath = join(candidateRoot, ".proxycore-previous-coredns-corefile.tar");
  const previousZonesPath = join(candidateRoot, ".proxycore-previous-coredns-zones");
  await writeFile(previousCorefilePath, previousCorefileArchive, { mode: 0o600 });
  await rm(previousZonesPath, { recursive: true, force: true });
  await copyDirectory(zonesRoot, previousZonesPath);

  try {
    await replaceDirectory(zonesRoot, join(candidatePath, "zones"));
    const corefile = await readFile(join(candidatePath, "Corefile"));
    await container.putArchive(createTar([{ name: "Corefile", contents: corefile }]), {
      path: "/etc/coredns",
    });
  } catch (error) {
    await replaceDirectory(zonesRoot, previousZonesPath).catch(() => undefined);
    throw error;
  }
}

async function rollbackCoreDns(
  container: Docker.Container,
  candidateRoot: string,
  zonesRoot: string,
): Promise<void> {
  const previousCorefilePath = join(candidateRoot, ".proxycore-previous-coredns-corefile.tar");
  const previousZonesPath = join(candidateRoot, ".proxycore-previous-coredns-zones");
  await container.putArchive(await readFile(previousCorefilePath), { path: "/etc/coredns" });
  await replaceDirectory(zonesRoot, previousZonesPath);
}

async function replaceDirectory(target: string, source: string): Promise<void> {
  await clearDirectory(target);
  await copyDirectory(source, target);
}

async function clearDirectory(directory: string): Promise<void> {
  await mkdir(directory, { recursive: true, mode: 0o755 });
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    await rm(join(directory, entry.name), { recursive: true, force: true });
  }
}

async function copyDirectory(source: string, target: string): Promise<void> {
  await mkdir(source, { recursive: true, mode: 0o755 });
  await mkdir(target, { recursive: true, mode: 0o755 });
  for (const entry of await readdir(source, { withFileTypes: true })) {
    const sourcePath = join(source, entry.name);
    const targetPath = join(target, entry.name);
    if (entry.isDirectory()) {
      await copyDirectory(sourcePath, targetPath);
      continue;
    }
    if (!entry.isFile()) {
      throw new Error(`Unsupported file type in CoreDNS zone directory: ${entry.name}`);
    }
    await copyFile(sourcePath, targetPath);
    await chmod(targetPath, 0o644);
  }
}

async function readArchive(container: Docker.Container, path: string): Promise<Buffer> {
  const stream = await container.getArchive({ path });
  return new Promise<Buffer>((resolve, reject) => {
    const chunks: Buffer[] = [];
    stream.on("data", (chunk: Buffer | string) => {
      chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
    });
    stream.on("end", () => resolve(Buffer.concat(chunks)));
    stream.on("error", reject);
    stream.resume();
  });
}

function createTar(entries: Array<{ name: string; contents: Buffer }>): Buffer {
  const blocks: Buffer[] = [];
  for (const entry of entries) {
    const header = Buffer.alloc(512);
    writeTarString(header, 0, 100, entry.name);
    writeTarOctal(header, 100, 8, 0o644);
    writeTarOctal(header, 108, 8, 0);
    writeTarOctal(header, 116, 8, 0);
    writeTarOctal(header, 124, 12, entry.contents.length);
    writeTarOctal(header, 136, 12, 0);
    header[156] = 0x30;
    writeTarString(header, 257, 6, "ustar");
    writeTarString(header, 263, 2, "00");
    writeTarString(header, 265, 32, "root");
    writeTarString(header, 297, 32, "root");
    header.fill(0x20, 148, 156);
    const checksum = header.reduce((sum, value) => sum + value, 0).toString(8).padStart(6, "0");
    header.write(checksum, 148, 6, "ascii");
    header[154] = 0;
    header[155] = 0x20;
    blocks.push(header, entry.contents);
    const padding = (512 - (entry.contents.length % 512)) % 512;
    if (padding > 0) blocks.push(Buffer.alloc(padding));
  }
  blocks.push(Buffer.alloc(1024));
  return Buffer.concat(blocks);
}

function writeTarString(header: Buffer, offset: number, length: number, value: string): void {
  Buffer.from(value, "ascii").subarray(0, length).copy(header, offset);
}

function writeTarOctal(header: Buffer, offset: number, length: number, value: number): void {
  const text = value.toString(8).padStart(length - 1, "0");
  header.write(`${text}\0`, offset, length, "ascii");
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
    stream.on("data", () => undefined);
    stream.on("end", resolve);
    stream.on("error", reject);
    stream.resume();
  });
  const result = await process.inspect();
  if (result.ExitCode !== 0) {
    throw new Error(`Fixed service operation failed with exit ${result.ExitCode ?? "unknown"}`);
  }
}
