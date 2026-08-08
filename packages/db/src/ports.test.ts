import { describe, expect, it } from "vitest";
import { InMemoryJobStore, InMemoryRevisionStore } from "./ports";

describe("in-memory persistence ports", () => {
  it("stores immutable snapshots with monotonic revision numbers", async () => {
    const store = new InMemoryRevisionStore();
    const revision = await store.create({ zones: [{ name: "home.arpa" }] });

    expect(revision.revisionNumber).toBe(1);
    expect((await store.latest())?.checksum).toHaveLength(64);
    expect((await store.markApplied(revision.id)).appliedAt).toBeInstanceOf(Date);
  });

  it("serializes jobs targeting the same service", async () => {
    const store = new InMemoryJobStore();
    const first = await store.enqueue({
      revisionId: "revision-1",
      target: "coredns",
      correlationId: "corr-1",
    });
    await store.enqueue({
      revisionId: "revision-2",
      target: "coredns",
      correlationId: "corr-2",
    });

    expect((await store.claimNext("coredns"))?.id).toBe(first.id);
    expect(await store.claimNext("coredns")).toBeUndefined();
    await store.update(first.id, { status: "applied" });
    expect((await store.claimNext("coredns"))?.correlationId).toBe("corr-2");
  });

  it("requeues a claim whose lease expired", async () => {
    const store = new InMemoryJobStore();
    const job = await store.enqueue({
      revisionId: "revision-1",
      target: "combined",
      correlationId: "corr-1",
    });
    const claimed = await store.claimNext();
    expect(claimed?.id).toBe(job.id);
    await store.update(job.id, { claimedAt: new Date(0) });

    expect(await store.recoverStale(1_000, new Date(5_000))).toBe(1);
    expect((await store.claimNext())?.id).toBe(job.id);
  });
});
