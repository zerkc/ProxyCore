import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { randomUUID } from "node:crypto";
import { describe, expect, it } from "vitest";
import { FixedServiceControl } from "../../control/src/service";
import { startControlServer } from "../../control/src/transport";
import type { ControlRequest } from "../../control/src/protocol";
import { UnixSocketControlClient } from "./control-client";

describe("UnixSocketControlClient", () => {
  it("round-trips an allowlisted request and validates correlation", async () => {
    const root = await mkdtemp(join(tmpdir(), "proxycore-control-"));
    const socketPath = join(root, "control.sock");
    const control = new FixedServiceControl({
      coredns: {
        health: async (request: ControlRequest) => ({
          service: request.service,
          healthy: true,
        }),
      },
    });
    const server = await startControlServer(socketPath, control);
    try {
      const client = new UnixSocketControlClient(socketPath);
      const request: ControlRequest = {
        requestId: randomUUID(),
        operation: "health",
        service: "coredns",
        revisionId: randomUUID(),
        checksum: "checksum",
        candidatePath: "/var/lib/proxycore/candidates/revision/coredns",
      };

      await expect(client.execute(request)).resolves.toMatchObject({
        requestId: request.requestId,
        operation: "health",
        ok: true,
        output: { service: "coredns", healthy: true },
      });
    } finally {
      await new Promise<void>((resolve) => server.close(() => resolve()));
      await rm(root, { recursive: true, force: true });
    }
  });
});
