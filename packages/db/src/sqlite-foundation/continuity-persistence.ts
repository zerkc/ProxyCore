import type { Database } from "bun:sqlite";
import type { ContinuityPersistencePort } from "../ports.js";
import { createSQLiteContinuityTransactionPort } from "./continuity-transaction-port.js";

const activeConnections = new WeakSet<Database>();

/** Unwired composition owning one explicit BEGIN IMMEDIATE / COMMIT / ROLLBACK
 * per call on the caller's single connection. Reentrant, concurrent and
 * caller-transaction calls are rejected (no savepoints). A callback error is
 * rethrown by identity even if ROLLBACK fails; a COMMIT failure surfaces after
 * a best-effort ROLLBACK. Callers own connection lifetime; no clock/UUID hooks.
 */
export function createSQLiteContinuityPersistence(db: Database): ContinuityPersistencePort {
  return {
    async withTransaction(work) {
      if (activeConnections.has(db) || db.inTransaction) {
        throw new Error("Nested continuity transaction");
      }
      activeConnections.add(db);
      try {
        db.exec("BEGIN IMMEDIATE");
        let result;
        try {
          result = await work(createSQLiteContinuityTransactionPort(db));
        } catch (error) {
          rollback(db);
          throw error;
        }
        try {
          db.exec("COMMIT");
        } catch (error) {
          rollback(db);
          throw error;
        }
        return result;
      } finally {
        activeConnections.delete(db);
      }
    },
  };
}

function rollback(db: Database) {
  try { db.exec("ROLLBACK"); } catch { /* the original failure wins */ }
}
