import { describe, expect, it } from "vitest";
import {
  InMemoryContinuityPersistence,
  type EnrollmentAttemptRecord,
  type SnapshotAcknowledgement,
} from "./ports";

describe("phase 2 persistence ports", () => {
  it("keeps a node attempt and acknowledgement in one typed transaction", async () => {
    const store = new InMemoryContinuityPersistence();
    const attempt: EnrollmentAttemptRecord = {
      id: "attempt-1",
      state: "confirmed",
      primaryUrl: "https://primary.example",
      localNodeIp: "192.0.2.20",
      ephemeralPrivateKeyWrapped: "wrapped-key",
      createdAt: new Date("2026-01-01T00:00:00Z"),
      updatedAt: new Date("2026-01-01T00:00:00Z"),
    };
    const acknowledgement: SnapshotAcknowledgement = {
      nodeId: "00000000-0000-4000-8000-000000000001",
      contentHash: "hash-47",
      snapshotVersion: 1,
      replicationVersion: 1,
      revisionId: "revision-47",
      leadershipGeneration: 3,
      appliedAt: new Date("2026-01-01T00:01:00Z"),
      receivedAt: new Date("2026-01-01T00:01:01Z"),
    };

    await store.withTransaction(async (tx) => {
      await tx.createEnrollmentAttempt(attempt);
      await tx.recordSnapshotAcknowledgement(acknowledgement);
    });

    await store.withTransaction(async (tx) => {
      expect(await tx.getEnrollmentAttempt("attempt-1")).toMatchObject({
        id: "attempt-1",
        state: "confirmed",
        primaryUrl: "https://primary.example",
      });
      expect(
        await tx.getSnapshotAcknowledgement(
          acknowledgement.nodeId,
          acknowledgement.contentHash,
        ),
      ).toMatchObject(acknowledgement);
    });
  });

  it("rejects an illegal state value before it can cross the port", async () => {
    const store = new InMemoryContinuityPersistence();
    await expect(
      store.withTransaction((tx) =>
        tx.createEnrollmentAttempt({
          id: "attempt-invalid",
          state: "not-a-state" as EnrollmentAttemptRecord["state"],
          primaryUrl: "https://primary.example",
          localNodeIp: "192.0.2.21",
          ephemeralPrivateKeyWrapped: "wrapped-key",
          createdAt: new Date("2026-01-01T00:00:00Z"),
          updatedAt: new Date("2026-01-01T00:00:00Z"),
        }),
      ),
    ).rejects.toThrow("invalid enrollment attempt state");
  });
});
