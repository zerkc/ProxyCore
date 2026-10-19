import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, expect, it } from "vitest";
import { FixedServiceControl } from "./service";
import { requestControl, startControlServer } from "./transport";

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => {
  for (const cleanup of cleanups.splice(0)) await cleanup();
});

it("logs a bounded stage when socket setup fails", async () => {
  const root = await mkdtemp(join(tmpdir(), "proxycore-control-setup-"));
  try {
    await writeFile(join(root, "blocker"), "file");
    const lines: string[] = [];
    await expect(
      startControlServer(
        join(root, "blocker", "socket"),
        new FixedServiceControl({}),
        (line) => lines.push(line),
      ),
    ).rejects.toThrow();
    expect(lines).toEqual([
      "control socket failed reason=setup_failed stage=mkdir",
    ]);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

it("logs a safe request ID and operation when control fails without changing its response", async () => {
  const root = await mkdtemp(join(tmpdir(), "proxycore-control-log-"));
  const socket = join(root, "control.sock");
  const lines: string[] = [];
  const server = await startControlServer(
    socket,
    new FixedServiceControl({
      coredns: {
        validate: async () => {
          throw new Error("private credential");
        },
      },
    }),
    (line) => lines.push(line),
  );
  cleanups.push(async () => {
    await new Promise<void>((resolve) => server.close(() => resolve()));
    await rm(root, { recursive: true, force: true });
  });
  const response = await requestControl(socket, {
    requestId: "request-1",
    operation: "validate",
    service: "coredns",
    revisionId: "revision-1",
    checksum: "secret-checksum",
    candidatePath: "/private/candidate",
  });
  expect(response).toMatchObject({
    requestId: "request-1",
    operation: "validate",
    ok: false,
    error: "private credential",
  });
  expect(lines).toEqual([
    expect.stringContaining(
      "request=request-1 operation=validate service=coredns",
    ),
  ]);
  expect(lines[0]).not.toMatch(/credential|checksum|candidate/);
});
