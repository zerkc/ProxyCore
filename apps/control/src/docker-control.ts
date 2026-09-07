import Docker from "dockerode";
import { randomUUID } from "node:crypto";
import {
  access,
  chmod,
  copyFile,
  mkdir,
  readFile,
  readdir,
  rename,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
import { join, dirname } from "node:path";
import { FixedServiceControl, type FixedHandlers } from "./service";
import type { ControlRequest, ControlService } from "./protocol";

export type DockerControlOptions = {
  socketPath?: string;
  candidateRoot?: string;
  corednsZonesRoot?: string;
  /** Persisted Corefile directory (mounted into CoreDNS as /etc/coredns-config). */
  corednsConfigRoot?: string;
  containers?: Partial<Record<ControlService, string>>;
};

export function createDockerServiceControl(
  options: DockerControlOptions = {},
): FixedServiceControl {
  const docker = new Docker({
    socketPath: options.socketPath ?? "/var/run/docker.sock",
  });
  const candidateRoot =
    options.candidateRoot ?? "/var/lib/proxycore/candidates";
  const corednsZonesRoot =
    options.corednsZonesRoot ?? "/var/lib/proxycore/coredns-zones";
  const corednsConfigRoot =
    options.corednsConfigRoot ?? "/var/lib/proxycore/coredns-config";
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
      corednsConfigRoot,
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
  corednsConfigRoot: string,
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
        await promoteNginx(container, request.candidatePath, candidateRoot);
      } else {
        await promoteCoreDns(
          container,
          request.candidatePath,
          candidateRoot,
          corednsZonesRoot,
          corednsConfigRoot,
        );
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
        await rollbackNginx(container, candidateRoot);
      } else {
        await rollbackCoreDns(
          container,
          candidateRoot,
          corednsZonesRoot,
          corednsConfigRoot,
        );
      }
      return { status: "rolled-back" };
  }
}

export function corednsLiveCorefilePath(configRoot: string): string {
  return join(configRoot, "Corefile");
}

function corefileLooksApplied(contents: string): boolean {
  return contents.includes("file /etc/coredns/zones/");
}

/**
 * When the persisted Corefile volume is still the image default (forward-only)
 * but a previous apply left candidates on disk, restore the latest applied
 * Corefile so recreating CoreDNS does not drop managed zones.
 */
export async function seedLiveCorefileFromCandidates(
  candidateRoot: string,
  configRoot: string,
): Promise<string | undefined> {
  await mkdir(configRoot, { recursive: true, mode: 0o755 });
  const livePath = corednsLiveCorefilePath(configRoot);
  try {
    const current = await readFile(livePath, "utf8");
    if (corefileLooksApplied(current)) {
      return undefined;
    }
  } catch {
    // missing live Corefile
  }

  const latest = await findLatestAppliedCorefile(candidateRoot);
  if (!latest) return undefined;
  const contents = await readFile(latest, "utf8");
  if (!corefileLooksApplied(contents)) return undefined;
  await writeFile(livePath, contents, { mode: 0o644 });
  return latest;
}

async function findLatestAppliedCorefile(
  candidateRoot: string,
): Promise<string | undefined> {
  let bestPath: string | undefined;
  let bestMtime = 0;
  const visit = async (directory: string): Promise<void> => {
    let entries;
    try {
      entries = await readdir(directory, { withFileTypes: true });
    } catch {
      return;
    }
    for (const entry of entries) {
      const path = join(directory, entry.name);
      if (entry.isDirectory()) {
        await visit(path);
        continue;
      }
      if (!entry.isFile() || entry.name !== "Corefile") continue;
      // Worker layout: <candidateRoot>/<rev>/coredns/Corefile
      if (
        !path.includes("/coredns/Corefile") &&
        !path.endsWith(`${join("coredns", "Corefile")}`)
      ) {
        continue;
      }
      const info = await stat(path);
      if (info.mtimeMs >= bestMtime) {
        bestMtime = info.mtimeMs;
        bestPath = path;
      }
    }
  };
  await visit(candidateRoot);
  return bestPath;
}

async function promoteCoreDns(
  container: Docker.Container,
  candidatePath: string,
  candidateRoot: string,
  zonesRoot: string,
  configRoot: string,
): Promise<void> {
  const liveCorefilePath = corednsLiveCorefilePath(configRoot);
  const previousCorefilePath = join(
    candidateRoot,
    ".proxycore-previous-coredns-corefile",
  );
  const previousZonesPath = join(
    candidateRoot,
    ".proxycore-previous-coredns-zones",
  );

  await mkdir(configRoot, { recursive: true, mode: 0o755 });
  let previousCorefile: Buffer;
  try {
    previousCorefile = await readFile(liveCorefilePath);
  } catch {
    previousCorefile = await readArchiveFile(
      container,
      "/etc/coredns-config/Corefile",
    ).catch(() => readArchiveFile(container, "/etc/coredns/Corefile"));
  }
  await writeFile(previousCorefilePath, previousCorefile, { mode: 0o600 });
  await rm(previousZonesPath, { recursive: true, force: true });
  await copyDirectory(zonesRoot, previousZonesPath);

  try {
    await replaceDirectory(zonesRoot, join(candidatePath, "zones"));
    const corefile = await readFile(join(candidatePath, "Corefile"));
    // Persist on the shared volume so container recreate keeps applied config.
    await writeFile(liveCorefilePath, corefile, { mode: 0o644 });
    await container.putArchive(
      createTar([{ name: "Corefile", contents: corefile }]),
      {
        path: "/etc/coredns-config",
      },
    );
  } catch (error) {
    await writeFile(liveCorefilePath, previousCorefile, { mode: 0o644 }).catch(
      () => undefined,
    );
    await replaceDirectory(zonesRoot, previousZonesPath).catch(() => undefined);
    throw error;
  }
}

async function rollbackCoreDns(
  container: Docker.Container,
  candidateRoot: string,
  zonesRoot: string,
  configRoot: string,
): Promise<void> {
  const liveCorefilePath = corednsLiveCorefilePath(configRoot);
  const previousCorefilePath = join(
    candidateRoot,
    ".proxycore-previous-coredns-corefile",
  );
  const previousZonesPath = join(
    candidateRoot,
    ".proxycore-previous-coredns-zones",
  );
  const previousCorefile = await readFile(previousCorefilePath);
  await mkdir(configRoot, { recursive: true, mode: 0o755 });
  await writeFile(liveCorefilePath, previousCorefile, { mode: 0o644 });
  await container.putArchive(
    createTar([{ name: "Corefile", contents: previousCorefile }]),
    {
      path: "/etc/coredns-config",
    },
  );
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
      throw new Error(
        `Unsupported file type in CoreDNS zone directory: ${entry.name}`,
      );
    }
    await copyFile(sourcePath, targetPath);
    await chmod(targetPath, 0o644);
  }
}

async function readArchiveFile(
  container: Docker.Container,
  path: string,
): Promise<Buffer> {
  return extractFirstFileFromTar(await readArchive(container, path));
}

async function readArchive(
  container: Docker.Container,
  path: string,
): Promise<Buffer> {
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

/** Extract the first regular file payload from a ustar archive. */
export function extractFirstFileFromTar(archive: Buffer): Buffer {
  let offset = 0;
  while (offset + 512 <= archive.length) {
    const header = archive.subarray(offset, offset + 512);
    offset += 512;
    if (header.every((byte) => byte === 0)) {
      break;
    }
    const sizeText = header
      .subarray(124, 136)
      .toString("ascii")
      .replace(/\0.*$/, "")
      .trim();
    const size = Number.parseInt(sizeText, 8) || 0;
    const typeFlag = header[156];
    const payload = archive.subarray(offset, offset + size);
    offset += size + ((512 - (size % 512)) % 512);
    if (typeFlag === 0 || typeFlag === 0x30) {
      return Buffer.from(payload);
    }
  }
  throw new Error("CoreDNS Corefile archive did not contain a file");
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
    const checksum = header
      .reduce((sum, value) => sum + value, 0)
      .toString(8)
      .padStart(6, "0");
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

function writeTarString(
  header: Buffer,
  offset: number,
  length: number,
  value: string,
): void {
  Buffer.from(value, "ascii").subarray(0, length).copy(header, offset);
}

function writeTarOctal(
  header: Buffer,
  offset: number,
  length: number,
  value: number,
): void {
  const text = value.toString(8).padStart(length - 1, "0");
  header.write(`${text}\0`, offset, length, "ascii");
}

// ── Nginx stable-config helpers ──────────────────────────────────────────────

export const NGINX_STABLE_CONFIG = "nginx-live.conf";
export const NGINX_PREVIOUS_STABLE_CONFIG = "nginx-previous-live.conf";

export function nginxLiveConfigPath(candidateRoot: string): string {
  return join(candidateRoot, NGINX_STABLE_CONFIG);
}

export function nginxPreviousStableConfigPath(candidateRoot: string): string {
  return join(candidateRoot, NGINX_PREVIOUS_STABLE_CONFIG);
}

/**
 * Find the newest legacy nginx.conf candidate under candidateRoot.
 * Only considers paths matching <candidateRoot>/<rev>/nginx/nginx.conf.
 * Returns the absolute path or undefined.
 */
export async function findNewestLegacyNginxCandidate(
  candidateRoot: string,
): Promise<string | undefined> {
  let bestPath: string | undefined;
  let bestMtime = 0;

  async function visit(directory: string): Promise<void> {
    let entries;
    try {
      entries = await readdir(directory, { withFileTypes: true });
    } catch {
      return;
    }
    for (const entry of entries) {
      const path = join(directory, entry.name);
      if (entry.isDirectory()) {
        await visit(path);
        continue;
      }
      // Legacy layout: <candidateRoot>/<rev>/nginx/nginx.conf
      if (!entry.isFile() || entry.name !== "nginx.conf") continue;
      if (
        !path.includes("/nginx/nginx.conf") &&
        !path.endsWith(join("nginx", "nginx.conf"))
      ) {
        continue;
      }
      const info = await stat(path);
      if (info.mtimeMs >= bestMtime) {
        bestMtime = info.mtimeMs;
        bestPath = path;
      }
    }
  }

  await visit(candidateRoot);
  return bestPath;
}

/**
 * Atomically write the stable Nginx config using a temp file + rename while
 * retaining the previous stable config for rollback.
 */
export async function writeStableNginxConfig(
  candidateRoot: string,
  contents: Buffer,
  previousContents?: Buffer,
): Promise<void> {
  const stablePath = nginxLiveConfigPath(candidateRoot);
  const previousStablePath = nginxPreviousStableConfigPath(candidateRoot);

  await mkdir(dirname(stablePath), { recursive: true, mode: 0o755 });
  if (previousContents && previousContents.length > 0) {
    await writeFileAtomically(previousStablePath, previousContents);
  } else {
    await rm(previousStablePath, { force: true });
  }
  await writeFileAtomically(stablePath, contents);
}

async function readFileIfPresent(path: string): Promise<Buffer | undefined> {
  try {
    return await readFile(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return undefined;
    }
    throw error;
  }
}

async function writeFileAtomically(
  path: string,
  contents: Buffer,
  mode = 0o644,
): Promise<void> {
  const temporaryPath = `${path}.tmp-${process.pid}-${randomUUID()}`;
  try {
    await writeFile(temporaryPath, contents, { mode });
    await rename(temporaryPath, path);
  } finally {
    await rm(temporaryPath, { force: true });
  }
}

// ── Nginx promote / rollback ─────────────────────────────────────────────────

/**
 * Promote an Nginx candidate and persist it only after the container copy
 * succeeds. The container snapshot is kept as a second rollback guard for
 * the first apply, before a stable config exists on the shared volume.
 */
async function promoteNginx(
  container: Docker.Container,
  candidatePath: string,
  candidateRoot: string,
): Promise<void> {
  const candidateConfigPath = join(candidatePath, "nginx.conf");
  const candidateContents = await readFile(candidateConfigPath);
  const previousContainerConfig = await readArchiveFile(
    container,
    "/etc/nginx/nginx.conf",
  ).catch(() => undefined);

  try {
    await exec(container, [
      "cp",
      "/etc/nginx/nginx.conf",
      "/etc/nginx/.proxycore.previous.conf",
    ]);
  } catch {
    // First apply or a freshly recreated container without a live config.
  }

  try {
    await exec(container, ["cp", candidateConfigPath, "/etc/nginx/nginx.conf"]);
    await writeStableNginxConfig(
      candidateRoot,
      candidateContents,
      previousContainerConfig,
    );
  } catch (error) {
    try {
      if (previousContainerConfig) {
        await container.putArchive(
          createTar([
            { name: "nginx.conf", contents: previousContainerConfig },
          ]),
          { path: "/etc/nginx" },
        );
      } else {
        await exec(container, [
          "cp",
          "/etc/nginx/.proxycore.previous.conf",
          "/etc/nginx/nginx.conf",
        ]);
      }
    } catch {
      // Preserve the original promotion error; the next health/recovery pass
      // will report the container state if the best-effort restore failed.
    }
    throw error;
  }
}

/**
 * Roll back Nginx to the previous stable config. The persistent previous file
 * is authoritative once at least one stable promotion has completed; the
 * in-container backup covers the first promotion from the baked default.
 */
async function rollbackNginx(
  container: Docker.Container,
  candidateRoot: string,
): Promise<void> {
  const stablePath = nginxLiveConfigPath(candidateRoot);
  const previousStablePath = nginxPreviousStableConfigPath(candidateRoot);
  const previousConfig = await readFileIfPresent(previousStablePath);

  if (previousConfig) {
    const currentStable = await readFileIfPresent(stablePath);
    await writeFileAtomically(stablePath, previousConfig);
    try {
      await container.putArchive(
        createTar([{ name: "nginx.conf", contents: previousConfig }]),
        { path: "/etc/nginx" },
      );
    } catch (error) {
      if (currentStable) {
        await writeFileAtomically(stablePath, currentStable).catch(
          () => undefined,
        );
      }
      throw error;
    }
    return;
  }

  await exec(container, [
    "cp",
    "/etc/nginx/.proxycore.previous.conf",
    "/etc/nginx/nginx.conf",
  ]);
  await rm(stablePath, { force: true });
}

// ── Candidate path assertion ────────────────────────────────────────────────

export function assertCandidatePath(
  candidatePath: string,
  candidateRoot: string,
): void {
  if (
    !candidatePath.startsWith(`${candidateRoot}/`) ||
    candidatePath.includes("..") ||
    candidatePath.includes("\0")
  ) {
    throw new Error("Candidate path is outside the fixed worker root");
  }
}

async function exec(
  container: Docker.Container,
  command: string[],
): Promise<void> {
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
    throw new Error(
      `Fixed service operation failed with exit ${result.ExitCode ?? "unknown"}`,
    );
  }
}
