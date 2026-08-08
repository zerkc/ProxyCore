import { describe, expect, it } from "vitest";
import {
  selectRetentionDeletions,
  summarizeHealth,
  type RetentionArtifact,
} from "./operations";

describe("worker operations", () => {
  it("cleans old/oversized artifacts without deleting live state", () => {
    const now = new Date("2026-08-08T00:00:00Z");
    const artifacts: RetentionArtifact[] = [
      { id: "old", sizeBytes: 60, createdAt: new Date("2026-07-01"), kind: "log" },
      { id: "new", sizeBytes: 60, createdAt: new Date("2026-08-07"), kind: "log" },
      {
        id: "current",
        sizeBytes: 100,
        createdAt: new Date("2026-07-01"),
        kind: "rendered",
        current: true,
      },
      {
        id: "cert",
        sizeBytes: 100,
        createdAt: new Date("2026-07-01"),
        kind: "certificate",
        activeCertificate: true,
      },
    ];

    expect(selectRetentionDeletions(artifacts, { maxAgeDays: 7, maxSizeBytes: 150 }, now)).toEqual([
      "old",
      "new",
    ]);
  });

  it("summarizes distinct component health states", () => {
    const summary = summarizeHealth([
      { component: "app", status: "healthy", details: {} },
      { component: "worker", status: "degraded", details: { queued: 1 } },
      { component: "coredns", status: "healthy", details: {} },
    ]);

    expect(summary.overall).toBe("degraded");
    expect(summary.components.worker?.status).toBe("degraded");
  });
});
