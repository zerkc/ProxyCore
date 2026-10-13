import type { Database, SQLQueryBindings } from "bun:sqlite";
import { createSnapshot, checksumSnapshot, type ConfigurationSnapshot } from "../../../domain/src/snapshot.js";
import type { RevisionRecord, RevisionStore } from "../ports.js";
import { decodeTimestamp } from "./timestamp.js";
import { SQLiteWriter } from "./write.js";

const columns = ["id", "revision_number", "checksum", "snapshot", "actor_user_id",
  "created_at", "applied_at", "source", "source_primary_id", "source_node_id",
  "source_revision_id", "snapshot_content_hash", "snapshot_version",
  "replication_version", "leadership_generation"] as const;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

// Existing readers keep their projector private. This transaction-local duplicate
// preserves GENERAL public dates, nullable own keys, unchecked leadership Number
// rounding and generic stored JSON. Neither snapshots nor checksums are revalidated.
// Project captured rows before COMMIT: never reread an AFTER-trigger replacement.
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

function nextNumber(raw: Record<string, unknown> | undefined): bigint {
  if (raw === undefined) return 1n;
  const value = raw.revision_number;
  if (Object.keys(raw).length !== 1 || !Object.hasOwn(raw, "revision_number")
    || typeof value !== "bigint" || value < -2147483648n || value > 2147483647n) {
    throw new Error("Invalid INTEGER revision_number");
  }
  if (value === 2147483647n) throw new Error("Revision number overflow");
  return value + 1n;
}

/** Standalone, additive and unwired canonical RevisionStore.create.
 * Normalize THEN checksum using the live domain helpers before any writer/SQL,
 * including caller-transaction rejection. JSON.stringify stores normalized JSON;
 * it is deliberately not the checksum serialization algorithm.
 * Approved PG compatibility change: one owned BEGIN IMMEDIATE serializes checksum
 * reuse and native max+1 allocation. Caller BEGIN/savepoints are rejected intact.
 * The broad SQLite writer lock is not PG advisory-lock/wire-error equivalence.
 * Capture the private native writer clock explicitly (DDL has no time default),
 * not a new public clock option. No settings, pointer, factory or runtime wiring.
 * Drain/finalize every owned statement and project before real COMMIT, publishing
 * neither existing nor inserted records if COMMIT or any earlier stage fails.
 */
export async function createSQLiteRevisionRecord(
  db: Database, snapshot: ConfigurationSnapshot, actorUserId?: string,
): ReturnType<RevisionStore["create"]> {
  const normalized = createSnapshot(snapshot);
  const checksum = checksumSnapshot(normalized);
  return new SQLiteWriter(db).write(({ timestamp, newUUID }) => {
    const [existing] = rows(db, `SELECT ${columns.join(",")} FROM main.config_revisions
      WHERE checksum=? LIMIT 1`, checksum);
    if (existing) return project(existing);

    const [latest] = rows(db, `SELECT revision_number FROM main.config_revisions
      ORDER BY revision_number DESC LIMIT 1`);
    const revisionNumber = nextNumber(latest);
    const [inserted] = rows(db, `INSERT INTO main.config_revisions
      (id,revision_number,checksum,snapshot,actor_user_id,created_at,applied_at,source)
      VALUES (?,?,?,?,?,?,NULL,'ordinary') RETURNING ${columns.join(",")}`,
    newUUID(), revisionNumber, checksum, JSON.stringify(normalized), actorUserId ?? null, timestamp);
    if (!inserted) throw new Error("Revision insert returned no row");
    return project(inserted);
  });
}
