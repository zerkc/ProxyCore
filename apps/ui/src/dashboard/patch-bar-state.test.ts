import { describe, expect, it } from "vitest";
import { selectFailedJob, deriveBarState, type JobLike } from "./patch-bar-state";

const T1 = "2024-01-01T10:00:00.000Z";
const T2 = "2024-01-01T11:00:00.000Z";
const T3 = "2024-01-01T12:00:00.000Z";

function job(overrides: Partial<JobLike> = {}): JobLike {
  return {
    id: "J0",
    status: "failed",
    createdAt: T1,
    ...overrides,
  };
}

describe("selectFailedJob", () => {
  it("returns undefined for undefined", () => {
    expect(selectFailedJob(undefined)).toBeUndefined();
  });

  it("returns undefined for empty array", () => {
    expect(selectFailedJob([])).toBeUndefined();
  });

  it("returns undefined when no job has status failed", () => {
    const jobs: JobLike[] = [
      { id: "J1", status: "queued", createdAt: T1 },
      { id: "J2", status: "succeeded", createdAt: T2 },
    ];
    expect(selectFailedJob(jobs)).toBeUndefined();
  });

  it("returns the single failed job", () => {
    const jobs = [job({ id: "J1", status: "failed", createdAt: T1 })];
    expect(selectFailedJob(jobs)).toMatchObject({ id: "J1" });
  });

  it("picks newest by createdAt desc", () => {
    const jobs: JobLike[] = [
      { id: "J1", status: "failed", createdAt: T1 },
      { id: "J2", status: "failed", createdAt: T2 },
    ];
    expect(selectFailedJob(jobs)).toMatchObject({ id: "J2" });
  });

  it("picks newest even when array order is reversed", () => {
    const jobs: JobLike[] = [
      { id: "J2", status: "failed", createdAt: T2 },
      { id: "J1", status: "failed", createdAt: T1 },
    ];
    expect(selectFailedJob(jobs)).toMatchObject({ id: "J2" });
  });

  it("tie-breaks by id desc on identical createdAt", () => {
    const jobs: JobLike[] = [
      { id: "J1", status: "failed", createdAt: T1 },
      { id: "J2", status: "failed", createdAt: T1 },
    ];
    const first = selectFailedJob(jobs);
    expect(first).toMatchObject({ id: "J2" });

    // Deterministic across multiple calls
    const second = selectFailedJob(jobs);
    expect(second).toMatchObject({ id: "J2" });
    expect(first).toEqual(second);
  });

  it("ignores jobs whose createdAt fails Date.parse", () => {
    const jobs: JobLike[] = [
      { id: "J1", status: "failed", createdAt: "not-a-date" },
      { id: "J2", status: "failed", createdAt: T1 },
    ];
    expect(selectFailedJob(jobs)).toMatchObject({ id: "J2" });
  });

  it("ignores non-failed jobs in mixed array", () => {
    const jobs: JobLike[] = [
      { id: "J1", status: "succeeded", createdAt: T3 },
      { id: "J2", status: "queued", createdAt: T2 },
      { id: "J3", status: "failed", createdAt: T1 },
    ];
    expect(selectFailedJob(jobs)).toMatchObject({ id: "J3" });
  });
});

describe("deriveBarState", () => {
  it("returns checking when loaded is false", () => {
    expect(deriveBarState({ loaded: false, pendingJob: false, inSync: true, failureReason: "anything" })).toEqual({ kind: "checking" });
    expect(deriveBarState({ loaded: false, pendingJob: true, inSync: false })).toEqual({ kind: "checking" });
  });

  it("returns applying when pendingJob is true", () => {
    expect(deriveBarState({ loaded: true, pendingJob: true, inSync: true })).toEqual({ kind: "applying" });
    expect(deriveBarState({ loaded: true, pendingJob: true, inSync: false, failureReason: "ignored" })).toEqual({ kind: "applying" });
  });

  it("returns failed when failureReason is set, even if inSync is true", () => {
    expect(deriveBarState({ loaded: true, pendingJob: false, inSync: true, failureReason: "DNS timeout" })).toEqual({ kind: "failed", reason: "DNS timeout" });
  });

  it("returns failed when failureReason is set, even if inSync is false", () => {
    expect(deriveBarState({ loaded: true, pendingJob: false, inSync: false, failureReason: "DNS timeout" })).toEqual({ kind: "failed", reason: "DNS timeout" });
  });

  it("returns live when inSync is true and no failure", () => {
    expect(deriveBarState({ loaded: true, pendingJob: false, inSync: true })).toEqual({ kind: "live" });
    expect(deriveBarState({ loaded: true, pendingJob: false, inSync: true, failureReason: undefined })).toEqual({ kind: "live" });
  });

  it("returns pending when inSync is false and no failure", () => {
    expect(deriveBarState({ loaded: true, pendingJob: false, inSync: false })).toEqual({ kind: "pending" });
  });

  it("failureReason takes precedence over both live and pending", () => {
    const withReason = deriveBarState({ loaded: true, pendingJob: false, inSync: true, failureReason: "x" });
    expect(withReason.kind).toBe("failed");
    const noReason = deriveBarState({ loaded: true, pendingJob: false, inSync: false });
    expect(noReason.kind).toBe("pending");
  });
});

describe("REQ-6: empty errorMessage fallback", () => {
  it("returns failed with Apply failed when failureReason is empty string", () => {
    const state = deriveBarState({ loaded: true, pendingJob: false, inSync: false, failureReason: "" });
    expect(state).toEqual({ kind: "failed", reason: "Apply failed" });
  });

  it("returns failed with Apply failed when failureReason is null", () => {
    const state = deriveBarState({ loaded: true, pendingJob: false, inSync: false, failureReason: null });
    expect(state).toEqual({ kind: "failed", reason: "Apply failed" });
  });

  it("returns pending when failureReason is omitted (undefined)", () => {
    const state = deriveBarState({ loaded: true, pendingJob: false, inSync: false });
    expect(state).toEqual({ kind: "pending" });
  });

  it("non-empty errorMessage is used verbatim", () => {
    const state = deriveBarState({
      loaded: true,
      pendingJob: false,
      inSync: true,
      failureReason: "DNS validation timed out",
    });
    expect(state).toEqual({ kind: "failed", reason: "DNS validation timed out" });
  });
});
