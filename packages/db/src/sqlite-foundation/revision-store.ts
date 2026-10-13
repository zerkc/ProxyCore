import type { Database } from "bun:sqlite";
import type { RevisionStore } from "../ports.js";
import { createSQLiteRevisionRecord } from "./revision-create-record.js";
import { getSQLiteRevisionRecord } from "./revision-record.js";
import { latestSQLiteRevisionRecord } from "./revision-latest-record.js";
import { markAppliedSQLiteRevisionRecord } from "./revision-mark-applied-record.js";

/** Additive, unwired composition of the verified public revision operations.
 * Construction captures only the connection; callers own its lifetime.
 * Each primitive retains its transaction, default, projection and error policy:
 * create/markApplied own writes and reject caller transactions, while get/latest
 * read caller-pending state. No factory clock, cache, cloning or settings work.
 * This factory does not activate the SQLite runtime or change apply policy.
 */
export function createSQLiteRevisionStore(db: Database): RevisionStore {
  return {
    create(snapshot, actorUserId) {
      return createSQLiteRevisionRecord(db, snapshot, actorUserId);
    },
    async get(id) {
      // Preserve synchronous getter errors as rejections of the same object.
      return getSQLiteRevisionRecord(db, id);
    },
    latest() {
      return latestSQLiteRevisionRecord(db);
    },
    markApplied(id, appliedAt) {
      return markAppliedSQLiteRevisionRecord(db, id, appliedAt);
    },
  };
}
