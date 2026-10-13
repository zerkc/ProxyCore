import type { Database } from "bun:sqlite";
import type { ContinuityTransactionPort, EnrollmentGrantRecord } from "../ports.js";
import { decodeTimestamp } from "./timestamp.js";

export type SQLiteEnrollmentGrantRecord = EnrollmentGrantRecord;

const columns = [
  "attempt_id", "token_id", "installation_id", "node_id", "primary_id",
  "primary_generation", "verified_preview_digest", "node_ephemeral_public_key",
  "sealed_bootstrap_payload", "payload_hash", "created_at", "expires_at",
] as const;
const select = `SELECT ${columns.join(", ")} FROM main.enrollment_grants WHERE attempt_id = ? LIMIT 1`;
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

function generation(row: Record<string, unknown>): number {
  const value = row.primary_generation;
  const limit = 1n << 63n;
  if (typeof value !== "bigint" || value < -limit || value >= limit) {
    throw new Error("Invalid INTEGER primary_generation");
  }
  // Read projection retains canonical Number(int64) rounding, not write-binder policy.
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

/**
 * Unwired literal TEXT lookup by attempt ID, not token ID or normalized UUID.
 * Owns only an uncached statement; caller connection, cache and transaction survive.
 * Statement completion precedes projection and absence publication.
 */
export async function getSQLiteEnrollmentGrantRecord(
  db: Database,
  id: string,
): ReturnType<ContinuityTransactionPort["getEnrollmentGrant"]> {
  if (typeof id !== "string") throw new TypeError("Enrollment grant ID must be a string");
  const statement = db.prepare(select);
  let value: unknown;
  try {
    value = statement.get(id);
  } finally {
    statement.finalize();
  }
  if (value === null) return undefined;
  if (typeof value !== "object" || Array.isArray(value)) {
    throw new Error("Invalid enrollment grant row");
  }
  const row = value as Record<string, unknown>;
  if (Object.keys(row).length !== columns.length || columns.some(key => !Object.hasOwn(row, key))) {
    throw new Error("Invalid enrollment grant columns");
  }
  return {
    attemptId: identifier(row, "attempt_id"),
    tokenId: identifier(row, "token_id"),
    installationId: identifier(row, "installation_id"),
    nodeId: identifier(row, "node_id"),
    primaryId: identifier(row, "primary_id"),
    primaryGeneration: generation(row),
    verifiedPreviewDigest: row.verified_preview_digest === null
      ? undefined : text(row, "verified_preview_digest"),
    nodeEphemeralPublicKey: text(row, "node_ephemeral_public_key"),
    sealedBootstrapPayload: text(row, "sealed_bootstrap_payload"),
    payloadHash: text(row, "payload_hash"),
    createdAt: date(row, "created_at"),
    expiresAt: date(row, "expires_at"),
  };
}
