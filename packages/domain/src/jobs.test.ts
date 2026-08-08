import { describe, expect, it } from "vitest";
import { transitionJob } from "./jobs";
import type { JobStatus } from "./model";

describe("apply job state machine", () => {
  it("allows the normal validation/apply/health path", () => {
    let status: JobStatus = "queued";
    status = transitionJob(status, "validating");
    status = transitionJob(status, "applying");
    status = transitionJob(status, "applied");

    expect(status).toBe("applied");
  });

  it("rejects jumping directly to applied", () => {
    expect(() => transitionJob("queued", "applied")).toThrow(/transition/i);
  });
});
