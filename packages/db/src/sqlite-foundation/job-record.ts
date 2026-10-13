import type { Database } from "bun:sqlite";
import type { JobRecord } from "../ports.js";
import { getSQLiteJobRow, type SQLiteJobRow } from "./job-read.js";
import { decodeTimestamp } from "./timestamp.js";

export type SQLiteJobRecord = JobRecord;

/** Project the lossless absolute instant, not a local or textual PG date.
 * Normalized fractional nanos are floored to milliseconds before narrowing.
 * Valid instants outside Date's range and both infinities become Invalid Date;
 * malformed encodings remain errors from the native reader/codec.
 */
function date(key: string): Date {
  const value = decodeTimestamp(key);
  if (value.kind !== "finite") return new Date(NaN);
  const milliseconds = value.seconds * 1000n + BigInt(Math.floor(value.nanos / 1_000_000));
  if (milliseconds < -8640000000000000n || milliseconds > 8640000000000000n) return new Date(NaN);
  return new Date(Number(milliseconds));
}

function optionalDate(key: string | null): Date | undefined {
  return key === null ? undefined : date(key);
}

function json(text: string | null): unknown {
  return text === null ? undefined : JSON.parse(text) ?? undefined;
}

/** Public JobRecord-compatible projection; additive and unwired, not a JobStore.
 * Literal ID lookup and all query/validation/finalizer errors belong to the
 * verified native reader. Optional own keys match toJob, including undefined.
 * Number(bigint) intentionally retains the source's unchecked int64 rounding.
 * Each read creates a detached plain record with fresh Date/parsed JSON values.
 */
export function getSQLiteJobRecord(db: Database, id: string): SQLiteJobRecord | undefined {
  const row = getSQLiteJobRow(db, id);
  if (row === undefined) return undefined;
  return projectSQLiteJobRecord(row);
}

/** Shared projection of an already validated native row; no database reread. */
export function projectSQLiteJobRecord(row: SQLiteJobRow): SQLiteJobRecord {
  return {
    id: row.id,
    revisionId: row.revision_id,
    actorUserId: row.actor_user_id ?? undefined,
    target: row.target,
    status: row.status,
    source: row.source,
    sourcePrimaryId: row.source_primary_id ?? undefined,
    sourceNodeId: row.source_node_id ?? undefined,
    sourceRevisionId: row.source_revision_id ?? undefined,
    snapshotContentHash: row.snapshot_content_hash ?? undefined,
    snapshotVersion: row.snapshot_version === null ? undefined : Number(row.snapshot_version),
    replicationVersion: row.replication_version === null ? undefined : Number(row.replication_version),
    leadershipGeneration: row.leadership_generation === null ? undefined : Number(row.leadership_generation),
    correlationId: row.correlation_id,
    createdAt: date(row.created_at),
    claimedAt: optionalDate(row.claimed_at),
    startedAt: optionalDate(row.started_at),
    finishedAt: optionalDate(row.finished_at),
    validationOutput: json(row.validation_output),
    applyOutput: json(row.apply_output),
    healthOutput: json(row.health_output),
    errorMessage: row.error_message ?? undefined,
  };
}
