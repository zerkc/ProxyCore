import type { Database, SQLQueryBindings } from "bun:sqlite";
import type { NodeStateRecord, ContinuityTransactionPort } from "../ports.js";
import { encodeTimestamp } from "./timestamp.js";

export type SQLiteNodeStateSaveRecord = NodeStateRecord;

// Fixed source construction order; neither clause can access unknown properties.
const fields = [
  ["enrolledAt", "enrolled_at"],
  ["enrollmentPrimaryId", "enrollment_primary_id"],
  ["lastSeenAt", "last_seen_at"],
  ["lastAppliedSnapshotId", "last_applied_snapshot_id"],
  ["enrollmentAttemptId", "enrollment_attempt_id"],
  ["primaryUrl", "primary_url"],
  ["primaryInstallationId", "primary_installation_id"],
  ["primaryTlsSpkiSha256", "primary_tls_spki_sha256"],
  ["credentialId", "credential_id"],
  ["syncEnabled", "sync_enabled"],
  ["lastAttemptAt", "last_attempt_at"],
  ["lastSuccessAt", "last_success_at"],
  ["consecutiveFailures", "consecutive_failures"],
  ["nextAttemptAt", "next_attempt_at"],
  ["lastErrorCode", "last_error_code"],
  ["updatedAt", "updated_at"],
] as const;

function encode(column: string, value: unknown): SQLQueryBindings {
  // Explicit null bypasses encoders and reaches the native constraints.
  if (value === null) return null;
  if (column.endsWith("_at")) {
    const iso = (value as Date).toISOString();
    const millis = BigInt(new Date(iso).getTime());
    const remainder = ((millis % 1000n) + 1000n) % 1000n;
    return encodeTimestamp({ kind: "finite", seconds: (millis - remainder) / 1000n,
      nanos: Number(remainder) * 1_000_000 });
  }
  if (column === "sync_enabled") {
    if (typeof value !== "boolean") throw new Error("Invalid SQLite syncEnabled boolean");
    return value ? 1 : 0;
  }
  if (column === "consecutive_failures") {
    if (typeof value !== "number" || !Number.isInteger(value)
      || value < -2147483648 || value > 2147483647) {
      throw new Error("Invalid SQLite consecutiveFailures int32");
    }
    return value;
  }
  return value as SQLQueryBindings;
}

/** Unwired canonical saveNodeState, on the caller's connection and provided ID.
 * One MAIN UPSERT, no clock, readback, RETURNING, cached cursor or owned transaction.
 * Undefined INSERT fields use native defaults; undefined conflict fields preserve.
 * Null bypasses encoders. Boolean/int32 input boundaries are not PG wire coercions.
 * Success is void after execution/finalization, pending inside caller transactions.
 * A later caller COMMIT failure remains caller-owned. A finalizer error after a
 * successful autocommit may reject despite a durable write: no rollback promise.
 */
export async function saveSQLiteNodeStateRecord(
  db: Database,
  state: Parameters<ContinuityTransactionPort["saveNodeState"]>[0],
): ReturnType<ContinuityTransactionPort["saveNodeState"]> {
  const selected: [string, unknown][] = [];
  for (const [property, column] of fields) {
    const value = state[property];
    if (value !== undefined) selected.push([column, value]);
  }
  if (selected.length === 0) throw new Error("No values to set");
  // Bind ID explicitly: missing ID becomes SQL NULL rather than selecting default.
  const bindings: SQLQueryBindings[] = [state.id ?? null,
    ...selected.map(([column, value]) => encode(column, value))];
  const columns = ["id", ...selected.map(([column]) => column)];
  const statement = db.prepare(
    `INSERT INTO main.node_state (${columns.join(", ")})`
    + ` VALUES (${columns.map(() => "?").join(", ")}) ON CONFLICT(id) DO UPDATE SET `
    + selected.map(([column]) => `${column} = excluded.${column}`).join(", "),
  );
  try {
    statement.run(...bindings);
  } finally {
    statement.finalize();
  }
}
