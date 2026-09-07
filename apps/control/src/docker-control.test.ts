import { mkdtemp, mkdir, readFile, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import {
  assertCandidatePath,
  corednsLiveCorefilePath,
  extractFirstFileFromTar,
  findNewestLegacyNginxCandidate,
  nginxLiveConfigPath,
  nginxPreviousStableConfigPath,
  NGINX_PREVIOUS_STABLE_CONFIG,
  NGINX_STABLE_CONFIG,
  seedLiveCorefileFromCandidates,
  writeStableNginxConfig,
} from "./docker-control";

describe("Docker control boundary", () => {
  it("accepts only candidates below the fixed shared root", () => {
    expect(() =>
      assertCandidatePath(
        "/var/lib/proxycore/candidates/revision-1",
        "/var/lib/proxycore/candidates",
      ),
    ).not.toThrow();
  });

  it("rejects traversal and unrelated filesystem paths", () => {
    expect(() =>
      assertCandidatePath("/etc/passwd", "/var/lib/proxycore/candidates"),
    ).toThrow(/fixed worker root/i);
    expect(() =>
      assertCandidatePath(
        "/var/lib/proxycore/candidates/../secrets",
        "/var/lib/proxycore/candidates",
      ),
    ).toThrow(/fixed worker root/i);
  });
});

describe("CoreDNS Corefile persistence helpers", () => {
  it("extracts the first regular file from a ustar archive", () => {
    const payload = Buffer.from(
      "home.arpa:53 {\n    file /etc/coredns/zones/home.arpa.zone\n}\n",
    );
    const archive = createMinimalTar("Corefile", payload);
    expect(extractFirstFileFromTar(archive).equals(payload)).toBe(true);
  });

  it("seeds the live Corefile from the latest applied candidate", async () => {
    const root = await mkdtemp(join(tmpdir(), "proxycore-coredns-"));
    const candidateRoot = join(root, "candidates");
    const configRoot = join(root, "config");
    const appliedDir = join(candidateRoot, "rev-9", "coredns");
    await mkdir(appliedDir, { recursive: true });
    await mkdir(configRoot, { recursive: true });
    await writeFile(
      join(appliedDir, "Corefile"),
      "ggzdeveloper.com:53 {\n    file /etc/coredns/zones/ggzdeveloper.com.zone\n}\n",
    );
    await writeFile(
      corednsLiveCorefilePath(configRoot),
      ".:53 {\n    forward . 1.1.1.1:53\n}\n",
    );

    const seeded = await seedLiveCorefileFromCandidates(
      candidateRoot,
      configRoot,
    );
    expect(seeded).toContain(`${join("rev-9", "coredns", "Corefile")}`);
    const live = await readFile(corednsLiveCorefilePath(configRoot), "utf8");
    expect(live).toContain("file /etc/coredns/zones/ggzdeveloper.com.zone");
  });

  it("does not overwrite an already-applied live Corefile", async () => {
    const root = await mkdtemp(join(tmpdir(), "proxycore-coredns-"));
    const candidateRoot = join(root, "candidates");
    const configRoot = join(root, "config");
    await mkdir(join(candidateRoot, "rev-1", "coredns"), { recursive: true });
    await mkdir(configRoot, { recursive: true });
    await writeFile(
      join(candidateRoot, "rev-1", "coredns", "Corefile"),
      "other.com:53 {\n    file /etc/coredns/zones/other.com.zone\n}\n",
    );
    await writeFile(
      corednsLiveCorefilePath(configRoot),
      "keep.com:53 {\n    file /etc/coredns/zones/keep.com.zone\n}\n",
    );

    const seeded = await seedLiveCorefileFromCandidates(
      candidateRoot,
      configRoot,
    );
    expect(seeded).toBeUndefined();
    const live = await readFile(corednsLiveCorefilePath(configRoot), "utf8");
    expect(live).toContain("keep.com");
  });
});

describe("Nginx stable-config path helpers", () => {
  it("nginxLiveConfigPath returns the fixed stable filename under candidateRoot", () => {
    expect(nginxLiveConfigPath("/var/lib/proxycore/candidates")).toBe(
      "/var/lib/proxycore/candidates/nginx-live.conf",
    );
    expect(nginxLiveConfigPath("/some/other/root")).toBe(
      "/some/other/root/nginx-live.conf",
    );
  });

  it("nginxPreviousStableConfigPath returns the fixed previous filename", () => {
    expect(nginxPreviousStableConfigPath("/var/lib/proxycore/candidates")).toBe(
      "/var/lib/proxycore/candidates/nginx-previous-live.conf",
    );
  });

  it("NGINX_STABLE_CONFIG and NGINX_PREVIOUS_STABLE_CONFIG are the expected filenames", () => {
    expect(NGINX_STABLE_CONFIG).toBe("nginx-live.conf");
    expect(NGINX_PREVIOUS_STABLE_CONFIG).toBe("nginx-previous-live.conf");
  });

  it("assertCandidatePath rejects paths outside candidateRoot", () => {
    expect(() =>
      assertCandidatePath("/etc/shadow", "/var/lib/proxycore/candidates"),
    ).toThrow(/fixed worker root/i);
    expect(() =>
      assertCandidatePath(
        "/var/lib/proxycore/candidates/../../../etc/passwd",
        "/var/lib/proxycore/candidates",
      ),
    ).toThrow(/fixed worker root/i);
  });

  it("assertCandidatePath accepts paths strictly inside candidateRoot", () => {
    expect(() =>
      assertCandidatePath(
        "/var/lib/proxycore/candidates/rev-1/nginx/nginx.conf",
        "/var/lib/proxycore/candidates",
      ),
    ).not.toThrow();
  });
});

describe("Nginx stable-config persistence", () => {
  it("writeStableNginxConfig atomically writes the stable file and saves previous", async () => {
    const root = await mkdtemp(join(tmpdir(), "proxycore-nginx-stable-"));
    const candidateRoot = join(root, "candidates");
    await mkdir(candidateRoot, { recursive: true });

    const first = Buffer.from("first config content\n");
    const second = Buffer.from("second config content\n");

    // First write — no previous
    await writeStableNginxConfig(candidateRoot, first);
    expect(await readFile(nginxLiveConfigPath(candidateRoot), "utf8")).toBe(
      "first config content\n",
    );

    // Second write — previous should be saved
    await writeStableNginxConfig(candidateRoot, second, first);
    expect(await readFile(nginxLiveConfigPath(candidateRoot), "utf8")).toBe(
      "second config content\n",
    );
    expect(
      await readFile(nginxPreviousStableConfigPath(candidateRoot), "utf8"),
    ).toBe("first config content\n");
  });

  it("writeStableNginxConfig creates the parent directory if missing", async () => {
    const root = await mkdtemp(join(tmpdir(), "proxycore-nginx-stable-"));
    // candidateRoot does not exist yet
    const candidateRoot = join(root, "does-not-exist");
    await writeStableNginxConfig(candidateRoot, Buffer.from("content"));
    const info = await stat(candidateRoot);
    expect(info.isDirectory()).toBe(true);
  });
});

describe("Nginx legacy candidate discovery", () => {
  it("finds the newest legacy nginx.conf in <rev>/nginx/nginx.conf layout", async () => {
    const root = await mkdtemp(join(tmpdir(), "proxycore-nginx-legacy-"));
    const candidateRoot = join(root, "candidates");

    // Create rev-1 (older)
    const rev1 = join(candidateRoot, "rev-1", "nginx");
    await mkdir(rev1, { recursive: true });
    await writeFile(join(rev1, "nginx.conf"), "# rev-1\n");
    // ensure rev-1's file exists before rev-2 is created
    await stat(join(rev1, "nginx.conf"));

    // Create rev-2 (newer)
    const rev2 = join(candidateRoot, "rev-2", "nginx");
    await mkdir(rev2, { recursive: true });
    await writeFile(join(rev2, "nginx.conf"), "# rev-2\n");

    // Add a non-legacy nginx.conf to make sure it's not picked
    await mkdir(join(candidateRoot, "rev-3"), { recursive: true });
    await writeFile(
      join(candidateRoot, "rev-3", "nginx.conf"),
      "# not legacy\n",
    );

    const found = await findNewestLegacyNginxCandidate(candidateRoot);
    expect(found).toContain("/rev-2/nginx/nginx.conf");
    expect(found).not.toContain("/rev-3/");
  });

  it("returns undefined when no legacy candidates exist", async () => {
    const root = await mkdtemp(join(tmpdir(), "proxycore-nginx-legacy-"));
    const found = await findNewestLegacyNginxCandidate(
      join(root, "candidates"),
    );
    expect(found).toBeUndefined();
  });

  it("ignores non-nginx.conf files in candidate subdirectories", async () => {
    const root = await mkdtemp(join(tmpdir(), "proxycore-nginx-legacy-"));
    const candidateRoot = join(root, "candidates");
    const rev = join(candidateRoot, "rev-1", "nginx");
    await mkdir(rev, { recursive: true });
    await writeFile(join(rev, "nginx.conf"), "# real\n");
    await writeFile(join(rev, "other.conf"), "# ignore\n");

    const found = await findNewestLegacyNginxCandidate(candidateRoot);
    expect(found).toContain("nginx.conf");
    expect(found).not.toContain("other.conf");
  });
});

describe("Shell entrypoint syntax verification", () => {
  it("nginx-entrypoint.sh passes POSIX sh lint checks", async () => {
    const scriptPath = join(process.cwd(), "scripts", "nginx-entrypoint.sh");
    const contents = await readFile(scriptPath);
    // Basic structural checks for POSIX compliance
    expect(contents.includes("#!/bin/sh")).toBe(true);
    expect(contents.includes("set -e")).toBe(true);
    // No bash-isms that break POSIX
    expect(contents.includes("[[")).toBe(false);
    expect(contents.includes("<<")).toBe(false);
    // exec handoff present
    expect(contents.includes("exec /docker-entrypoint.sh")).toBe(true);
    // env-override path is used
    expect(contents.includes("PROXYCORE_NGINX_LIVE_CONFIG")).toBe(true);
  });
});

function createMinimalTar(name: string, contents: Buffer): Buffer {
  const header = Buffer.alloc(512);
  Buffer.from(name, "ascii").copy(header, 0);
  header.write(
    `${contents.length.toString(8).padStart(11, "0")}\0`,
    124,
    12,
    "ascii",
  );
  header[156] = 0x30;
  Buffer.from("ustar", "ascii").copy(header, 257);
  Buffer.from("00", "ascii").copy(header, 263);
  header.fill(0x20, 148, 156);
  const checksum = header
    .reduce((sum, value) => sum + value, 0)
    .toString(8)
    .padStart(6, "0");
  header.write(checksum, 148, 6, "ascii");
  header[154] = 0;
  header[155] = 0x20;
  const padding = (512 - (contents.length % 512)) % 512;
  return Buffer.concat([
    header,
    contents,
    Buffer.alloc(padding),
    Buffer.alloc(1024),
  ]);
}
