import type { Database, SQLQueryBindings } from "bun:sqlite";
import type { EnrollmentAttemptRecord, ContinuityTransactionPort } from "../ports.js";
import { encodeTimestamp } from "./timestamp.js";

// Fixed canonical source construction order; unknown input properties never reach SQL.
const fields = [
  ["id", "id"],
  ["state", "state"],
  ["primaryUrl", "primary_url"],
  ["expectedPrimaryId", "expected_primary_id"],
  ["verifiedPrimaryId", "verified_primary_id"],
  ["verifiedPrimaryNodeId", "verified_primary_node_id"],
  ["verifiedLeadershipGeneration", "verified_leadership_generation"],
  ["verifiedPrimaryTlsSpkiSha256", "verified_primary_tls_spki_sha256"],
  ["verifiedPrimaryCaFingerprint", "verified_primary_ca_fingerprint"],
  ["previewDigest", "preview_digest"],
  ["localNodeIp", "local_node_ip"],
  ["archiveId", "archive_id"],
  ["ephemeralPrivateKeyWrapped", "ephemeral_private_key_wrapped"],
  ["bootstrapPayload", "bootstrap_payload"],
  ["nodeCredentialSecretId", "node_credential_secret_id"],
  ["clusterKeyId", "cluster_key_id"],
  ["initialSnapshotHash", "initial_snapshot_hash"],
  ["initialSnapshotRevisionId", "initial_snapshot_revision_id"],
  ["initialApplyJobId", "initial_apply_job_id"],
  ["failureCode", "failure_code"],
  ["confirmedAt", "confirmed_at"],
  ["createdAt", "created_at"],
  ["updatedAt", "updated_at"],
] as const satisfies readonly (readonly [keyof EnrollmentAttemptRecord, string])[];

function encode(column: string, value: unknown): SQLQueryBindings {
  // Explicit SQL NULL bypasses Date and number encoders, including required columns.
  if (value === null) return null;
  if (column.endsWith("_at")) {
    const iso = (value as Date).toISOString();
    const millis = BigInt(new Date(iso).getTime());
    const remainder = ((millis % 1000n) + 1000n) % 1000n;
    return encodeTimestamp({ kind: "finite", seconds: (millis - remainder) / 1000n,
      nanos: Number(remainder) * 1_000_000 });
  }
  if (column === "verified_leadership_generation") {
    if (typeof value !== "number" || !Number.isFinite(value) || !Number.isInteger(value)) {
      throw new Error("Invalid SQLite verifiedLeadershipGeneration finite integer int64");
    }
    // Convert exactly the represented number: no safe-integer restriction or rounding.
    // This cannot recover precision already lost before the caller supplied the value.
    const integer = BigInt(value);
    if (integer < -(1n << 63n) || integer >= (1n << 63n)) {
      throw new Error("Invalid SQLite verifiedLeadershipGeneration signed int64 range");
    }
    return integer;
  }
  return value as SQLQueryBindings;
}

/** Unwired canonical createEnrollmentAttempt on the caller's connection.
 * One private uncached MAIN INSERT, no conflict action, RETURNING, readback, clock,
 * generated ID, connection lifecycle or owned transaction. Undefined fields omit
 * to native INSERT defaults (nullable fields default NULL); bootstrap always binds
 * decoded TEXT for truthy bytes, explicit NULL otherwise, as in the canonical source.
 * Date ISO validation precedes SQL; strings remain raw native bindings, not PG wire
 * coercion guarantees. Success is void after execution/finalization, pending inside
 * caller transactions/savepoints. Later caller COMMIT failures remain caller-owned.
 * A finalizer error after successful autocommit may reject despite a durable INSERT:
 * this method makes no rollback promise.
 */
export async function createSQLiteEnrollmentAttemptRecord(
  db: Database,
  attempt: Parameters<ContinuityTransactionPort["createEnrollmentAttempt"]>[0],
): ReturnType<ContinuityTransactionPort["createEnrollmentAttempt"]> {
  if (![
    "draft", "verified", "confirmed", "exchanged", "archived", "initial-apply-pending",
    "committed", "cancelled", "recoverable", "failed",
  ].includes(attempt.state)) {
    throw new Error(`invalid enrollment attempt state: ${attempt.state}`);
  }
  const selected: [string, unknown][] = [];
  for (const [property, column] of fields) {
    const source = attempt[property];
    const value = property === "bootstrapPayload"
      ? source ? new TextDecoder().decode(source as Uint8Array) : null
      : source;
    if (value !== undefined) selected.push([column, value]);
  }
  const bindings = selected.map(([column, value]) => encode(column, value));
  const statement = db.prepare(
    `INSERT INTO main.enrollment_attempts (${selected.map(([column]) => column).join(", ")})`
    + ` VALUES (${selected.map(() => "?").join(", ")})`,
  );
  try {
    statement.run(...bindings);
  } finally {
    statement.finalize();
  }
}
