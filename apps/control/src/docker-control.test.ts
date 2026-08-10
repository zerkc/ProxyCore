import { mkdtemp, mkdir, readFile, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import {
  assertCandidatePath,
  corednsLiveCorefilePath,
  extractFirstFileFromTar,
  seedLiveCorefileFromCandidates,
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
    const payload = Buffer.from("home.arpa:53 {\n    file /etc/coredns/zones/home.arpa.zone\n}\n");
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

    const seeded = await seedLiveCorefileFromCandidates(candidateRoot, configRoot);
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

    const seeded = await seedLiveCorefileFromCandidates(candidateRoot, configRoot);
    expect(seeded).toBeUndefined();
    const live = await readFile(corednsLiveCorefilePath(configRoot), "utf8");
    expect(live).toContain("keep.com");
  });
});

function createMinimalTar(name: string, contents: Buffer): Buffer {
  const header = Buffer.alloc(512);
  Buffer.from(name, "ascii").copy(header, 0);
  header.write(`${contents.length.toString(8).padStart(11, "0")}\0`, 124, 12, "ascii");
  header[156] = 0x30;
  Buffer.from("ustar", "ascii").copy(header, 257);
  Buffer.from("00", "ascii").copy(header, 263);
  header.fill(0x20, 148, 156);
  const checksum = header.reduce((sum, value) => sum + value, 0).toString(8).padStart(6, "0");
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
