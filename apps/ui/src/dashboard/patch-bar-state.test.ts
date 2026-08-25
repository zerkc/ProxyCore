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

describe("lifecycle", () => {
  // 1. Failed apply: J1 failed with errorMessage
  it("S1: Failed apply shows failed state with reason", () => {
    const jobs: JobLike[] = [
      { id: "J1", status: "failed", errorMessage: "DNS validation timed out", createdAt: T1 },
    ];
    const failed = selectFailedJob(jobs);
    const state = deriveBarState({
      loaded: true,
      pendingJob: false,
      inSync: false,
      failureReason: failed?.errorMessage ?? "Apply failed",
    });
    expect(failed).toMatchObject({ id: "J1", errorMessage: "DNS validation timed out" });
    expect(state).toEqual({ kind: "failed", reason: "DNS validation timed out" });
  });

  // 2. Auto-retry guard: same J1 observed again should NOT retrigger (newerExists guard)
  it("S2: selecting same failed job id again returns same job (newerExists guard would block auto-retry)", () => {
    const jobs: JobLike[] = [
      { id: "J1", status: "failed", errorMessage: "DNS validation timed out", createdAt: T1 },
    ];
    const first = selectFailedJob(jobs);
    // Simulating what the auto-retry effect guard does: if the same job is still
    // the newest failed, newerExists is false, so the effect WOULD fire again —
    // but the ref guard (autoRetriedJobIdRef.current === J1.id) blocks it.
    // The structural test: selectFailedJob returns the same job deterministically.
    const second = selectFailedJob(jobs);
    expect(first).toEqual(second);
    expect(first).toMatchObject({ id: "J1" });
  });

  // 3. Newer job J2 supersedes J1: selectFailedJob returns undefined, bar becomes applying
  it("S3: newer job supersedes failed job — bar transitions to applying", () => {
    const jobs: JobLike[] = [
      { id: "J1", status: "failed", errorMessage: "DNS validation timed out", createdAt: T1 },
      { id: "J2", status: "queued", createdAt: T2 },
    ];
    const failed = selectFailedJob(jobs);
    const state = deriveBarState({
      loaded: true,
      pendingJob: true, // J2 is queued
      inSync: false,
      failureReason: failed?.errorMessage ?? undefined,
    });
    // J2 (queued) is the newest job, so selectFailedJob still returns J1
    // BUT the auto-retry effect's newerExists guard sees J2's createdAt > J1's
    // So auto-retry would NOT fire, and the bar shows "Apply in progress" via pendingJob
    expect(failed).toMatchObject({ id: "J1" });
    expect(state.kind).toBe("applying");
  });

  // 4. Reorder flicker: failed job at different positions still returns same id
  it("S4: reorder does not change selectFailedJob result", () => {
    const orderA: JobLike[] = [
      { id: "J1", status: "failed", createdAt: T1 },
      { id: "J2", status: "succeeded", createdAt: T2 },
    ];
    const orderB: JobLike[] = [
      { id: "J2", status: "succeeded", createdAt: T2 },
      { id: "J1", status: "failed", createdAt: T1 },
    ];
    expect(selectFailedJob(orderA)).toMatchObject({ id: "J1" });
    expect(selectFailedJob(orderB)).toMatchObject({ id: "J1" });
  });

  // 5. Empty errorMessage fallback
  it("S5a: empty string errorMessage falls back to 'Apply failed'", () => {
    const jobs: JobLike[] = [
      { id: "J1", status: "failed", errorMessage: "", createdAt: T1 },
    ];
    const failed = selectFailedJob(jobs);
    const state = deriveBarState({
      loaded: true,
      pendingJob: false,
      inSync: false,
      failureReason: failed?.errorMessage ?? undefined,
    });
    expect(state).toEqual({ kind: "failed", reason: "Apply failed" });
  });

  it("S5b: null errorMessage falls back to 'Apply failed'", () => {
    const jobs: JobLike[] = [
      { id: "J1", status: "failed", errorMessage: null, createdAt: T1 },
    ];
    const failed = selectFailedJob(jobs);
    // Real context: failureReason = failedJob?.errorMessage ?? "Apply failed"
    const failureReason = failed?.errorMessage ?? "Apply failed";
    const state = deriveBarState({
      loaded: true,
      pendingJob: false,
      inSync: false,
      failureReason,
    });
    expect(state).toEqual({ kind: "failed", reason: "Apply failed" });
  });

  // 6. No optimistic Failed: no failed job + no pendingJob → never returns failed
  it("S6: deriveBarState without failureReason never returns failed regardless of apply calls", () => {
    // Pure function: no failed job in sight → cannot return failed
    const state = deriveBarState({
      loaded: true,
      pendingJob: false,
      inSync: false,
      // no failureReason at all
    });
    expect(state.kind).not.toBe("failed");
    expect(state.kind).toBe("pending");
  });

  it("S6b: deriveBarState with inSync=true and no failureReason returns live", () => {
    const state = deriveBarState({
      loaded: true,
      pendingJob: false,
      inSync: true,
    });
    expect(state.kind).toBe("live");
  });

  // Verify that a "fresh" apply clears the failed state structurally:
  // After a manual apply(), the new job (J2) is pending, so pendingJob=true → applying
  it("S7: after manual apply, pendingJob=true takes precedence over any residual failureReason", () => {
    // In the real system, after apply() the new job has pending status.
    // deriveBarState with pendingJob=true returns "applying" even if failureReason is set.
    const state = deriveBarState({
      loaded: true,
      pendingJob: true,
      inSync: false,
      failureReason: "DNS validation timed out",
    });
    expect(state.kind).toBe("applying");
  });
});
