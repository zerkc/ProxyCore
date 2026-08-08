import { assertDomain } from "./errors";
import type { JobStatus } from "./model";

const transitions: Record<JobStatus, readonly JobStatus[]> = {
  queued: ["validating"],
  validating: ["applying", "failed"],
  applying: ["applied", "failed", "rolled-back"],
  applied: [],
  failed: ["queued"],
  "rolled-back": ["queued"],
};

export function transitionJob(current: JobStatus, next: JobStatus): JobStatus {
  assertDomain(
    transitions[current].includes(next),
    `Invalid job transition: ${current} -> ${next}`,
    "JOB_TRANSITION",
  );
  return next;
}

export function isTerminalJobStatus(status: JobStatus): boolean {
  return status === "applied" || status === "failed" || status === "rolled-back";
}
