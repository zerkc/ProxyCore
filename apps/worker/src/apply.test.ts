import { describe, expect, it } from "vitest";
import { InMemoryJobStore, InMemoryRevisionStore } from "@proxycore/db";
import type { ControlRequest, ControlResponse } from "../../control/src/protocol";
import { ApplyOrchestrator, type ControlClient } from "./apply";

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

function setup(failedOperation?: string) {
  const revisions = new InMemoryRevisionStore();
  const revision = revisions.create({ zones: [] });
  const jobs = new InMemoryJobStore();
  const job = jobs.enqueue({
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
    const { worker, jobs, revisions, revision, job, control } = setup();
    const result = await worker.apply(job.id, { zones: [] }, () => ({
      service: "coredns",
      candidatePath: "/candidates/revision-1",
      checksum: "abc",
    }));

    expect(result.status).toBe("applied");
    expect(revisions.get(revision.id)?.appliedAt).toBeInstanceOf(Date);
    expect(control.operations).toEqual(["stage", "validate", "promote", "reload", "health"]);
    expect(jobs.get(job.id)?.healthOutput).toBeDefined();
  });

  it("leaves the active revision unchanged when validation fails", async () => {
    const { worker, revisions, revision, job, control } = setup("validate");
    const result = await worker.apply(job.id, { zones: [] }, () => ({
      service: "coredns",
      candidatePath: "/candidates/revision-1",
      checksum: "abc",
    }));

    expect(result.status).toBe("failed");
    expect(revisions.get(revision.id)?.appliedAt).toBeUndefined();
    expect(control.operations).toEqual(["stage", "validate"]);
  });

  it("rolls back after a post-promotion health failure", async () => {
    const { worker, job, control } = setup("health");
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
