import type { Database, SQLQueryBindings } from "bun:sqlite";
import type { RevisionRecord, RevisionStore } from "../ports.js";
import { decodeTimestamp, encodeTimestamp } from "./timestamp.js";
import { SQLiteWriter } from "./write.js";

const columns = ["id", "revision_number", "checksum", "snapshot", "actor_user_id",
  "created_at", "applied_at", "source", "source_primary_id", "source_node_id",
  "source_revision_id", "snapshot_content_hash", "snapshot_version",
  "replication_version", "leadership_generation"] as const;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

// The reader's validator/projector is private. This transaction-local duplicate
// deliberately preserves its public GENERAL date and unchecked JSON/number policy.
// No reader reread: AFTER triggers and settings changes must not replace RETURNING.
function project(raw: unknown): RevisionRecord {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)
    || Object.keys(raw).length !== columns.length
    || columns.some((key) => !Object.hasOwn(raw, key))) throw new Error("Invalid revision columns");
  const row = raw as Record<string, unknown>;
  function text(key: string): string {
    const value = row[key];
    if (typeof value !== "string" || value.includes("\0")) throw new Error(`Invalid TEXT ${key}`);
    return value;
  }
  function identifier(key: string): string {
    const value = text(key);
    if (value.length !== 36 || !uuid.test(value)) throw new Error(`Invalid stored UUID ${key}`);
    return value;
  }
  function optionalText(key: string, isID = false): string | undefined {
    return row[key] === null ? undefined : isID ? identifier(key) : text(key);
  }
  function integer(key: string, bits: 32 | 64): number {
    const value = row[key], limit = 1n << BigInt(bits - 1);
    if (typeof value !== "bigint" || value < -limit || value >= limit) throw new Error(`Invalid INTEGER ${key}`);
    return Number(value);
  }
  function optionalInteger(key: string, bits: 32 | 64): number | undefined {
    return row[key] === null ? undefined : integer(key, bits);
  }
  function date(key: string): Date {
    const value = decodeTimestamp(text(key));
    if (value.kind !== "finite") return new Date(NaN);
    const millis = value.seconds * 1000n + BigInt(Math.floor(value.nanos / 1_000_000));
    return millis < -8640000000000000n || millis > 8640000000000000n
      ? new Date(NaN) : new Date(Number(millis));
  }
  const source = text("source");
  if (source !== "ordinary" && source !== "import" && source !== "sync") throw new Error("Invalid source domain");
  return {
    id: identifier("id"),
    revisionNumber: integer("revision_number", 32),
    checksum: text("checksum"),
    snapshot: JSON.parse(text("snapshot")) as RevisionRecord["snapshot"],
    actorUserId: optionalText("actor_user_id", true),
    source,
    sourcePrimaryId: optionalText("source_primary_id", true),
    sourceNodeId: optionalText("source_node_id", true),
    sourceRevisionId: optionalText("source_revision_id", true),
    snapshotContentHash: optionalText("snapshot_content_hash"),
    snapshotVersion: optionalInteger("snapshot_version", 32),
    replicationVersion: optionalInteger("replication_version", 32),
    leadershipGeneration: optionalInteger("leadership_generation", 64),
    createdAt: date("created_at"),
    appliedAt: row.applied_at === null ? undefined : date("applied_at"),
  };
}

function rows(db: Database, sql: string, ...bindings: SQLQueryBindings[]): Record<string, unknown>[] {
  const statement = db.prepare<Record<string, unknown>, SQLQueryBindings[]>(sql);
  try { return statement.all(...bindings); }
  finally { statement.finalize(); }
}

/** Standalone, additive, unwired RevisionStore.markApplied.
 * Approved compatibility change: own BEGIN IMMEDIATE; reject caller transactions
 * and savepoints rather than PG's sequential partial writes/caller participation.
 * Capture optional default Date once. Undefined means default; null/non-Date are
 * unsupported runtime inputs and fail through toISOString before SQL mutation.
 * Invalid Date also fails before mutation. Input encoding follows the job writer,
 * not PG wire/parser equivalence. Private writer clock replaces settings' server
 * NOW defaults, independently of the supplied historical appliedAt; no parity claim.
 * MAIN, owned uncached drained/finalized statements, full pre-COMMIT projection,
 * and SQLiteWriter's real COMMIT mean no record escapes any rollback failure.
 */
export async function markAppliedSQLiteRevisionRecord(
  db: Database, id: string, appliedAt: Date = new Date(),
): ReturnType<RevisionStore["markApplied"]> {
  if (typeof id !== "string") throw new TypeError("Revision ID must be a string");
  const iso = appliedAt.toISOString();
  const millis = BigInt(new Date(iso).getTime());
  const remainder = ((millis % 1000n) + 1000n) % 1000n;
  const timestamp = encodeTimestamp({ kind: "finite", seconds: (millis - remainder) / 1000n,
    nanos: Number(remainder) * 1_000_000 })!;
  return new SQLiteWriter(db).write(({ timestamp: initializedAt }) => {
    const [revision] = rows(db, `UPDATE main.config_revisions SET applied_at=? WHERE id=?
      RETURNING ${columns.join(",")}`, timestamp, id);
    if (!revision) throw new Error(`Revision not found: ${id}`);
    const selectSettings = () => rows(db, "SELECT id FROM main.installation_settings WHERE id='default'");
    if (!selectSettings().length) {
      const created = rows(db, `INSERT INTO main.installation_settings
        (id,forwarding_rules,default_resolver_pool,created_at,updated_at)
        VALUES ('default','[]',NULL,?,?) ON CONFLICT DO NOTHING RETURNING id`, initializedAt, initializedAt);
      if (!created.length && !selectSettings().length) {
        throw new Error("Installation settings could not be initialized");
      }
    }
    rows(db, `UPDATE main.installation_settings SET current_applied_revision_id=?,updated_at=?
      WHERE id='default'`, id, timestamp);
    return project(revision);
  });
}
