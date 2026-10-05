import type { Database } from "bun:sqlite";
import type { SnapshotAcknowledgement, ContinuityTransactionPort } from "../ports.js";
import { decodeTimestamp } from "./timestamp.js";

export type SQLiteSnapshotAcknowledgementRecord = SnapshotAcknowledgement;

const columns = [
  "node_id", "content_hash", "snapshot_version", "replication_version",
  "revision_id", "leadership_generation", "applied_at", "received_at",
] as const;
const select = `SELECT ${columns.join(", ")} FROM main.node_snapshot_acks WHERE node_id = ? AND content_hash = ? LIMIT 1`;
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

function integer(row: Record<string, unknown>, key: string, bits: 32 | 64): number {
  const value = row[key];
  const limit = 1n << BigInt(bits - 1);
  if (typeof value !== "bigint" || value < -limit || value >= limit) {
    throw new Error(`Invalid INTEGER ${key}`);
  }
  // Preserve canonical Number(int64) rounding on read.
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

/** Unwired literal bound TEXT lookup, with no PostgreSQL UUID normalization.
 * Own only an uncached statement; finalize before absence/projection/publication.
 * Errors reject with finalizer priority. Caller connection/cache/transactions
 * and savepoints remain caller-owned; this reader performs no writes or clocks.
 */
export async function getSQLiteSnapshotAcknowledgementRecord(
  db: Database,
  nodeId: string,
  contentHash: string,
): ReturnType<ContinuityTransactionPort["getSnapshotAcknowledgement"]> {
  if (typeof nodeId !== "string" || typeof contentHash !== "string") {
    throw new TypeError("Snapshot acknowledgement keys must be strings");
  }
  const statement = db.prepare(select);
  let value: unknown;
  try {
    value = statement.get(nodeId, contentHash);
  } finally {
    statement.finalize();
  }
  if (value === null) return undefined;
  if (typeof value !== "object" || Array.isArray(value)) {
    throw new Error("Invalid snapshot acknowledgement row");
  }
  const row = value as Record<string, unknown>;
  if (Object.keys(row).length !== columns.length || columns.some(key => !Object.hasOwn(row, key))) {
    throw new Error("Invalid snapshot acknowledgement columns");
  }
  return {
    nodeId: identifier(row, "node_id"),
    contentHash: text(row, "content_hash"),
    snapshotVersion: integer(row, "snapshot_version", 32),
    replicationVersion: integer(row, "replication_version", 32),
    revisionId: identifier(row, "revision_id"),
    leadershipGeneration: integer(row, "leadership_generation", 64),
    appliedAt: date(row, "applied_at"),
    receivedAt: date(row, "received_at"),
  };
}
