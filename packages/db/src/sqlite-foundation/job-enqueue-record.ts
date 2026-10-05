import type { Database, SQLQueryBindings } from "bun:sqlite";
import type { JobEnqueueInput, JobRecord, JobStore } from "../ports.js";
import type { SQLiteJobRow } from "./job-read.js";
import { projectSQLiteJobRecord } from "./job-record.js";
import { decodeTimestamp, encodeTimestamp } from "./timestamp.js";
import { SQLiteWriter, type WriteValue } from "./write.js";

// Explicit source-schema order. Undefined input uses schema defaults (all these
// optional columns default to SQL NULL); serializer undefined also becomes NULL.
const fields = [
  ["revisionId", "revision_id"], ["actorUserId", "actor_user_id"], ["target", "target"],
  ["status", "status"], ["source", "source"], ["sourcePrimaryId", "source_primary_id"],
  ["sourceNodeId", "source_node_id"], ["sourceRevisionId", "source_revision_id"],
  ["snapshotContentHash", "snapshot_content_hash"], ["snapshotVersion", "snapshot_version"],
  ["replicationVersion", "replication_version"], ["leadershipGeneration", "leadership_generation"],
  ["correlationId", "correlation_id"], ["validationOutput", "validation_output"],
  ["applyOutput", "apply_output"], ["healthOutput", "health_output"], ["errorMessage", "error_message"],
  ["claimedAt", "claimed_at"], ["startedAt", "started_at"], ["finishedAt", "finished_at"],
] as const;
const required = ["id", "revision_id", "correlation_id", "created_at", "target", "status", "source"];
const nullable = ["actor_user_id", "source_primary_id", "source_node_id", "source_revision_id",
  "snapshot_content_hash", "validation_output", "apply_output", "health_output", "error_message",
  "claimed_at", "started_at", "finished_at"];
const integers = ["snapshot_version", "replication_version", "leadership_generation"];
const columns = [...required, ...nullable, ...integers];
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

// Existing native validator is private; this small duplicate is a maintenance
// boundary, not a new exported codec. Validate RETURNING bytes without rereading.
function validate(value: unknown): SQLiteJobRow {
  if (value === null || typeof value !== "object" || Array.isArray(value)
    || Object.keys(value).length !== columns.length
    || columns.some((name) => !Object.hasOwn(value, name))) throw new Error("Invalid SQLite job columns");
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
    || !["ordinary", "import", "sync"].includes(row.source as string)) throw new Error("Invalid SQLite job domain");
  for (const name of integers) {
    const integer = row[name], bits = name === "leadership_generation" ? 63n : 31n;
    if (integer !== null && (typeof integer !== "bigint"
      || integer < -(1n << bits) || integer > (1n << bits) - 1n)) throw new Error(`Invalid job integer ${name}`);
  }
  return Object.freeze(row) as SQLiteJobRow;
}

function encode(column: string, value: unknown): SQLQueryBindings {
  if (value === null || value === undefined) return null;
  if (column.endsWith("_output")) return JSON.stringify(value) ?? null;
  // Bun maps NaN to SQL NULL, unlike PG integer input. Do not silently clear it.
  if (integers.includes(column) && typeof value === "number" && !Number.isFinite(value)) {
    throw new Error(`Invalid SQLite job integer input ${column}`);
  }
  if (column.endsWith("_at")) {
    // Invoke source Date encoder, including non-Date toISOString/InvalidDate
    // failures, before preparing SQL. No additional public runtime narrowing.
    const iso = (value as Date).toISOString();
    const millis = BigInt(new Date(iso).getTime());
    const remainder = ((millis % 1000n) + 1000n) % 1000n;
    return encodeTimestamp({ kind: "finite", seconds: (millis - remainder) / 1000n,
      nanos: Number(remainder) * 1_000_000 });
  }
  return value as SQLQueryBindings;
}

/** Internal dependency seam ONLY: no package export or runtime use.
 * Hook may perform synchronous transaction-local SQL on the supplied connection
 * only. No external signals, retained connection, deferred work or boundaries.
 * Purity is contractual: arbitrary callbacks' effects cannot be runtime-proven.
 * Missing hook is deliberately unwired, NOT delivery of real notifications.
 */
type InternalDependencies = {
  hook?: (db: Database, insertedId: string) => unknown;
  project?: (row: SQLiteJobRow) => JobRecord;
};
// Internal transaction-local insertion only; never opens a nested transaction.
export function createInternalSQLiteJobInserter(dependencies: InternalDependencies = {}) {
  const hook = dependencies.hook;
  if (hook && Object.prototype.toString.call(hook) === "[object AsyncFunction]") {
    throw new Error("Async enqueue hooks are unsupported");
  }
  return ({ db: connection, timestamp, newUUID }: WriteValue, job: JobEnqueueInput): JobRecord => {
    // Read properties in source construction order before encoding, rather
    // than interleaving getter access with serializer invocation.
    const insertedId = newUUID();
    const values = fields.map(([property]) => property === "status" ? job.status ?? "queued"
      : property === "source" ? job.source ?? "ordinary" : job[property]);
    const bindings = values.map((value, index) => encode(fields[index][1], value));
    const statement = connection.prepare<Record<string, unknown>, SQLQueryBindings[]>(
      `INSERT INTO main.apply_jobs (id,${fields.map(([, column]) => column).join(",")},created_at)`
      + ` VALUES (${Array(fields.length + 2).fill("?").join(",")}) RETURNING ${columns.join(",")}`,
    );
    let rows: Record<string, unknown>[];
    try { rows = statement.all(insertedId, ...bindings, timestamp); }
    finally { statement.finalize(); }
    if (rows.length !== 1) throw new Error("SQLite enqueue expected one returned row");
    if (hook) {
      const result = hook(connection, insertedId);
      if (result !== null && (typeof result === "object" || typeof result === "function")
        && typeof (result as { then?: unknown }).then === "function") {
        // Observe native Promise rejection without invoking arbitrary thenables.
        // This cannot cancel delayed work, which the internal contract forbids.
        if (result instanceof Promise) void result.catch(() => {});
        throw new Error("Async/thenable enqueue hooks are unsupported");
      }
    }
    return (dependencies.project ?? projectSQLiteJobRecord)(validate(rows[0]));
  };
}

export function createInternalSQLiteJobEnqueuer(dependencies: InternalDependencies = {}) {
  return async (db: Database, job: JobEnqueueInput): ReturnType<JobStore["enqueue"]> => {
    const insert = createInternalSQLiteJobInserter(dependencies);
    return new SQLiteWriter(db).write((value) => insert(value, job));
  };
}

/** Standalone, additive and unwired public JobStore.enqueue only.
 * Owned BEGIN IMMEDIATE rejects caller transactions, including savepoints.
 * INSERT fully drained/finalized, hook and projection precede real COMMIT; no
 * result escapes failed INSERT/hook/projection/COMMIT. Capture private timestamp
 * at owned transaction start (SQLite replacement for PG created_at default),
 * not a claim of identical PG server clock/error ordering. Literal SQLite UUID
 * text has no PG parser parity; Number(int64) rounding and fresh Date/JSON follow
 * shared projection. MAIN explicit columns resist TEMP shadowing. No cached
 * caller statement, reread, conditional enqueue, notifications or runtime wire.
 */
export async function enqueueSQLiteJobRecord(db: Database, job: JobEnqueueInput): Promise<JobRecord> {
  return createInternalSQLiteJobEnqueuer()(db, job);
}
