import type { Database } from "bun:sqlite";
import type { JobStore } from "../ports.js";
import { getSQLiteJobRecord } from "./job-record.js";
import { claimNextSQLiteJobRecord } from "./job-claim-record.js";
import { recoverStaleSQLiteJobRecords } from "./job-recovery-record.js";
import { updateSQLiteJobRecord } from "./job-update-record.js";
import { listSQLiteJobRecords } from "./job-list-record.js";
import { enqueueSQLiteJobRecord } from "./job-enqueue-record.js";
import { enqueueIfNotActiveSQLiteJobRecord } from "./job-enqueue-if-not-active-record.js";

/** Additive, unwired composition of the verified public SQLite operations.
 * Construction only captures the supplied connection; callers own its lifetime.
 * Each operation retains its own transaction, default, projection and error
 * policy. No factory transaction, clock, UUID, cache, reread or notification hook.
 * Claim projection remains after native COMMIT; this is not runtime activation
 * or a guarantee of safe apply resumption or notification delivery.
 */
export function createSQLiteJobStore(db: Database): JobStore {
  return {
    async get(id) {
      // The existing getter is synchronous; an async boundary preserves its
      // thrown error identity while satisfying the canonical Promise contract.
      return getSQLiteJobRecord(db, id);
    },
    claimNext(target) {
      return claimNextSQLiteJobRecord(db, target);
    },
    recoverStale(leaseMs, now) {
      return recoverStaleSQLiteJobRecords(db, leaseMs, now);
    },
    update(id, patch) {
      return updateSQLiteJobRecord(db, id, patch);
    },
    list() {
      return listSQLiteJobRecords(db);
    },
    enqueue(job) {
      return enqueueSQLiteJobRecord(db, job);
    },
    enqueueIfNotActive(job) {
      return enqueueIfNotActiveSQLiteJobRecord(db, job);
    },
  };
}
