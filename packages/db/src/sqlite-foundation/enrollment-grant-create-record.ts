import type { Database, SQLQueryBindings } from "bun:sqlite";
import type { EnrollmentGrantRecord, ContinuityTransactionPort } from "../ports.js";
import { encodeTimestamp } from "./timestamp.js";

// Canonical source order, not SQLite physical column order. Ignore unknown properties.
const fields = [
  ["attemptId", "attempt_id"],
  ["tokenId", "token_id"],
  ["installationId", "installation_id"],
  ["nodeId", "node_id"],
  ["primaryId", "primary_id"],
  ["primaryGeneration", "primary_generation"],
  ["verifiedPreviewDigest", "verified_preview_digest"],
  ["nodeEphemeralPublicKey", "node_ephemeral_public_key"],
  ["sealedBootstrapPayload", "sealed_bootstrap_payload"],
  ["payloadHash", "payload_hash"],
  ["createdAt", "created_at"],
  ["expiresAt", "expires_at"],
] as const satisfies readonly (readonly [keyof EnrollmentGrantRecord, string])[];

function encode(column: string, value: unknown): SQLQueryBindings {
  // SQL NULL bypasses encoders even for required fields; native constraints decide.
  if (value === null) return null;
  if (column === "created_at" || column === "expires_at") {
    const iso = (value as Date).toISOString();
    const millis = BigInt(new Date(iso).getTime());
    const remainder = ((millis % 1000n) + 1000n) % 1000n;
    return encodeTimestamp({
      kind: "finite", seconds: (millis - remainder) / 1000n,
      nanos: Number(remainder) * 1_000_000,
    });
  }
  if (column === "primary_generation") {
    if (typeof value !== "number" || !Number.isFinite(value) || !Number.isInteger(value)) {
      throw new Error("Invalid SQLite primaryGeneration finite integer int64");
    }
    // Exact represented integer, including unsafe integers. No extra rounding.
    const integer = BigInt(value);
    if (integer < -(1n << 63n) || integer >= (1n << 63n)) {
      throw new Error("Invalid SQLite primaryGeneration signed int64 range");
    }
    return integer;
  }
  return value as SQLQueryBindings;
}

/** Unwired canonical createEnrollmentGrant on a caller-owned connection.
 * One uncached MAIN INSERT, no conflict action, RETURNING, readback, clock, UUID
 * or owned transaction. Undefined omits to native defaults; explicit null binds
 * SQL NULL. Native created_at has no default (unlike PG): missing dates reject,
 * never receive an invented clock. ISO validation and all encoding precede prepare.
 * Text remains source identity, not a promise of exact PostgreSQL wire coercion.
 * Success is void after private statement finalization. Caller transactions and
 * savepoints retain commit/rollback ownership. A finalizer failure can reject AFTER
 * a durable autocommit INSERT; neither query nor finalizer errors are translated.
 */
export async function createSQLiteEnrollmentGrantRecord(
  db: Database,
  grant: Parameters<ContinuityTransactionPort["createEnrollmentGrant"]>[0],
): ReturnType<ContinuityTransactionPort["createEnrollmentGrant"]> {
  const selected: [string, unknown][] = [];
  for (const [property, column] of fields) {
    const value = grant[property];
    if (value !== undefined) selected.push([column, value]);
  }
  const bindings = selected.map(([column, value]) => encode(column, value));
  const statement = db.prepare(
    selected.length
      ? `INSERT INTO main.enrollment_grants (${selected.map(([column]) => column).join(", ")})`
        + ` VALUES (${selected.map(() => "?").join(", ")})`
      : "INSERT INTO main.enrollment_grants DEFAULT VALUES",
  );
  try {
    statement.run(...bindings);
  } finally {
    statement.finalize();
  }
}
