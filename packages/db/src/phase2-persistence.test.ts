import { describe, expect, it } from "vitest";
import {
  InMemoryContinuityPersistence,
  type EnrollmentAttemptRecord,
  type EnrollmentGrantRecord,
  type AppliedSnapshotRecord,
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
    const grant: EnrollmentGrantRecord = {
      attemptId: "attempt-1",
      tokenId: "token-1",
      installationId: "00000000-0000-4000-8000-000000000010",
      nodeId: "00000000-0000-4000-8000-000000000011",
      primaryId: "00000000-0000-4000-8000-000000000012",
      primaryGeneration: 3,
      nodeEphemeralPublicKey: '{"version":2,"publicKey":"key"}',
      verifiedPreviewDigest: "11".repeat(32),
      sealedBootstrapPayload: "sealed-envelope",
      payloadHash: "22".repeat(32),
      createdAt: new Date("2026-01-01T00:00:00Z"),
      expiresAt: new Date("2026-01-01T00:15:00Z"),
    };
    const publicationBody = new Uint8Array([0xde, 0xad, 0xbe, 0xef]);
    const appliedSnapshot: AppliedSnapshotRecord = {
      id: "snapshot-47",
      sourcePrimaryId: "00000000-0000-4000-8000-000000000012",
      leadershipGeneration: 3,
      snapshotVersion: 1,
      replicationVersion: 1,
      contentHash: "hash-47",
      snapshotBody: publicationBody,
      revisionId: "revision-47",
      status: "applied",
      applyJobId: "job-47",
      appliedAt: new Date("2026-01-01T00:01:00Z"),
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
      await tx.createEnrollmentGrant(grant);
      await tx.recordAppliedSnapshot(appliedSnapshot);
      await tx.recordSnapshotAcknowledgement(acknowledgement);
    });

    await store.withTransaction(async (tx) => {
      expect(await tx.getEnrollmentAttempt("attempt-1")).toMatchObject({
        id: "attempt-1",
        state: "confirmed",
        primaryUrl: "https://primary.example",
      });
      expect(await tx.getEnrollmentGrant("attempt-1")).toMatchObject(grant);
      const storedSnapshot = await tx.getAppliedSnapshot(appliedSnapshot.id);
      expect(storedSnapshot).toMatchObject(appliedSnapshot);
      expect(storedSnapshot?.snapshotBody).not.toBe(publicationBody);
      publicationBody[0] = 0;
      expect(storedSnapshot?.snapshotBody?.[0]).toBe(0xde);
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
