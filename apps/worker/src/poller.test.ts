import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { InMemoryJobStore, InMemoryRevisionStore } from "@proxycore/db";
import type { ControlRequest, ControlResponse } from "../../control/src/protocol";
import { ApplyOrchestrator, type ControlClient } from "./apply";
import { pollOnce, WorkerWakeup } from "./poller";

class SuccessfulControl implements ControlClient {
  readonly operations: string[] = [];

  async execute(request: ControlRequest): Promise<ControlResponse> {
    this.operations.push(`${request.service}:${request.operation}`);
    return {
      requestId: request.requestId,
      operation: request.operation,
      ok: true,
      output: { service: request.service, operation: request.operation },
    };
  }
}

describe("worker polling", () => {
  it("wakes before the reconciliation interval when notified", async () => {
    const wakeup = new WorkerWakeup();
    const pending = wakeup.wait(60_000);
    wakeup.notify();

    await expect(pending).resolves.toBeUndefined();
  });

  it("claims a persistent job and applies both data-plane candidates", async () => {
    const root = await mkdtemp(join(tmpdir(), "proxycore-worker-"));
    try {
      const revisions = new InMemoryRevisionStore();
      const revision = await revisions.create({
        settings: {
          ingress: { ipv4: "192.0.2.10" },
          defaultPool: {
            id: "default",
            endpoints: [{ host: "1.1.1.1", port: 53 }],
          },
          forwardingRules: [],
          retentionMaxAgeDays: 7,
          retentionMaxSizeMb: 50,
        },
        zones: [],
        streams: [],
        certificates: [],
      });
      const jobs = new InMemoryJobStore();
      await jobs.enqueue({
        revisionId: revision.id,
        target: "combined",
        correlationId: "poll-1",
      });
      const control = new SuccessfulControl();
      const orchestrator = new ApplyOrchestrator({
        jobs,
        revisions,
        control,
        candidateRoot: root,
      });

      const result = await pollOnce({
        jobs,
        revisions,
        orchestrator,
        renderOptions: { candidateRoot: root },
      });

      expect(result?.status).toBe("applied");
      expect((await revisions.get(revision.id))?.appliedAt).toBeInstanceOf(Date);
      expect(control.operations).toEqual([
        "coredns:stage",
        "coredns:validate",
        "nginx:stage",
        "nginx:validate",
        "coredns:promote",
        "nginx:promote",
        "coredns:reload",
        "coredns:health",
        "nginx:reload",
        "nginx:health",
      ]);
      expect(
        await readFile(join(root, revision.id, "coredns", "Corefile"), "utf8"),
      ).toContain("forward . 1.1.1.1:53");
      expect(
        await readFile(join(root, revision.id, "nginx", "nginx.conf"), "utf8"),
      ).toContain("events {}");
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  });
});
