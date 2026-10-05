import type { Database, SQLQueryBindings } from "bun:sqlite";
import type { SnapshotAcknowledgement, ContinuityTransactionPort } from "../ports.js";
import { encodeTimestamp } from "./timestamp.js";

export type SQLiteSnapshotAcknowledgementSaveRecord = SnapshotAcknowledgement;

// Canonical allowlist and source order: the first two fields are conflict keys.
const fields = [
  ["nodeId", "node_id"],
  ["contentHash", "content_hash"],
  ["snapshotVersion", "snapshot_version"],
  ["replicationVersion", "replication_version"],
  ["revisionId", "revision_id"],
  ["leadershipGeneration", "leadership_generation"],
  ["appliedAt", "applied_at"],
  ["receivedAt", "received_at"],
] as const satisfies readonly (readonly [keyof SnapshotAcknowledgement, string])[];

function encode(column: string, value: unknown): SQLQueryBindings {
  // Explicit NULL bypasses every encoder; native constraints are authoritative.
  if (value === null) return null;
  if (column === "applied_at" || column === "received_at") {
    const iso = (value as Date).toISOString();
    const millis = BigInt(new Date(iso).getTime());
    const remainder = ((millis % 1000n) + 1000n) % 1000n;
    return encodeTimestamp({
      kind: "finite", seconds: (millis - remainder) / 1000n,
      nanos: Number(remainder) * 1_000_000,
    });
  }
  if (column === "leadership_generation") {
    if (typeof value !== "number" || !Number.isFinite(value) || !Number.isInteger(value)) {
      throw new Error("Invalid SQLite leadershipGeneration finite integer int64");
    }
    // Exact conversion of the represented integer, not a safe-integer restriction.
    const integer = BigInt(value);
    if (integer < -(1n << 63n) || integer >= (1n << 63n)) {
      throw new Error("Invalid SQLite leadershipGeneration signed int64 range");
    }
    return integer;
  }
  if (column === "snapshot_version" || column === "replication_version") {
    if (typeof value !== "number" || !Number.isInteger(value)
      || value < -2147483648 || value > 2147483647) {
      throw new Error(`Invalid SQLite ${column} int32`);
    }
    return value;
  }
  return value as SQLQueryBindings;
}

/** Unwired canonical recordSnapshotAcknowledgement on a caller-owned connection.
 * One bound MAIN UPSERT; exactly six nonkey fields may be updated. Undefined
 * INSERT fields omit to native DEFAULT, and undefined SET fields are excluded.
 * All eight native columns have NO defaults (including received_at, unlike PG):
 * missing required INSERT values reject even when the conflict row exists.
 * Empty SET rejects before any INSERT encoding, matching the lazy source builder.
 * No clock, UUID, RETURNING, readback, cache access or owned transaction. Literal
 * TEXT/UUID constraints are native boundaries, not PostgreSQL wire coercion parity.
 * Own only prepare/run/finalize; finally preserves local finalizer-error priority.
 * A finalizer can reject after durable autocommit. Caller transactions/savepoints,
 * connection/cache and later COMMIT failures remain entirely caller-owned.
 */
export async function saveSQLiteSnapshotAcknowledgementRecord(
  db: Database,
  ack: Parameters<ContinuityTransactionPort["recordSnapshotAcknowledgement"]>[0],
): ReturnType<ContinuityTransactionPort["recordSnapshotAcknowledgement"]> {
  const selected: [string, unknown][] = [];
  const updates: string[] = [];
  for (const [property, column] of fields) {
    const value = ack[property];
    if (value !== undefined) {
      selected.push([column, value]);
      if (column !== "node_id" && column !== "content_hash") updates.push(column);
    }
  }
  if (updates.length === 0) throw new Error("No values to set");
  // All INSERT encoders complete before preparation or native statement effects.
  const bindings = selected.map(([column, value]) => encode(column, value));
  const statement = db.prepare(
    `INSERT INTO main.node_snapshot_acks (${selected.map(([column]) => column).join(", ")})`
    + ` VALUES (${selected.map(() => "?").join(", ")})`
    + " ON CONFLICT(node_id, content_hash) DO UPDATE SET "
    + updates.map(column => `${column} = excluded.${column}`).join(", "),
  );
  try {
    statement.run(...bindings);
  } finally {
    statement.finalize();
  }
}
