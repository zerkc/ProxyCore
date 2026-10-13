import { Database } from "bun:sqlite";
import { SQLiteWriter } from "./write.js";
import { decodeTimestamp, encodeTimestamp, type Timestamp } from "./timestamp.js";

type Finite = Extract<Timestamp, { kind: "finite" }>;
type RecoveryOptions = Readonly<{ leaseMs?: number; now?: Readonly<Finite>; signal?: AbortSignal }>;

/** Private and unwired: state recovery is NOT safe resumption of a running apply,
 * nor a guarantee against duplicate external processing. Claim and recovery use
 * separate transactions. BEGIN IMMEDIATE serializes all SQLite writers, unlike
 * PG recovery's independent UPDATE without a revision advisory lock.
 * Use the foundation safeIntegers connection. Aborts are synchronous boundary
 * checks; Bun's blocking busy wait requires a bounded busy_timeout and cannot
 * immediately observe cancellation. Only an actual successful COMMIT publishes
 * the count. Genuine migrated schemas are expected, not arbitrary corrupt files.
 * Signed safe-integer lease milliseconds (including zero/negative) are a native
 * policy restriction, NOT parity with PostgreSQL's unvalidated recovery lease.
 */
export function recoverStaleSQLiteJobs(db: Database, options: RecoveryOptions = {}): bigint {
  options.signal?.throwIfAborted();
  const suppliedLease = options.leaseMs;
  const leaseMs = suppliedLease === undefined ? 120000 : suppliedLease;
  if (!Number.isSafeInteger(leaseMs)) throw new Error("Lease milliseconds must be a signed safe integer");
  const explicit = options.now;
  return new SQLiteWriter(db, explicit === undefined ? undefined : () => explicit).write(({ timestamp }) => {
    const captured = decodeTimestamp(timestamp);
    if (captured.kind !== "finite") throw new Error("Recovery clock must be finite");
    const total = captured.seconds * 1000000000n + BigInt(captured.nanos) - BigInt(leaseMs) * 1000000n;
    const nanos = ((total % 1000000000n) + 1000000000n) % 1000000000n;
    // encodeTimestamp range-checks signed int64 seconds; never clamp to infinity.
    const cutoff = encodeTimestamp({ kind: "finite", seconds: (total - nanos) / 1000000000n, nanos: Number(nanos) });
    options.signal?.throwIfAborted();
    const update = db.prepare(`UPDATE main.apply_jobs SET status='queued',claimed_at=NULL,started_at=NULL,
      error_message='Recovered after worker lease expiration'
      WHERE status IN ('validating','applying') AND claimed_at IS NOT NULL AND claimed_at COLLATE BINARY < ?`);
    try { update.run(cutoff); }
    finally { update.finalize(); }
    const count = db.prepare<{ count: unknown }, []>("SELECT changes() AS count");
    let value: unknown;
    try { value = count.get()?.count; }
    finally { count.finalize(); }
    if (typeof value !== "bigint" || value < 0n || value > (1n << 63n) - 1n) {
      throw new Error("Recovery changes count must be a nonnegative signed int64 bigint");
    }
    options.signal?.throwIfAborted();
    return value;
  });
}
