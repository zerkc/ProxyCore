import { Database, type SQLQueryBindings } from "bun:sqlite";
import { decodeTimestamp, type Timestamp } from "./timestamp.js";
import { SQLiteWriter } from "./write.js";

// Exact ports.ts JobTarget domain; importing ports pulls unresolved workspace
// aliases into standalone private-module checks. This is not a runtime adapter.
type JobTarget = "coredns" | "nginx" | "combined" | "certificate";
type RequiredText = "id" | "revision_id" | "correlation_id" | "created_at";
type NullableText = "actor_user_id" | "source_primary_id" | "source_node_id"
  | "source_revision_id" | "snapshot_content_hash" | "validation_output"
  | "apply_output" | "health_output" | "error_message" | "claimed_at"
  | "started_at" | "finished_at";

/** Native SQLite values: SQL NULL and raw JSON TEXT are deliberately not projected.
 * Timestamp TEXT uses the validated sortable tagged encoding, not Date.
 * All INTEGER columns remain bigint, including signed int64 leadership values.
 */
export type SQLiteClaimedJobRow = Readonly<Record<RequiredText, string>
  & Record<NullableText, string | null> & {
    target: JobTarget;
    status: "validating";
    source: "ordinary" | "import" | "sync";
    snapshot_version: bigint | null;
    replication_version: bigint | null;
    leadership_generation: bigint | null;
  }>;
export type SQLiteClaimOptions = Readonly<{
  target?: JobTarget;
  clock?: () => Extract<Timestamp, { kind: "finite" }>;
  signal?: AbortSignal;
}>;

const targets = ["coredns", "nginx", "combined", "certificate"];
const required = ["id", "revision_id", "correlation_id", "created_at", "target", "status", "source"];
const nullable = ["actor_user_id", "source_primary_id", "source_node_id", "source_revision_id",
  "snapshot_content_hash", "validation_output", "apply_output", "health_output", "error_message",
  "claimed_at", "started_at", "finished_at"];
const integers = ["snapshot_version", "replication_version", "leadership_generation"];
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

function validate(row: Record<string, unknown>, status: "queued" | "validating"): void {
  if (Object.keys(row).length !== required.length + nullable.length + integers.length) {
    throw new Error("Invalid SQLite job columns");
  }
  for (const name of [...required, ...nullable]) {
    const value = row[name];
    if (nullable.includes(name) && value === null) continue;
    if (typeof value !== "string" || value.includes("\0")) throw new Error(`Invalid job ${name}`);
    if ((name === "id" || name.endsWith("_id") && name !== "correlation_id") && !uuid.test(value)) {
      throw new Error(`Invalid job UUID ${name}`);
    }
    if (name.endsWith("_at")) decodeTimestamp(value);
    if (name.endsWith("_output")) JSON.parse(value); // Validate only; keep original bytes.
  }
  if (row.status !== status || !targets.includes(row.target as string)
    || !["ordinary", "import", "sync"].includes(row.source as string)) {
    throw new Error("Invalid SQLite job domain");
  }
  for (const name of integers) {
    const value = row[name];
    const min = name === "leadership_generation" ? -(1n << 63n) : -(1n << 31n);
    const max = name === "leadership_generation" ? (1n << 63n) - 1n : (1n << 31n) - 1n;
    if (value !== null && (typeof value !== "bigint" || value < min || value > max)) {
      throw new Error(`Invalid job integer ${name}`);
    }
  }
}

// Own uncached statements: never finalize a caller's db.query() cached cursor.
function get(db: Database, sql: string, bindings: SQLQueryBindings[] = []): Record<string, unknown> | null {
  const statement = db.prepare<Record<string, unknown>, SQLQueryBindings[]>(sql);
  try {
    return statement.get(...bindings);
  } finally {
    statement.finalize();
  }
}

function references(db: Database, row: Record<string, unknown>): void {
  // Defensive selected-row boundary, not PostgreSQL wire-error equivalence.
  if (!get(db, "SELECT id FROM main.config_revisions WHERE id = ?", [row.revision_id as string])
    || row.actor_user_id !== null
      && !get(db, "SELECT id FROM main.users WHERE id = ?", [row.actor_user_id as string])) {
    throw new Error("Selected SQLite job has a missing foreign-key reference");
  }
}

/** Private, unwired ownership primitive; use the foundation safeIntegers connection.
 * Unexpected numeric INTEGER projections fail validation rather than lose int64.
 * One BEGIN IMMEDIATE serializes all writers,
 * broader than PG's revision advisory lock; this is NOT SKIP LOCKED. BUSY propagates.
 * Abort is checked at synchronous boundaries. Bun's synchronous busy wait cannot
 * be immediately interrupted by AbortSignal; callers must bound busy_timeout.
 * The return is published only after SQLiteWriter's actual COMMIT succeeds.
 */
export function claimNextSQLiteJob(db: Database, options: SQLiteClaimOptions = {}): SQLiteClaimedJobRow | undefined {
  options.signal?.throwIfAborted();
  if (options.target !== undefined && !targets.includes(options.target)) throw new Error("Invalid job target");
  // Clock callbacks are synchronous too. Contain abandoned rejections locally,
  // without awaiting processing or installing production global handlers.
  const clock = options.clock === undefined ? undefined : () => {
    const value = options.clock!();
    const then = value !== null && (typeof value === "object" || typeof value === "function")
      ? (value as unknown as { then?: unknown }).then : undefined;
    if (typeof then === "function") {
      new Promise((resolve, reject) => then.call(value, resolve, reject)).catch(() => {});
      throw new Error("Async claim clocks are unsupported");
    }
    return value;
  };
  return new SQLiteWriter(db, clock).write(({ timestamp }) => {
    options.signal?.throwIfAborted();
    const selected = get(db, "SELECT * FROM main.apply_jobs WHERE status = 'queued'"
      + (options.target === undefined ? "" : " AND target = ?") + " ORDER BY created_at ASC LIMIT 1",
    options.target === undefined ? [] : [options.target]);
    if (!selected) {
      options.signal?.throwIfAborted();
      return undefined;
    }
    validate(selected, "queued");
    references(db, selected);
    options.signal?.throwIfAborted();
    const claimed = get(db, "UPDATE main.apply_jobs SET status = 'validating', claimed_at = ?,"
      + " started_at = COALESCE(started_at, ?) WHERE id = ? AND status = 'queued' RETURNING *",
    [timestamp, timestamp, selected.id as string]);
    if (!claimed) throw new Error("SQLite queued claim lost its selected row");
    validate(claimed, "validating");
    references(db, claimed);
    options.signal?.throwIfAborted();
    return Object.freeze(claimed) as SQLiteClaimedJobRow;
  });
}
