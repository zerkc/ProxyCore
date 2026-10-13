import type { Database } from "bun:sqlite";
import type { JobStore } from "../ports.js";
import { recoverStaleSQLiteJobs } from "./job-recovery.js";

/** Additive, unwired public recovery using the canonical JobStore contract.
 * Preserve the source Date subtraction/TimeClip and JS coercion, not the native
 * safe-integer lease policy or its 120000ms default. An invalid cutoff is an
 * explicit SQLite boundary error, not PostgreSQL wire/error-class parity.
 * Native recovery completes its actual COMMIT before Number(count) is returned.
 * BUSY and transaction failures reject, never resolve zero. Recovery is not
 * safe apply resumption or protection against duplicated external processing.
 */
export async function recoverStaleSQLiteJobRecords(
  db: Database,
  leaseMs: Parameters<JobStore["recoverStale"]>[0],
  now: Parameters<JobStore["recoverStale"]>[1] = new Date(),
): ReturnType<JobStore["recoverStale"]> {
  const cutoff = new Date(now.getTime() - leaseMs);
  if (!Number.isFinite(cutoff.getTime())) {
    throw new Error("SQLite recovery cutoff must be a finite Date");
  }
  const millis = BigInt(cutoff.getTime());
  const remainder = ((millis % 1000n) + 1000n) % 1000n;
  const finite = { kind: "finite" as const, seconds: (millis - remainder) / 1000n,
    nanos: Number(remainder) * 1000000 };
  const count = recoverStaleSQLiteJobs(db, { leaseMs: 0, now: finite });
  return Number(count);
}
