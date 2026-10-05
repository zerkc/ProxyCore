import type { Database } from "bun:sqlite";
import type { JobStore } from "../ports.js";
import { claimNextSQLiteJob } from "./job-claim.js";
import { projectSQLiteJobRecord } from "./job-record.js";

/** Additive, unwired public claim projection with the canonical JobStore contract.
 * Native BEGIN IMMEDIATE/validation/update/actual COMMIT completes before this
 * mapper runs. Unlike PG's toJob inside its transaction callback, an unexpected
 * projection exception rejects AFTER committed ownership and cannot roll back.
 * Project the exact returned row once, never reread it. BUSY and native failures
 * reject; only a successful native empty claim resolves undefined. No public
 * clock/signal options, source priority, tie guarantee, or active-job exclusion.
 */
export async function claimNextSQLiteJobRecord(
  db: Database,
  target?: Parameters<JobStore["claimNext"]>[0],
): ReturnType<JobStore["claimNext"]> {
  const row = claimNextSQLiteJob(db, { target });
  return row === undefined ? undefined : projectSQLiteJobRecord(row);
}
