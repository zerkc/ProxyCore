import type { Database } from "bun:sqlite";
import type { JobStore } from "../ports.js";
import type { SQLiteJobRow } from "./job-read.js";
import { projectSQLiteJobRecord } from "./job-record.js";
import { decodeTimestamp } from "./timestamp.js";

const required = ["id", "revision_id", "correlation_id", "created_at", "target", "status", "source"];
const nullable = ["actor_user_id", "source_primary_id", "source_node_id", "source_revision_id",
  "snapshot_content_hash", "validation_output", "apply_output", "health_output", "error_message",
  "claimed_at", "started_at", "finished_at"];
const integers = ["snapshot_version", "replication_version", "leadership_generation"];
const columns = [...required, ...nullable, ...integers];
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

// The native reader's validator is private. This local returned-row boundary
// follows job-update-record without changing existing exports. UUID validation
// concerns canonical stored bytes, not the PostgreSQL UUID input parser.
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

/** Additive, unwired JobStore.list: no filtering, pagination or hidden cap.
 * Native TEXT COLLATE BINARY timestamps sort before lossy Date projection,
 * preserving nanos and infinities. Equal timestamps have unspecified order.
 * Own one uncached statement, drain all rows and finalize before projection.
 * Any prepare/all/finalizer/validation/projection error rejects the whole list;
 * only an actually empty result returns []. No ID rereads or reference queries.
 * Caller transactions/savepoints remain caller-owned. No writes or new clock.
 * Use safeIntegers native rows; public Number rounding is intentional.
 */
export async function listSQLiteJobRecords(db: Database): ReturnType<JobStore["list"]> {
  const statement = db.prepare<Record<string, unknown>, []>(
    `SELECT ${columns.join(", ")} FROM main.apply_jobs ORDER BY created_at DESC`,
  );
  let rows: Record<string, unknown>[];
  try {
    rows = statement.all();
  } finally {
    statement.finalize();
  }
  return rows.map((row) => projectSQLiteJobRecord(validate(row)));
}
