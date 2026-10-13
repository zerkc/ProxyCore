import type { Database } from "bun:sqlite";
import type { ContinuityTransactionPort, EnrollmentAttemptRecord } from "../ports.js";
import { decodeTimestamp } from "./timestamp.js";

export type SQLiteEnrollmentAttemptRecord = EnrollmentAttemptRecord;

const columns = [
  "id", "state", "primary_url", "expected_primary_id", "verified_primary_id",
  "verified_primary_node_id", "verified_leadership_generation",
  "verified_primary_tls_spki_sha256", "verified_primary_ca_fingerprint", "preview_digest",
  "local_node_ip", "archive_id", "ephemeral_private_key_wrapped", "bootstrap_payload",
  "node_credential_secret_id", "cluster_key_id", "initial_snapshot_hash",
  "initial_snapshot_revision_id", "initial_apply_job_id", "failure_code",
  "confirmed_at", "created_at", "updated_at",
] as const;
const select = `SELECT ${columns.join(", ")} FROM main.enrollment_attempts WHERE id = ? LIMIT 1`;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const states = new Set<EnrollmentAttemptRecord["state"]>([
  "draft", "verified", "confirmed", "exchanged", "archived", "initial-apply-pending",
  "committed", "cancelled", "recoverable", "failed",
]);

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

function leadership(row: Record<string, unknown>): number | undefined {
  const value = row.verified_leadership_generation;
  if (value === null) return undefined;
  const limit = 1n << 63n;
  if (typeof value !== "bigint" || value < -limit || value >= limit) {
    throw new Error("Invalid INTEGER verified_leadership_generation");
  }
  // Read projection intentionally retains the canonical Number(int64) rounding policy.
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
 * Unwired, literal TEXT lookup. Owns only its uncached statement, not the connection,
 * transaction, savepoints or caller statement cache. Finalize precedes all projection.
 */
export async function getSQLiteEnrollmentAttemptRecord(
  db: Database,
  id: string,
): ReturnType<ContinuityTransactionPort["getEnrollmentAttempt"]> {
  if (typeof id !== "string") throw new TypeError("Enrollment attempt ID must be a string");
  const statement = db.prepare(select);
  let raw: unknown;
  try {
    raw = statement.all(id);
  } finally {
    statement.finalize();
  }
  if (!Array.isArray(raw) || raw.length > 1) throw new Error("Invalid enrollment attempt result");
  if (raw.length === 0) return undefined;
  const value: unknown = raw[0];
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error("Invalid enrollment attempt row");
  }
  const row = value as Record<string, unknown>;
  if (Object.keys(row).length !== columns.length || columns.some(key => !Object.hasOwn(row, key))) {
    throw new Error("Invalid enrollment attempt columns");
  }
  const state = text(row, "state") as EnrollmentAttemptRecord["state"];
  if (!states.has(state)) throw new Error("Invalid enrollment attempt state");
  const bootstrap = optionalText(row, "bootstrap_payload");
  return {
    id: identifier(row, "id"),
    state,
    primaryUrl: text(row, "primary_url"),
    expectedPrimaryId: optionalText(row, "expected_primary_id", true),
    verifiedPrimaryId: optionalText(row, "verified_primary_id", true),
    verifiedPrimaryNodeId: optionalText(row, "verified_primary_node_id", true),
    verifiedLeadershipGeneration: leadership(row),
    verifiedPrimaryTlsSpkiSha256: optionalText(row, "verified_primary_tls_spki_sha256"),
    verifiedPrimaryCaFingerprint: optionalText(row, "verified_primary_ca_fingerprint"),
    previewDigest: optionalText(row, "preview_digest"),
    localNodeIp: text(row, "local_node_ip"),
    archiveId: optionalText(row, "archive_id", true),
    ephemeralPrivateKeyWrapped: text(row, "ephemeral_private_key_wrapped"),
    bootstrapPayload: bootstrap ? new TextEncoder().encode(bootstrap) : undefined,
    nodeCredentialSecretId: optionalText(row, "node_credential_secret_id", true),
    clusterKeyId: optionalText(row, "cluster_key_id", true),
    initialSnapshotHash: optionalText(row, "initial_snapshot_hash"),
    initialSnapshotRevisionId: optionalText(row, "initial_snapshot_revision_id", true),
    initialApplyJobId: optionalText(row, "initial_apply_job_id", true),
    failureCode: optionalText(row, "failure_code"),
    confirmedAt: row.confirmed_at === null ? undefined : date(row, "confirmed_at"),
    createdAt: date(row, "created_at"),
    updatedAt: date(row, "updated_at"),
  };
}
