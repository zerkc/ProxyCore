import type { Database, SQLQueryBindings } from "bun:sqlite";
import type { AppliedSnapshotRecord, ContinuityTransactionPort } from "../ports.js";
import { encodeTimestamp } from "./timestamp.js";

// Canonical source order. Unknown properties never become SQL identifiers.
const fields = [
  ["id", "id"],
  ["sourcePrimaryId", "source_primary_id"],
  ["leadershipGeneration", "leadership_generation"],
  ["snapshotVersion", "snapshot_version"],
  ["replicationVersion", "replication_version"],
  ["contentHash", "content_hash"],
  ["snapshotBody", "snapshot_body"],
  ["revisionId", "revision_id"],
  ["status", "status"],
  ["applyJobId", "apply_job_id"],
  ["failureCode", "failure_code"],
  ["appliedAt", "applied_at"],
  ["discardedAt", "discarded_at"],
] as const satisfies readonly (readonly [keyof AppliedSnapshotRecord, string])[];

function encode(column: string, value: unknown): SQLQueryBindings {
  // Explicit SQL NULL bypasses even required encoders; native constraints decide.
  if (value === null) return null;
  if (["applied_at", "discarded_at"].includes(column)) {
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
    // Convert the represented integer exactly, including unsafe integer numbers.
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

/** Unwired canonical recordAppliedSnapshot on a caller-owned connection.
 * One bound uncached MAIN INSERT: no RETURNING, conflict action, readback, UUID,
 * clock or owned transaction. Undefined omits to DEFAULT except snapshotBody:
 * truthy bytes (including empty arrays) are copied; undefined binds explicit NULL.
 * Explicit null bypasses encoders. Omitted status receives native applied.
 * Required appliedAt has NO native default, unlike PostgreSQL defaultNow:
 * missing dates reject instead of receiving an invented clock. ISO validation
 * and every encoder run before prepare. Native text/UUID/status/FK checks
 * remain authoritative; this is not a PostgreSQL wire-coercion parity claim.
 * Private run/finalize errors retain identity, with finally's local error priority.
 * A finalizer failure can reject after durable autocommit. Caller cache, connection,
 * transactions, savepoints and commit/rollback ownership remain untouched.
 */
export async function createSQLiteAppliedSnapshotRecord(
  db: Database,
  snapshot: Parameters<ContinuityTransactionPort["recordAppliedSnapshot"]>[0],
): ReturnType<ContinuityTransactionPort["recordAppliedSnapshot"]> {
  const selected: [string, unknown][] = [];
  for (const [property, column] of fields) {
    const value = property === "snapshotBody"
      ? (snapshot.snapshotBody ? new Uint8Array(snapshot.snapshotBody) : null)
      : snapshot[property];
    if (value !== undefined) selected.push([column, value]);
  }
  const bindings = selected.map(([column, value]) => encode(column, value));
  const statement = db.prepare(
    selected.length
      ? `INSERT INTO main.applied_snapshots (${selected.map(([column]) => column).join(", ")})`
        + ` VALUES (${selected.map(() => "?").join(", ")})`
      : "INSERT INTO main.applied_snapshots DEFAULT VALUES",
  );
  try {
    statement.run(...bindings);
  } finally {
    statement.finalize();
  }
}
