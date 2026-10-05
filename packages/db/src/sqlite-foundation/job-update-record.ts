import type { Database, SQLQueryBindings } from "bun:sqlite";
import type { JobStore } from "../ports.js";
import type { SQLiteJobRow } from "./job-read.js";
import { projectSQLiteJobRecord } from "./job-record.js";
import { decodeTimestamp, encodeTimestamp } from "./timestamp.js";

// Selection and encoding follow applyJobs schema order, not patch insertion order.
const assignments = [
  ["status", "status"],
  ["validationOutput", "validation_output"],
  ["applyOutput", "apply_output"],
  ["healthOutput", "health_output"],
  ["errorMessage", "error_message"],
  ["claimedAt", "claimed_at"],
  ["startedAt", "started_at"],
  ["finishedAt", "finished_at"],
] as const;
const required = ["id", "revision_id", "correlation_id", "created_at", "target", "status", "source"];
const nullable = ["actor_user_id", "source_primary_id", "source_node_id", "source_revision_id",
  "snapshot_content_hash", "validation_output", "apply_output", "health_output", "error_message",
  "claimed_at", "started_at", "finished_at"];
const integers = ["snapshot_version", "replication_version", "leadership_generation"];
const columns = [...required, ...nullable, ...integers];
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

// The native reader's validator is private. Keep this local returned-row boundary
// without exporting or modifying the reader. Reuse the public timestamp codec.
function validate(value: unknown): SQLiteJobRow {
  if (value === null || typeof value !== "object" || Array.isArray(value)
    || Object.keys(value).length !== columns.length
    || columns.some((name) => !Object.hasOwn(value, name))) {
    throw new Error("Invalid SQLite job columns");
  }
  const row = { ...value } as Record<string, unknown>;
  for (const name of [...required, ...nullable]) {
    const text = row[name];
    if (nullable.includes(name) && text === null) continue;
    if (typeof text !== "string" || text.includes("\0")) throw new Error(`Invalid job ${name}`);
    if ((name === "id" || name.endsWith("_id") && name !== "correlation_id") && !uuid.test(text)) {
      throw new Error(`Invalid job UUID ${name}`);
    }
    if (name.endsWith("_at")) decodeTimestamp(text);
    if (name.endsWith("_output")) JSON.parse(text);
  }
  if (!["coredns", "nginx", "combined", "certificate"].includes(row.target as string)
    || !["queued", "validating", "applying", "applied", "failed", "rolled-back"].includes(row.status as string)
    || !["ordinary", "import", "sync"].includes(row.source as string)) {
    throw new Error("Invalid SQLite job domain");
  }
  for (const name of integers) {
    const integer = row[name];
    const bits = name === "leadership_generation" ? 63n : 31n;
    if (integer !== null && (typeof integer !== "bigint"
      || integer < -(1n << bits) || integer > (1n << bits) - 1n)) {
      throw new Error(`Invalid job integer ${name}`);
    }
  }
  return Object.freeze(row) as SQLiteJobRow;
}

function encode(column: string, value: unknown): SQLQueryBindings {
  // Explicit null bypasses Drizzle encoders; nullable columns clear and status
  // null reaches SQLite's NOT NULL constraint. Serializer undefined maps to SQL
  // NULL as in pg prepareValue, but an undefined patch field is never selected.
  if (value === null) return null;
  if (column.endsWith("_output")) return JSON.stringify(value) ?? null;
  if (column.endsWith("_at")) {
    // Invoke the evidenced encoder, including its failure on non-Date values.
    // No instanceof/broad runtime gate: out-of-type toISOString calls are allowed.
    const iso = (value as Date).toISOString();
    const millis = BigInt(new Date(iso).getTime());
    const remainder = ((millis % 1000n) + 1000n) % 1000n;
    return encodeTimestamp({ kind: "finite", seconds: (millis - remainder) / 1000n,
      nanos: Number(remainder) * 1_000_000 });
  }
  return value as SQLQueryBindings;
}

/** Additive, unwired JobStore.update projection, not a runtime adapter.
 * Literal bound SQLite TEXT ID: no PostgreSQL UUID parser/collation parity.
 * Honor only the selected eight properties (including inherited values). No
 * clock, lifecycle/ownership checks, metadata autofill or reference rereads.
 * Own exactly one uncached UPDATE RETURNING statement. all() drains SQLITE_DONE
 * including implicit COMMIT failure, then finalize before validating/projecting.
 * Caller transactions/savepoints are allowed and remain caller-owned/pending.
 * Autocommitted mutation cannot be rolled back by subsequent projection failure.
 * Safe-integer native rows retain int64; public Number rounding is intentional.
 */
export async function updateSQLiteJobRecord(
  db: Database,
  id: Parameters<JobStore["update"]>[0],
  patch: Parameters<JobStore["update"]>[1],
): ReturnType<JobStore["update"]> {
  const selected: [string, unknown][] = [];
  for (const [property, column] of assignments) {
    // Ordinary property access mirrors PG's conditional access and assignment.
    if (patch[property] !== undefined) selected.push([column, patch[property]]);
  }
  if (selected.length === 0) throw new Error("No values to set");
  const bindings = selected.map(([column, value]) => encode(column, value));
  const statement = db.prepare<Record<string, unknown>, SQLQueryBindings[]>(
    `UPDATE main.apply_jobs SET ${selected.map(([column]) => `${column} = ?`).join(", ")}`
    + ` WHERE id = ? RETURNING ${columns.join(", ")}`,
  );
  let rows: Record<string, unknown>[];
  try {
    rows = statement.all(...bindings, id);
  } finally {
    statement.finalize();
  }
  if (rows.length === 0) throw new Error(`Job not found: ${id}`);
  return projectSQLiteJobRecord(validate(rows[0]));
}
