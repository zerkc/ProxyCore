import type { Database } from "bun:sqlite";
import type { NodeStateRecord, ContinuityTransactionPort } from "../ports.js";
import { decodeTimestamp } from "./timestamp.js";

export type SQLiteNodeStateRecord = NodeStateRecord;

const columns = ["id", "enrolled_at", "enrollment_primary_id", "last_seen_at", "last_applied_snapshot_id",
  "enrollment_attempt_id", "primary_url", "primary_installation_id", "primary_tls_spki_sha256", "credential_id",
  "sync_enabled", "last_attempt_at", "last_success_at", "consecutive_failures", "next_attempt_at", "last_error_code", "updated_at"] as const;
const select = `SELECT ${columns.join(", ")} FROM main.node_state WHERE id = ? LIMIT 1`;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

function text(row: Record<string, unknown>, key: string): string {
  const value = row[key];
  if (typeof value !== "string" || value.includes("\0")) throw new Error(`Invalid TEXT ${key}`);
  return value;
}
function optionalText(row: Record<string, unknown>, key: string, identifier = false): string | undefined {
  if (row[key] === null) return undefined;
  const value = text(row, key);
  if (identifier && !uuid.test(value)) throw new Error(`Invalid stored UUID ${key}`);
  return value;
}
function integer(row: Record<string, unknown>, key: string): bigint {
  const value = row[key];
  if (typeof value !== "bigint") throw new Error(`Invalid INTEGER ${key}`);
  return value;
}
function date(row: Record<string, unknown>, key: string): Date {
  const value = decodeTimestamp(text(row, key));
  // Same general public-date policy as revision readers, not scheduling policy.
  // Both infinities and finite values outside Date's range project to Invalid Date.
  if (value.kind !== "finite") return new Date(NaN);
  const milliseconds = value.seconds * 1000n + BigInt(Math.floor(value.nanos / 1_000_000));
  if (milliseconds < -8640000000000000n || milliseconds > 8640000000000000n) return new Date(NaN);
  return new Date(Number(milliseconds));
}
function optionalDate(row: Record<string, unknown>, key: string): Date | undefined {
  return row[key] === null ? undefined : date(row, key);
}

/** Standalone, unwired canonical getNodeState reader on a caller-owned connection.
 * One uncached cursor is drained and finalized before any validation/projection.
 * No clocks, transactions, caches, writes or connection lifecycle are owned here.
 * Deliberately excludes enrollment_token_hash from SQL and the canonical DTO.
 */
export async function getSQLiteNodeStateRecord(
  db: Database,
): ReturnType<ContinuityTransactionPort["getNodeState"]> {
  const statement = db.prepare(select);
  let raw: unknown;
  try {
    raw = statement.all("default");
  } finally {
    statement.finalize();
  }
  if (!Array.isArray(raw) || raw.length > 1) throw new Error("Invalid node state result");
  if (raw.length === 0) return undefined;
  const value: unknown = raw[0];
  if (typeof value !== "object" || value === null || Array.isArray(value)) throw new Error("Invalid node state row");
  const row = value as Record<string, unknown>;
  if (Object.keys(row).length !== columns.length || columns.some(key => !Object.hasOwn(row, key))) {
    throw new Error("Invalid node state columns");
  }
  const enabled = integer(row, "sync_enabled"), failures = integer(row, "consecutive_failures");
  if (enabled !== 0n && enabled !== 1n) throw new Error("Invalid sync_enabled");
  if (failures < -2147483648n || failures > 2147483647n) throw new Error("Invalid consecutive_failures");
  return {
    id: text(row, "id"),
    enrolledAt: optionalDate(row, "enrolled_at"),
    enrollmentPrimaryId: optionalText(row, "enrollment_primary_id", true),
    lastSeenAt: optionalDate(row, "last_seen_at"),
    lastAppliedSnapshotId: optionalText(row, "last_applied_snapshot_id", true),
    enrollmentAttemptId: optionalText(row, "enrollment_attempt_id", true),
    primaryUrl: optionalText(row, "primary_url"),
    primaryInstallationId: optionalText(row, "primary_installation_id", true),
    primaryTlsSpkiSha256: optionalText(row, "primary_tls_spki_sha256"),
    credentialId: optionalText(row, "credential_id", true),
    syncEnabled: enabled === 1n,
    lastAttemptAt: optionalDate(row, "last_attempt_at"),
    lastSuccessAt: optionalDate(row, "last_success_at"),
    consecutiveFailures: Number(failures),
    nextAttemptAt: optionalDate(row, "next_attempt_at"),
    lastErrorCode: optionalText(row, "last_error_code"),
    updatedAt: date(row, "updated_at"),
  };
}
