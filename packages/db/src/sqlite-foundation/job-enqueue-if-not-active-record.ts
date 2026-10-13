import type { Database } from "bun:sqlite";
import type { JobEnqueueInput, JobRecord, JobStore } from "../ports.js";
import { createInternalSQLiteJobInserter } from "./job-enqueue-record.js";
import { SQLiteWriter } from "./write.js";

// Internal seams inherit T30's synchronous, transaction-local SQL-only hook
// contract. Not package exports, runtime wiring or notification delivery.
type InternalDependencies = Parameters<typeof createInternalSQLiteJobInserter>[0];
export function createInternalSQLiteConditionalJobEnqueuer(dependencies: InternalDependencies = {}) {
  return async (db: Database, job: JobEnqueueInput): ReturnType<JobStore["enqueueIfNotActive"]> => {
    return new SQLiteWriter(db).write((value) => {
      // Own an uncached MAIN statement. Finalize before either branch, even if
      // stepping fails; guard errors reject rather than masquerading as empty.
      const guard = value.db.prepare<{ id: string }, []>(
        "SELECT id FROM main.apply_jobs WHERE status IN ('queued','validating','applying') LIMIT 1",
      );
      let blocked: { id: string } | null;
      try { blocked = guard.get(); }
      finally { guard.finalize(); }
      if (blocked) return undefined;
      // No input property access, encoding, UUID, INSERT, hook or projection
      // before the guard. Reuse ONLY the transaction-local ordinary insertion,
      // never the standalone ordinary enqueuer in an already-open transaction.
      return createInternalSQLiteJobInserter(dependencies)(value, job);
    });
  };
}

/** Standalone additive JobStore.enqueueIfNotActive, deliberately unwired.
 * GLOBAL active existence across all targets/revisions; no priority or target
 * filtering. Guard and insert share owned BEGIN IMMEDIATE; caller transactions
 * and savepoints are rejected unchanged. SQLite's broad writer lock is not PG
 * advisory-lock/SKIP LOCKED wire equivalence or external claim coordination.
 * The existing private SQLiteWriter clock runs BEFORE the guard by explicit
 * user approval; even blocked calls capture it, but do not generate a UUID.
 * A blocked undefined or inserted record is published only after own COMMIT
 * succeeds. RETURNING finalization, synchronous hook and projection all precede
 * COMMIT. Failures roll back/reject, including BUSY and blocked-path COMMIT.
 */
export async function enqueueIfNotActiveSQLiteJobRecord(
  db: Database, job: JobEnqueueInput,
): Promise<JobRecord | undefined> {
  return createInternalSQLiteConditionalJobEnqueuer()(db, job);
}
