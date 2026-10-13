import type { Database } from "bun:sqlite";
import type { ContinuityTransactionPort } from "../ports.js";
import { getSQLiteNodeStateRecord } from "./node-state-record.js";
import { saveSQLiteNodeStateRecord } from "./node-state-save-record.js";
import { createSQLiteEnrollmentAttemptRecord } from "./enrollment-attempt-create-record.js";
import { getSQLiteEnrollmentAttemptRecord } from "./enrollment-attempt-read-record.js";
import { createSQLiteEnrollmentGrantRecord } from "./enrollment-grant-create-record.js";
import { getSQLiteEnrollmentGrantRecord } from "./enrollment-grant-read-record.js";
import { createSQLiteSyncAttemptRecord } from "./sync-attempt-create-record.js";
import { createSQLiteAppliedSnapshotRecord } from "./applied-snapshot-create-record.js";
import { getSQLiteAppliedSnapshotRecord } from "./applied-snapshot-read-record.js";
import { saveSQLiteSnapshotAcknowledgementRecord } from "./snapshot-acknowledgement-save-record.js";
import { getSQLiteSnapshotAcknowledgementRecord } from "./snapshot-acknowledgement-read-record.js";

/** Unwired composition: construction only captures the caller's connection.
 * All primitives are already async; direct returns preserve their Promise and
 * error identities, defaults and projection policies. Callers own transactions,
 * savepoints and connection lifetime. A finalizer rejection after a durable
 * autocommit is not a factory-level atomic rollback guarantee.
 */
export function createSQLiteContinuityTransactionPort(db: Database): ContinuityTransactionPort {
  return {
    getNodeState() {
      return getSQLiteNodeStateRecord(db);
    },
    saveNodeState(state) {
      return saveSQLiteNodeStateRecord(db, state);
    },
    createEnrollmentAttempt(attempt) {
      return createSQLiteEnrollmentAttemptRecord(db, attempt);
    },
    getEnrollmentAttempt(id) {
      return getSQLiteEnrollmentAttemptRecord(db, id);
    },
    createEnrollmentGrant(grant) {
      return createSQLiteEnrollmentGrantRecord(db, grant);
    },
    getEnrollmentGrant(id) {
      return getSQLiteEnrollmentGrantRecord(db, id);
    },
    recordSyncAttempt(attempt) {
      return createSQLiteSyncAttemptRecord(db, attempt);
    },
    recordAppliedSnapshot(snapshot) {
      return createSQLiteAppliedSnapshotRecord(db, snapshot);
    },
    getAppliedSnapshot(id) {
      return getSQLiteAppliedSnapshotRecord(db, id);
    },
    recordSnapshotAcknowledgement(ack) {
      return saveSQLiteSnapshotAcknowledgementRecord(db, ack);
    },
    getSnapshotAcknowledgement(nodeId, contentHash) {
      return getSQLiteSnapshotAcknowledgementRecord(db, nodeId, contentHash);
    },
  };
}
