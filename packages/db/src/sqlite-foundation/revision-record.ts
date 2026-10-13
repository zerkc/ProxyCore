import type { Database } from "bun:sqlite";
import type { RevisionRecord } from "../ports.js";
import { decodeTimestamp } from "./timestamp.js";

export type SQLiteRevisionRecord = RevisionRecord;

const columns = ["id", "revision_number", "checksum", "snapshot", "actor_user_id",
  "created_at", "applied_at", "source", "source_primary_id", "source_node_id",
  "source_revision_id", "snapshot_content_hash", "snapshot_version",
  "replication_version", "leadership_generation"] as const;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

function text(row: Record<string, unknown>, key: string): string {
  const value = row[key];
  if (typeof value !== "string" || value.includes("\0")) throw new Error(`Invalid TEXT ${key}`);
  return value;
}
function identifier(row: Record<string, unknown>, key: string): string {
  const value = text(row, key);
  if (value.length !== 36 || !uuid.test(value)) throw new Error(`Invalid stored UUID ${key}`);
  return value;
}
function optionalText(row: Record<string, unknown>, key: string, isID = false): string | undefined {
  return row[key] === null ? undefined : isID ? identifier(row, key) : text(row, key);
}
function integer(row: Record<string, unknown>, key: string, bits: 32 | 64): number {
  const value = row[key], limit = 1n << BigInt(bits - 1);
  if (typeof value !== "bigint" || value < -limit || value >= limit) throw new Error(`Invalid INTEGER ${key}`);
  // Leadership intentionally retains toRevision's unchecked int64 Number rounding.
  return Number(value);
}
function optionalInteger(row: Record<string, unknown>, key: string, bits: 32 | 64): number | undefined {
  return row[key] === null ? undefined : integer(row, key, bits);
}
function date(key: string): Date {
  const value = decodeTimestamp(key);
  // General public-date policy: both tagged infinities become Invalid Date.
  // Native keys are untouched; malformed tags still throw in the codec.
  if (value.kind !== "finite") return new Date(NaN);
  const milliseconds = value.seconds * 1000n + BigInt(Math.floor(value.nanos / 1_000_000));
  if (milliseconds < -8640000000000000n || milliseconds > 8640000000000000n) return new Date(NaN);
  return new Date(Number(milliseconds));
}

/** Additive, unwired get projection. Own one uncached cursor; never own a transaction.
 * Lookup binds the literal ID, with no PostgreSQL UUID-input coercion claim.
 * The canonical snapshot type is compile-time only: generic JSON values remain
 * direct parsed values (including null); no schema checks or checksum rebuilding.
 * Plain mutable Dates and JSON are freshly detached on every successful read.
 */
export function getSQLiteRevisionRecord(db: Database, id: string): RevisionRecord | undefined {
  if (typeof id !== "string") throw new TypeError("Revision ID must be a string");
  const statement = db.prepare("SELECT * FROM main.config_revisions WHERE id = ?");
  try {
    const raw: unknown = statement.get(id);
    if (raw === null) return undefined;
    if (typeof raw !== "object" || raw === null || Array.isArray(raw)) throw new Error("Invalid revision row");
    const row = raw as Record<string, unknown>, keys = Object.keys(row);
    if (keys.length !== columns.length || columns.some((key) => !Object.hasOwn(row, key))) {
      throw new Error("Invalid revision columns");
    }
    const source = text(row, "source");
    if (source !== "ordinary" && source !== "import" && source !== "sync") throw new Error("Invalid source domain");
    return {
      id: identifier(row, "id"),
      revisionNumber: integer(row, "revision_number", 32),
      checksum: text(row, "checksum"),
      snapshot: JSON.parse(text(row, "snapshot")) as RevisionRecord["snapshot"],
      actorUserId: optionalText(row, "actor_user_id", true),
      source,
      sourcePrimaryId: optionalText(row, "source_primary_id", true),
      sourceNodeId: optionalText(row, "source_node_id", true),
      sourceRevisionId: optionalText(row, "source_revision_id", true),
      snapshotContentHash: optionalText(row, "snapshot_content_hash"),
      snapshotVersion: optionalInteger(row, "snapshot_version", 32),
      replicationVersion: optionalInteger(row, "replication_version", 32),
      leadershipGeneration: optionalInteger(row, "leadership_generation", 64),
      createdAt: date(text(row, "created_at")),
      appliedAt: row.applied_at === null ? undefined : date(text(row, "applied_at")),
    };
  } finally {
    statement.finalize();
  }
}
