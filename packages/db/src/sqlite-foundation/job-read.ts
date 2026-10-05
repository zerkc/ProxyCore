import { Database } from "bun:sqlite";
import { decodeTimestamp } from "./timestamp.js";

type RequiredText = "id" | "revision_id" | "correlation_id" | "created_at";
type NullableText = "actor_user_id" | "source_primary_id" | "source_node_id"
  | "source_revision_id" | "snapshot_content_hash" | "validation_output"
  | "apply_output" | "health_output" | "error_message" | "claimed_at"
  | "started_at" | "finished_at";

/** Detached native row, not a JobRecord projection. Preserve SQL NULL, raw JSON
 * TEXT, tagged timestamp bytes and exact INTEGER bigints, including int64.
 */
export type SQLiteJobRow = Readonly<Record<RequiredText, string>
  & Record<NullableText, string | null> & {
    target: "coredns" | "nginx" | "combined" | "certificate";
    status: "queued" | "validating" | "applying" | "applied" | "failed" | "rolled-back";
    source: "ordinary" | "import" | "sync";
    snapshot_version: bigint | null;
    replication_version: bigint | null;
    leadership_generation: bigint | null;
  }>;

const required = ["id", "revision_id", "correlation_id", "created_at", "target", "status", "source"];
const nullable = ["actor_user_id", "source_primary_id", "source_node_id", "source_revision_id",
  "snapshot_content_hash", "validation_output", "apply_output", "health_output", "error_message",
  "claimed_at", "started_at", "finished_at"];
const integers = ["snapshot_version", "replication_version", "leadership_generation"];
const columns = [...required, ...nullable, ...integers];
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

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
    if (name.endsWith("_output")) JSON.parse(text); // Validate without replacing raw TEXT.
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

/** Private and unwired. Literal SQLite TEXT lookup: no UUID input normalization
 * or PG grammar/collation parity. Own one uncached statement; never finalize a
 * caller's db.query cache. No transaction ownership or reference lookups: the
 * selected row alone is validated, and failures propagate rather than look absent.
 * Use a safeIntegers connection; numeric INTEGER projections are rejected.
 */
export function getSQLiteJobRow(db: Database, id: string): SQLiteJobRow | undefined {
  if (typeof id !== "string") throw new TypeError("Job ID must be a literal string");
  const statement = db.prepare<Record<string, unknown>, [string]>(
    "SELECT * FROM main.apply_jobs WHERE id = ?",
  );
  try {
    const row = statement.get(id);
    return row === null ? undefined : validate(row);
  } finally {
    statement.finalize();
  }
}
