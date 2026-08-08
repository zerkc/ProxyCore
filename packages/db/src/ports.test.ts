import { describe, expect, it } from "vitest";
import { InMemoryJobStore, InMemoryRevisionStore } from "./ports";

describe("in-memory persistence ports", () => {
  it("stores immutable snapshots with monotonic revision numbers", () => {
    const store = new InMemoryRevisionStore();
    const revision = store.create({ zones: [{ name: "home.arpa" }] });

    expect(revision.revisionNumber).toBe(1);
    expect(store.latest()?.checksum).toHaveLength(64);
    expect(store.markApplied(revision.id).appliedAt).toBeInstanceOf(Date);
  });

  it("serializes jobs targeting the same service", () => {
    const store = new InMemoryJobStore();
    const first = store.enqueue({
      revisionId: "revision-1",
      target: "coredns",
      correlationId: "corr-1",
    });
    store.enqueue({
      revisionId: "revision-2",
      target: "coredns",
      correlationId: "corr-2",
    });

    expect(store.claimNext("coredns")?.id).toBe(first.id);
    expect(store.claimNext("coredns")).toBeUndefined();
    store.update(first.id, { status: "applied" });
    expect(store.claimNext("coredns")?.correlationId).toBe("corr-2");
  });
});
