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
  readonly requests: ControlRequest[] = [];

  constructor(
    private readonly nginxHealthStatuses: Array<"healthy" | "drift"> = [
      "healthy",
    ],
    private readonly failedOperation?: ControlRequest["operation"],
    private readonly beforeExecute?: (
      request: ControlRequest,
    ) => Promise<void> | void,
  ) {}

  private nginxHealthProbe = 0;
  private failureConsumed = false;

  private nextNginxHealthStatus(): "healthy" | "drift" {
    const index = Math.min(
      this.nginxHealthProbe,
      this.nginxHealthStatuses.length - 1,
    );
    this.nginxHealthProbe += 1;
    return this.nginxHealthStatuses[index] ?? "healthy";
  }

  async execute(request: ControlRequest): Promise<ControlResponse> {
    this.operations.push(`${request.service}:${request.operation}`);
    this.requests.push(request);
    await this.beforeExecute?.(request);
    const failed =
      request.operation === this.failedOperation && !this.failureConsumed;
    if (failed) this.failureConsumed = true;
    return {
      requestId: request.requestId,
      operation: request.operation,
      ok: !failed,
      output: failed
        ? undefined
        : request.service === "nginx" && request.operation === "health"
          ? { status: this.nextNginxHealthStatus() }
          : { service: request.service, operation: request.operation },
      error: failed ? "simulated failure" : undefined,
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

  it("repairs an applied revision after the ordinary queue drains", async () => {
    const root = await mkdtemp(join(tmpdir(), "proxycore-worker-reconcile-"));
    try {
      const revisions = new InMemoryRevisionStore();
      const revision = await revisions.create(reconciliationSnapshot());
      await revisions.markApplied(revision.id);
      const jobs = new InMemoryJobStore();
      const control = new SuccessfulControl(["drift", "healthy"]);
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
      expect(result?.target).toBe("nginx");
      expect(control.operations).toEqual([
        "nginx:health",
        "nginx:stage",
        "nginx:validate",
        "nginx:promote",
        "nginx:reload",
        "nginx:health",
      ]);
      const recordedJobs = await jobs.list();
      expect(recordedJobs).toHaveLength(1);
      expect(recordedJobs[0]?.correlationId).toBe(
        `reconcile-nginx:${revision.id}`,
      );
      expect(
        await readFile(join(root, revision.id, "nginx", "nginx.conf"), "utf8"),
      ).toContain("events {}");
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  });

  it("does not reconcile a healthy applied revision", async () => {
    const revisions = new InMemoryRevisionStore();
    const revision = await revisions.create(reconciliationSnapshot());
    await revisions.markApplied(revision.id);
    const jobs = new InMemoryJobStore();
    const control = new SuccessfulControl();
    const orchestrator = new ApplyOrchestrator({ jobs, revisions, control });

    const result = await pollOnce({ jobs, revisions, orchestrator });

    expect(result).toBeUndefined();
    expect(control.operations).toEqual(["nginx:health"]);
    expect(control.requests[0]?.candidatePath).toBe(
      join("/var/lib/proxycore/candidates", revision.id, "nginx"),
    );
    expect(await jobs.list()).toEqual([]);
  });

  it("lets claimed ordinary work arriving during the probe win over repair", async () => {
    const revisions = new InMemoryRevisionStore();
    const revision = await revisions.create(reconciliationSnapshot());
    await revisions.markApplied(revision.id);
    const jobs = new InMemoryJobStore();
    let ordinaryEnqueued = false;
    const control = new SuccessfulControl(
      ["drift"],
      undefined,
      async (request) => {
        if (
          !ordinaryEnqueued &&
          request.service === "nginx" &&
          request.operation === "health"
        ) {
          ordinaryEnqueued = true;
          await jobs.enqueue({
            revisionId: "ordinary-revision",
            target: "combined",
            correlationId: "ordinary-1",
          });
          await jobs.claimNext();
        }
      },
    );
    const orchestrator = new ApplyOrchestrator({ jobs, revisions, control });

    const result = await pollOnce({ jobs, revisions, orchestrator });

    expect(result).toBeUndefined();
    expect(control.operations).toEqual(["nginx:health"]);
    expect(await jobs.list()).toMatchObject([
      {
        target: "combined",
        status: "validating",
        correlationId: "ordinary-1",
      },
    ]);
  });

  it("repairs a later drift after a previous reconciliation", async () => {
    const root = await mkdtemp(join(tmpdir(), "proxycore-worker-repeat-"));
    try {
      const revisions = new InMemoryRevisionStore();
      const revision = await revisions.create(reconciliationSnapshot());
      await revisions.markApplied(revision.id);
      const jobs = new InMemoryJobStore();
      const control = new SuccessfulControl([
        "drift",
        "healthy",
        "drift",
        "healthy",
      ]);
      const orchestrator = new ApplyOrchestrator({
        jobs,
        revisions,
        control,
        candidateRoot: root,
      });
      const options = {
        jobs,
        revisions,
        orchestrator,
        renderOptions: { candidateRoot: root },
      };

      const first = await pollOnce(options);
      const second = await pollOnce(options);

      expect(first?.status).toBe("applied");
      expect(second?.status).toBe("applied");
      expect(control.operations).toEqual([
        "nginx:health",
        "nginx:stage",
        "nginx:validate",
        "nginx:promote",
        "nginx:reload",
        "nginx:health",
        "nginx:health",
        "nginx:stage",
        "nginx:validate",
        "nginx:promote",
        "nginx:reload",
        "nginx:health",
      ]);
      expect(await jobs.list()).toHaveLength(2);
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  });

  it("retries later drift after a failed reconciliation", async () => {
    const root = await mkdtemp(join(tmpdir(), "proxycore-worker-retry-"));
    try {
      const revisions = new InMemoryRevisionStore();
      const revision = await revisions.create(reconciliationSnapshot());
      await revisions.markApplied(revision.id);
      const jobs = new InMemoryJobStore();
      const control = new SuccessfulControl(
        ["drift", "drift", "healthy"],
        "stage",
      );
      const orchestrator = new ApplyOrchestrator({
        jobs,
        revisions,
        control,
        candidateRoot: root,
      });
      const options = {
        jobs,
        revisions,
        orchestrator,
        renderOptions: { candidateRoot: root },
      };

      const first = await pollOnce(options);
      const second = await pollOnce(options);

      expect(first?.status).toBe("failed");
      expect(second?.status).toBe("applied");
      expect(await jobs.list()).toHaveLength(2);
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  });
});

function reconciliationSnapshot() {
  return {
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
  };
}
