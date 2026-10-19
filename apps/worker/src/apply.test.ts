import { mkdtemp } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { InMemoryJobStore, InMemoryRevisionStore } from "@proxycore/db";
import type { ControlRequest, ControlResponse } from "../../control/src/protocol";
import { ApplyOrchestrator, type ControlClient } from "./apply";
import { renderJobCandidates } from "./render";

class FakeControl implements ControlClient {
  readonly operations: string[] = [];

  constructor(private readonly failedOperation?: string) {}

  async execute(request: ControlRequest): Promise<ControlResponse> {
    this.operations.push(request.operation);
    return {
      requestId: request.requestId,
      operation: request.operation,
      ok: request.operation !== this.failedOperation,
      output: request.operation === this.failedOperation ? undefined : { operation: request.operation },
      error: request.operation === this.failedOperation ? "simulated failure" : undefined,
    };
  }
}

async function setup(failedOperation?: string) {
  const revisions = new InMemoryRevisionStore();
  const revision = await revisions.create({ zones: [] });
  const jobs = new InMemoryJobStore();
  const job = await jobs.enqueue({
    revisionId: revision.id,
    target: "coredns",
    correlationId: "correlation-1",
  });
  const control = new FakeControl(failedOperation);
  const worker = new ApplyOrchestrator({ jobs, revisions, control });
  return { worker, jobs, revisions, revision, job, control };
}

describe("apply orchestrator", () => {
  it("validates, reloads, health-checks, and marks the revision applied", async () => {
    const { worker, jobs, revisions, revision, job, control } = await setup();
    const result = await worker.apply(job.id, { zones: [] }, () => ({
      service: "coredns",
      candidatePath: "/candidates/revision-1",
      checksum: "abc",
    }));

    expect(result.status).toBe("applied");
    expect((await revisions.get(revision.id))?.appliedAt).toBeInstanceOf(Date);
    expect(control.operations).toEqual(["stage", "validate", "promote", "reload", "health"]);
    expect((await jobs.get(job.id))?.healthOutput).toBeDefined();
  });

  it("renders the desired configuration from an envelope-backed revision", async () => {
    const { worker, job, control } = await setup();
    let renderedSnapshot: unknown;
    const desired = { settings: { defaultPool: { id: "pool-1" } }, zones: [] };
    const result = await worker.apply(
      job.id,
      {
        replicated: { configuration: desired },
      },
      (snapshot) => {
        renderedSnapshot = snapshot;
        return {
          service: "coredns",
          candidatePath: "/candidates/revision-1",
          checksum: "abc",
        };
      },
    );

    expect(result.status).toBe("applied");
    expect(renderedSnapshot).toEqual(desired);
    expect(control.operations).toEqual(["stage", "validate", "promote", "reload", "health"]);
  });

  it("renders a node's own ingress over the primary's settings.ingress", async () => {
    const { worker, job } = await setup();
    let renderedSnapshot: unknown;
    const desired = {
      settings: {
        ingress: { ipv4: "203.0.113.1", ipv6: "2001:db8::1" },
        defaultPool: { id: "pool-1" },
      },
      zones: [],
    };
    const envelope = {
      nodeLocal: { ingress: { ipv4: "198.51.100.77" } },
      replicated: { configuration: desired },
    };
    await worker.apply(job.id, envelope as never, (snapshot) => {
      renderedSnapshot = snapshot;
      return { service: "coredns", candidatePath: "/candidates/revision-1", checksum: "abc" };
    });

    expect(renderedSnapshot).toEqual({
      settings: { ingress: { ipv4: "198.51.100.77" }, defaultPool: { id: "pool-1" } },
      zones: [],
    });
    // The stored envelope (and so the primary's hash) is not mutated.
    expect(desired.settings.ingress).toEqual({ ipv4: "203.0.113.1", ipv6: "2001:db8::1" });
  });

  it("answers a proxied record with the node's ingress, not the primary's", async () => {
    const { jobs, revisions, job, control } = await setup();
    const candidateRoot = await mkdtemp(join(tmpdir(), "proxycore-candidates-"));
    const worker = new ApplyOrchestrator({ jobs, revisions, control, candidateRoot });
    const desired = {
      settings: {
        ingress: { ipv4: "203.0.113.1" },
        defaultPool: { id: "default", endpoints: [{ host: "1.1.1.1", port: 53 }] },
        forwardingRules: [],
        retentionMaxAgeDays: 7,
        retentionMaxSizeMb: 50,
      },
      zones: [
        {
          id: "zone-1",
          name: "example.test",
          enabled: true,
          records: [
            {
              id: "record-1",
              name: "app.example.test",
              type: "A",
              value: "192.0.2.20",
              ttl: 300,
              enabled: true,
              proxied: true,
              proxy: { origin: { ip: "192.0.2.20", port: 8080, protocol: "http" }, tlsEnabled: false },
            },
          ],
        },
      ],
      streams: [],
      certificates: [],
    };
    let zoneFile = "";
    const result = await worker.apply(
      job.id,
      {
        nodeLocal: { ingress: { ipv4: "198.51.100.77" } },
        replicated: { configuration: desired },
      } as never,
      async (snapshot) => {
        const candidates = await renderJobCandidates(snapshot, job, { candidateRoot });
        const candidate = (Array.isArray(candidates) ? candidates : [candidates]).find(
          (item) => item.service === "coredns",
        )!;
        zoneFile = Object.values(candidate.files ?? {}).join("\n");
        return candidate;
      },
    );

    expect(result.errorMessage).toBeUndefined();
    expect(result.status).toBe("applied");
    expect(zoneFile).toContain("198.51.100.77");
    expect(zoneFile).not.toContain("203.0.113.1");
  });

  it("leaves the active revision unchanged when validation fails", async () => {
    const { worker, revisions, revision, job, control } = await setup("validate");
    const result = await worker.apply(job.id, { zones: [] }, () => ({
      service: "coredns",
      candidatePath: "/candidates/revision-1",
      checksum: "abc",
    }));

    expect(result.status).toBe("failed");
    expect((await revisions.get(revision.id))?.appliedAt).toBeUndefined();
    expect(control.operations).toEqual(["stage", "validate"]);
  });

  it("logs a safe job, revision, service and step when a control operation fails", async () => {
    const { jobs, revisions, job, control } = await setup("validate");
    const logs: string[] = [];
    const worker = new ApplyOrchestrator({
      jobs,
      revisions,
      control,
      log: (line) => logs.push(line),
    });
    const result = await worker.apply(job.id, { zones: [] }, () => ({
      service: "coredns",
      candidatePath: "/candidates/private",
      checksum: "abc",
    }));
    expect(result.status).toBe("failed");
    expect(result.errorMessage).toBe("simulated failure");
    expect(logs).toEqual([
      expect.stringContaining("step=validate service=coredns"),
    ]);
    expect(logs[0]).toContain(`job=${job.id} revision=${job.revisionId}`);
    expect(logs[0]).toContain(`correlation=${job.correlationId}`);
    expect(logs[0]).not.toMatch(/simulated failure|private|abc/);
  });

  it("logs the job ID when lookup fails before rendering", async () => {
    const { jobs, revisions, control } = await setup();
    const logs: string[] = [];
    const worker = new ApplyOrchestrator({
      jobs,
      revisions,
      control,
      log: (line) => logs.push(line),
    });
    await expect(
      worker.apply("missing-job", { zones: [] }, () => {
        throw new Error("should not render");
      }),
    ).rejects.toThrow("Job not found");
    expect(logs).toEqual([
      expect.stringContaining(
        "job=missing-job revision=unavailable step=lookup",
      ),
    ]);
  });

  it("logs failed rollback without exposing control error text", async () => {
    const { jobs, revisions, job } = await setup();
    const logs: string[] = [];
    const control: ControlClient = {
      execute: async (request) => ({
        requestId: request.requestId,
        operation: request.operation,
        ok: request.operation !== "health" && request.operation !== "rollback",
        error: "private credential",
      }),
    };
    const worker = new ApplyOrchestrator({
      jobs,
      revisions,
      control,
      log: (line) => logs.push(line),
    });
    const result = await worker.apply(job.id, { zones: [] }, () => ({
      service: "coredns",
      candidatePath: "/private/candidate",
      checksum: "abc",
    }));
    expect(result.status).toBe("failed");
    expect(logs).toEqual([
      expect.stringContaining("step=health service=coredns"),
      expect.stringContaining(
        `worker rollback failed job=${job.id} revision=${job.revisionId} correlation=${job.correlationId} service=coredns`,
      ),
    ]);
    expect(logs.join("\n")).not.toMatch(/credential|candidate|abc/);
  });

  it("rolls back after a post-promotion health failure", async () => {
    const { worker, job, control } = await setup("health");
    const result = await worker.apply(job.id, { zones: [] }, () => ({
      service: "coredns",
      candidatePath: "/candidates/revision-1",
      checksum: "abc",
    }));

    expect(result.status).toBe("rolled-back");
    expect(control.operations).toEqual([
      "stage",
      "validate",
      "promote",
      "reload",
      "health",
      "rollback",
    ]);
  });
});
