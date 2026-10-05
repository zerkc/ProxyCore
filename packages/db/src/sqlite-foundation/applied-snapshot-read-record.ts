import type { Database } from "bun:sqlite";
import type { AppliedSnapshotRecord, ContinuityTransactionPort } from "../ports.js";
import { decodeTimestamp } from "./timestamp.js";

export type SQLiteAppliedSnapshotRecord = AppliedSnapshotRecord;

const columns = [
  "id", "source_primary_id", "leadership_generation", "snapshot_version",
  "replication_version", "content_hash", "snapshot_body", "revision_id", "status",
  "apply_job_id", "failure_code", "applied_at", "discarded_at",
] as const;
const select = `SELECT ${columns.join(", ")} FROM main.applied_snapshots WHERE id = ? LIMIT 1`;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const statuses = ["pending", "applied", "rejected", "rolled-back", "archived"] as const;

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

function integer(row: Record<string, unknown>, key: string, bits: 32 | 64): number {
  const value = row[key];
  const limit = 1n << BigInt(bits - 1);
  if (typeof value !== "bigint" || value < -limit || value >= limit) {
    throw new Error(`Invalid INTEGER ${key}`);
  }
  // Canonical read projection deliberately retains Number(int64) rounding.
  return Number(value);
}

function date(row: Record<string, unknown>, key: string): Date {
  const value = decodeTimestamp(text(row, key));
  if (value.kind !== "finite") return new Date(NaN);
  const milliseconds = value.seconds * 1000n + BigInt(Math.floor(value.nanos / 1_000_000));
  if (milliseconds < -8640000000000000n || milliseconds > 8640000000000000n) {
    return new Date(NaN);
  }
  return new Date(Number(milliseconds));
}

function body(value: unknown): Uint8Array | undefined {
  if (value === null) return undefined;
  if (!(value instanceof Uint8Array)) throw new Error("Invalid BLOB snapshot_body");
  // Empty and non-UTF8 bytes are still BLOBs. Never decode or share native storage.
  return new Uint8Array(value);
}

/** Unwired literal TEXT lookup; no PostgreSQL UUID-input normalization.
 * Owns only an uncached statement, finalized before absence or projection.
 * Caller connection, cache, transaction and savepoints remain caller-owned.
 * Prepare/get/finalize/projection errors reject; finally gives finalizer priority.
 */
export async function getSQLiteAppliedSnapshotRecord(
  db: Database,
  id: string,
): ReturnType<ContinuityTransactionPort["getAppliedSnapshot"]> {
  if (typeof id !== "string") throw new TypeError("Applied snapshot ID must be a string");
  const statement = db.prepare(select);
  let value: unknown;
  try {
    value = statement.get(id);
  } finally {
    statement.finalize();
  }
  if (value === null) return undefined;
  if (typeof value !== "object" || Array.isArray(value)) {
    throw new Error("Invalid applied snapshot row");
  }
  const row = value as Record<string, unknown>;
  if (Object.keys(row).length !== columns.length || columns.some(key => !Object.hasOwn(row, key))) {
    throw new Error("Invalid applied snapshot columns");
  }
  const status = text(row, "status");
  if (!statuses.includes(status as typeof statuses[number])) throw new Error("Invalid applied snapshot status");
  return {
    id: identifier(row, "id"),
    sourcePrimaryId: identifier(row, "source_primary_id"),
    leadershipGeneration: integer(row, "leadership_generation", 64),
    snapshotVersion: integer(row, "snapshot_version", 32),
    replicationVersion: integer(row, "replication_version", 32),
    contentHash: text(row, "content_hash"),
    snapshotBody: body(row.snapshot_body),
    revisionId: row.revision_id === null ? undefined : identifier(row, "revision_id"),
    status: status as AppliedSnapshotRecord["status"],
    applyJobId: row.apply_job_id === null ? undefined : identifier(row, "apply_job_id"),
    failureCode: row.failure_code === null ? undefined : text(row, "failure_code"),
    appliedAt: date(row, "applied_at"),
    discardedAt: row.discarded_at === null ? undefined : date(row, "discarded_at"),
  };
}
