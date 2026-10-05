import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import { createInterface } from "node:readline";
import type { JobStore } from "../ports.js";
import { openSQLite } from "./open.js";
import { encodeTimestamp } from "./timestamp.js";
import { claimNextSQLiteJobRecord } from "./job-claim-record.js";
import { recoverStaleSQLiteJobRecords } from "./job-recovery-record.js";

type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2)
      ? true : false
    : false;
type Expect<T extends true> = T;
type CanonicalParameters = Expect<Identical<Parameters<typeof recoverStaleSQLiteJobRecords>,
  [db: Database, leaseMs: Parameters<JobStore["recoverStale"]>[0], now?: Parameters<JobStore["recoverStale"]>[1]]>>;
type CanonicalReturn = Expect<Identical<ReturnType<typeof recoverStaleSQLiteJobRecords>,
  ReturnType<JobStore["recoverStale"]>>>;

const [mode, path, originalID] = process.argv.slice(2);
const db = openSQLite(path);
const revision = db.query<{ revision_id: string }, []>("SELECT revision_id FROM apply_jobs LIMIT 1").get()!.revision_id;
const id = (n: number) => `bbbbbbbb-bbbb-bbbb-bbbb-${String(n).padStart(12, "0")}`;
const stamp = (seconds: bigint, nanos = 0) => encodeTimestamp({ kind: "finite", seconds, nanos })!;
const maxDate = 8640000000000000;
const rawJSON = ' \n{"n":922337203685477580812345,"snow":"雪🦊"} \t';
let cases = 0;
function rows(connection = db) { return connection.query("SELECT * FROM main.apply_jobs ORDER BY id").all(); }
function insert(n = 1, status = "validating", claimed: string | null = stamp(0n)) {
  db.query(`INSERT INTO main.apply_jobs(id,revision_id,target,status,source,correlation_id,created_at,
    claimed_at,started_at,finished_at,validation_output,apply_output,health_output,error_message,
    source_primary_id,source_node_id,source_revision_id,snapshot_content_hash,snapshot_version,
    replication_version,leadership_generation)
    VALUES(?,?,'combined',?,'sync',?,'0',?,'2','0',?,'null',NULL,'old',?,?,?,'',-2147483648,
      2147483647,9223372036854775807)`)
    .run(id(n), revision, status, `correlation-${n}`, claimed, rawJSON, id(50), id(51), id(52));
}
async function check(name: string, body: () => void | Promise<void>) {
  await body(); cases++; console.log(`PASS ${name}`);
}
async function test(name: string, body: () => void | Promise<void>) {
  db.exec("DELETE FROM main.apply_jobs");
  const revisions = db.query("SELECT * FROM main.config_revisions ORDER BY id").all();
  await check(name, body);
  assert.equal(db.inTransaction, false);
  assert.deepEqual(db.query("SELECT * FROM main.config_revisions ORDER BY id").all(), revisions);
}
function recover(connection = db, lease = 0, now: Date = new Date(1000)) {
  const promise = recoverStaleSQLiteJobRecords(connection, lease, now);
  assert(promise instanceof Promise, "success and errors are public Promises");
  return promise;
}
async function fails(body: () => Promise<unknown>, pattern?: RegExp) {
  const before = rows();
  if (pattern) await assert.rejects(body, pattern);
  else await assert.rejects(body);
  assert.equal(db.inTransaction, false);
  assert.deepEqual(rows(), before);
  const reopened = openSQLite(path);
  try { assert.deepEqual(rows(reopened), before); }
  finally { reopened.close(); }
}

// A local facade delegates real transactions/statements to this migrated DB.
// Only changes() is substituted for otherwise unallocatable int64 counts. It
// checks that conversion happens after actual COMMIT, without production hooks.
function countFacade(count: bigint | Error) {
  let committed = false, reads = 0, finalized = 0;
  const connection = {
    get inTransaction() { return db.inTransaction; },
    transaction(callback: () => unknown) {
      const transaction = db.transaction(callback);
      return { immediate() {
        const result = transaction.immediate();
        committed = true;
        return result;
      } };
    },
    prepare(sql: string) {
      if (sql !== "SELECT changes() AS count") return db.prepare(sql);
      return {
        get() {
          assert.equal(db.inTransaction, true);
          reads++;
          if (count instanceof Error) throw count;
          return { count };
        },
        finalize() { finalized++; },
      };
    },
  } as unknown as Database;
  return { connection, state: () => ({ committed, reads, finalized }) };
}

async function unit() {
  await check("Go-enqueue-public-claim-recover-reclaim", async () => {
    const revisions = db.query("SELECT * FROM main.config_revisions ORDER BY id").all();
    const first = (await claimNextSQLiteJobRecord(db))!;
    assert.equal(first.id, originalID); assert.equal(first.status, "validating");
    assert.equal(await recover(db, 0, new Date(maxDate)), 1);
    const recovered = rows()[0] as Record<string, unknown>;
    assert.equal(recovered.status, "queued"); assert.equal(recovered.claimed_at, null);
    assert.equal(recovered.started_at, null);
    const second = (await claimNextSQLiteJobRecord(db))!;
    assert.equal(second.id, originalID); assert.equal(second.revisionId, first.revisionId);
    assert.equal(second.correlationId, first.correlationId);
    assert.equal(second.errorMessage, "Recovered after worker lease expiration");
    assert.deepEqual(db.query("SELECT * FROM main.config_revisions ORDER BY id").all(), revisions);
    const reopened = openSQLite(path);
    try { assert.deepEqual(rows(reopened), rows()); }
    finally { reopened.close(); }
  });
  await test("empty-repeat-is-number-zero", async () => {
    assert.equal(await recover(), 0); assert.equal(await recover(), 0);
  });
  for (const status of ["queued", "validating", "applying", "applied", "failed", "rolled-back"]) {
    for (const claimed of [null, "0", "2", stamp(0n, 999999999), stamp(1n), stamp(1n, 1)]) {
      await test(`strict-cutoff-${status}-${claimed}`, async () => {
        insert(1, status, claimed);
        const before = rows()[0] as Record<string, unknown>;
        const eligible = ["validating", "applying"].includes(status) && claimed !== null && claimed < stamp(1n);
        assert.equal(await recover(), eligible ? 1 : 0);
        assert.deepEqual(rows()[0], eligible ? { ...before, status: "queued", claimed_at: null,
          started_at: null, error_message: "Recovered after worker lease expiration" } : before);
        assert.equal(await recover(), 0);
      });
    }
  }
  for (const output of [null, "null", rawJSON]) {
    for (const leadership of [-9223372036854775808n, 9007199254740993n, null]) {
      await test(`metadata-${output}-${leadership}`, async () => {
        insert();
        db.query("UPDATE main.apply_jobs SET validation_output=?,leadership_generation=?").run(output, leadership);
        const before = rows()[0] as Record<string, unknown>;
        assert.equal(await recover(), 1);
        assert.deepEqual(rows()[0], { ...before, status: "queued", claimed_at: null,
          started_at: null, error_message: "Recovered after worker lease expiration" });
      });
    }
  }
  for (const source of ["ordinary", "import", "sync"]) {
    for (const historical of [null, "0", "2", stamp(-9223372036854775808n), stamp(9223372036854775807n, 999999999)]) {
      await test(`source-historical-${source}-${historical}`, async () => {
        insert();
        db.query(`INSERT OR IGNORE INTO main.users(id,username,password_hash,role,created_at,updated_at)
          VALUES(?,'public-recovery-fixture','opaque','owner','0','2')`).run(id(99));
        db.query("UPDATE main.apply_jobs SET source=?,actor_user_id=?,started_at=?,finished_at=?")
          .run(source, id(99), historical, historical);
        const before = rows()[0] as Record<string, unknown>;
        assert.equal(await recover(), 1);
        assert.deepEqual(rows()[0], { ...before, status: "queued", claimed_at: null,
          started_at: null, error_message: "Recovered after worker lease expiration" });
      });
    }
  }
  // Literal normalized cutoff goldens, independent of the wrapper conversion.
  const boundaries: [string, number, number, bigint, number][] = [
    ["zero", 0, 0, 0n, 0], ["negative-one-ms", -1, 0, -1n, 999000000],
    ["source-default-sized-lease", 123456, 120000, 3n, 456000000],
    ["MAXDate", maxDate, 0, 8640000000000n, 0],
    ["MINDate", -maxDate, 0, -8640000000000n, 0],
    ["unsafe-positive-cancellation", maxDate, 17280000000000000, -8640000000000n, 0],
    ["unsafe-negative-cancellation", -maxDate, -17280000000000000, 8640000000000n, 0],
    ["negative-lease", 0, -1, 0n, 1000000],
    ["positive-fraction-to-zero", 0, -0.9, 0n, 0],
    ["negative-fraction-to-zero", 0, 0.9, 0n, 0],
    ["negative-fraction-to-minus-one", 0, 1.9, -1n, 999000000],
    ["positive-fraction-to-one", 0, -1.9, 0n, 1000000],
    ["input-Date-TimeClip-positive", 0.9, 0, 0n, 0],
    ["input-Date-TimeClip-negative", -0.9, 0, 0n, 0],
    ["fraction-cross-zero", 1, 1.5, 0n, 0],
  ];
  for (const [name, millis, lease, seconds, nanos] of boundaries) {
    await test(name, async () => {
      const before = nanos === 0 ? stamp(seconds - 1n, 999999999) : stamp(seconds, nanos - 1);
      const after = nanos === 999999999 ? stamp(seconds + 1n) : stamp(seconds, nanos + 1);
      insert(1, "validating", before); insert(2, "applying", stamp(seconds, nanos));
      insert(3, "validating", after);
      assert.equal(await recover(db, lease, new Date(millis)), 1);
      assert.equal((rows()[1] as Record<string, unknown>).status, "applying");
      assert.equal((rows()[2] as Record<string, unknown>).status, "validating");
    });
  }
  await test("default-now-explicit-undefined-captured-once", async () => {
    insert(1, "validating", "0"); insert(2, "applying", "2");
    assert.equal(await recoverStaleSQLiteJobRecords(db, 0), 1);
    db.query("UPDATE main.apply_jobs SET status='validating',claimed_at='0' WHERE id=?").run(id(1));
    assert.equal(await recoverStaleSQLiteJobRecords(db, 0, undefined), 1);
    let calls = 0;
    const clock = { getTime() { calls++; return calls === 1 ? -1 : maxDate; } } as Date;
    insert(3, "validating", stamp(-1n, 998999999));
    insert(4, "applying", stamp(-1n, 999000000));
    assert.equal(await recover(db, 0, clock), 1); assert.equal(calls, 1);
  });
  for (const lease of [NaN, Infinity, -Infinity, undefined]) {
    await test(`invalid-lease-${lease}`, async () => {
      insert(); await fails(() => recoverStaleSQLiteJobRecords(db, lease as number, new Date(0)), /SQLite.*cutoff.*finite/i);
    });
  }
  await test("undefined-lease-with-default-now-not-native-default", async () => {
    insert(1, "validating", "0");
    await fails(() => recoverStaleSQLiteJobRecords(db, undefined as unknown as number, undefined), /SQLite.*cutoff.*finite/i);
  });
  await test("missing-lease-not-native-default", async () => {
    insert(1, "validating", "0");
    // @ts-expect-error canonical lease is required; runtime omission is tested only.
    await fails(() => recoverStaleSQLiteJobRecords(db), /SQLite.*cutoff.*finite/i);
  });
  for (const millis of [NaN, Infinity, -Infinity, maxDate + 1, -maxDate - 1]) {
    await test(`invalid-Date-${millis}`, async () => {
      insert(); await fails(() => recover(db, 0, new Date(millis)), /SQLite.*cutoff.*finite/i);
    });
  }
  for (const [millis, lease] of [[maxDate, -1], [-maxDate, 1]]) {
    await test(`cutoff-outside-Date-${millis}`, async () => {
      insert(); await fails(() => recover(db, lease, new Date(millis)), /SQLite.*cutoff.*finite/i);
    });
  }
  for (const lease of [null, "1", false, true, { valueOf: () => 1 }]) {
    await test(`JS-coercion-${String(lease)}`, async () => {
      insert(1, "validating", stamp(-1n, 998999999));
      assert.equal(await recover(db, lease as number, new Date(0)), 1);
    });
  }
  for (const lease of [1n, Symbol("lease")]) {
    await test(`JS-subtraction-error-${String(lease)}`, async () => {
      insert(); await fails(() => recover(db, lease as unknown as number), /TypeError|BigInt|convert/i);
    });
  }
  for (const time of [null, 0, {}, { getTime: 1 }]) {
    await test(`JS-getTime-error-${String(time)}`, async () => {
      insert(); await fails(() => recover(db, 0, time as unknown as Date), /getTime|null/i);
    });
  }
  for (const [value, seconds] of [[null, 0n], ["1000", 1n]] as const) {
    await test(`JS-getTime-result-coercion-${value}`, async () => {
      insert(1, "validating", stamp(seconds - 1n, 999999999));
      insert(2, "applying", stamp(seconds));
      const time = { getTime: () => value } as unknown as Date;
      assert.equal(await recover(db, 0, time), 1);
    });
  }
  await test("primitive-getTime-error-identity", async () => {
    insert(); const sentinel = new Error("fixture getTime error");
    const promise = recover(db, 0, { getTime() { throw sentinel; } } as unknown as Date);
    await assert.rejects(promise, (error) => error === sentinel);
    assert.equal((rows()[0] as Record<string, unknown>).status, "validating");
  });
  for (const count of [0n, 1n, 9007199254740993n, 9223372036854775807n]) {
    await test(`Number-count-after-real-COMMIT-${count}`, async () => {
      insert(); const seam = countFacade(count);
      const result = await recover(seam.connection);
      assert.equal(result, Number(count)); assert.equal(typeof result, "number");
      assert.deepEqual(seam.state(), { committed: true, reads: 1, finalized: 1 });
      const reopened = openSQLite(path);
      try { assert.equal((rows(reopened)[0] as Record<string, unknown>).status, "queued"); }
      finally { reopened.close(); }
    });
  }
  await test("native-negative-count-rejection-not-zero", async () => {
    insert(); const seam = countFacade(-1n);
    await fails(() => recover(seam.connection), /nonnegative/);
    assert.deepEqual(seam.state(), { committed: false, reads: 1, finalized: 1 });
  });
  await test("native-count-error-identity-not-zero", async () => {
    insert(); const sentinel = new Error("fixture count failure"), seam = countFacade(sentinel);
    const before = rows();
    await assert.rejects(recover(seam.connection), (error) => error === sentinel);
    assert.deepEqual(rows(), before);
    assert.deepEqual(seam.state(), { committed: false, reads: 1, finalized: 1 });
    assert.equal(await recover(), 1);
  });
  await test("real-deferred-COMMIT-failure-no-count-full-rollback", async () => {
    insert(); const before = rows();
    const changes = () => db.query<{ n: bigint }, []>("SELECT total_changes() AS n").get()!.n;
    const baseline = changes();
    db.exec(`CREATE TABLE main.public_recovery_guard(job_id TEXT REFERENCES apply_jobs(id) DEFERRABLE INITIALLY DEFERRED);
      CREATE TEMP TRIGGER public_recovery_guard_insert AFTER UPDATE ON main.apply_jobs BEGIN
        INSERT INTO public_recovery_guard VALUES('missing-fixture-job'); END`);
    try {
      let published: number | undefined;
      await assert.rejects(async () => { published = await recover(); }, /FOREIGN KEY/);
      assert.equal(published, undefined); assert.equal(changes() - baseline, 2n);
      assert.equal(db.inTransaction, false); assert.deepEqual(rows(), before);
      assert.equal(db.query<{ n: bigint }, []>("SELECT count(*) AS n FROM public_recovery_guard").get()!.n, 0n);
      const reopened = openSQLite(path);
      try {
        assert.deepEqual(rows(reopened), before);
        assert.equal(reopened.query<{ n: bigint }, []>("SELECT count(*) AS n FROM public_recovery_guard").get()!.n, 0n);
      } finally { reopened.close(); }
    } finally { db.exec("DROP TRIGGER temp.public_recovery_guard_insert; DROP TABLE main.public_recovery_guard"); }
    assert.equal(await recover(), 1);
  });
  await test("SQL-update-error-rollback-next-success", async () => {
    insert(); db.exec("CREATE TEMP TRIGGER public_recovery_fail BEFORE UPDATE ON main.apply_jobs BEGIN SELECT RAISE(ABORT,'fixture-update'); END");
    try { await fails(() => recover(), /fixture-update/); }
    finally { db.exec("DROP TRIGGER temp.public_recovery_fail"); }
    assert.equal(await recover(), 1);
  });
  await test("nested-Promise-rejection-keeps-caller-transaction", async () => {
    insert(); const before = rows(); db.exec("BEGIN IMMEDIATE");
    try {
      await assert.rejects(recover(), /Nested/);
      assert.equal(db.inTransaction, true); assert.deepEqual(rows(), before);
    } finally { db.exec("ROLLBACK"); }
    assert.equal(await recover(), 1);
  });
  await test("unsafe-connection-native-error-not-zero", async () => {
    insert(); const unsafe = new Database(path, { strict: true, safeIntegers: false });
    try { await fails(() => recover(unsafe), /bigint/); }
    finally { unsafe.close(); }
    assert.equal(await recover(), 1);
  });
  await test("closed-connection-Promise-error", async () => {
    insert(); const closed = openSQLite(path); closed.close();
    await fails(() => recover(closed), /closed|finalized|transaction/i);
  });
  await test("own-statement-finalization-cached-caller-preserved", async () => {
    insert(); const cached = db.query<{ count: bigint }, []>("SELECT changes() AS count");
    assert.equal(await recover(), 1); assert.equal(cached.get()!.count, 1n);
    db.exec("ALTER TABLE main.apply_jobs ADD COLUMN fixture_only TEXT");
    db.exec("ALTER TABLE main.apply_jobs DROP COLUMN fixture_only");
    assert.equal(await recover(), 0);
  });
  await test("many-repeat-reopen", async () => {
    insert(1); insert(2, "applying"); insert(3, "applied");
    assert.equal(await recover(), 2); assert.equal(await recover(), 0);
    const reopened = openSQLite(path);
    try { assert.deepEqual(rows(reopened), rows()); }
    finally { reopened.close(); }
  });
  console.log(`CASES ${cases}`);
}

async function child() {
  db.exec("PRAGMA busy_timeout=150");
  const input = createInterface({ input: process.stdin });
  console.log("READY");
  try {
    for await (const command of input) {
      if (command === "exit") break;
      try {
        switch (command) {
          case "hold": db.exec("BEGIN IMMEDIATE"); console.log("HELD"); break;
          case "release": db.exec("ROLLBACK"); console.log("RELEASED"); break;
          case "claim": {
            const got = await claimNextSQLiteJobRecord(db);
            console.log(got ? `OWNED ${got.id}` : "EMPTY"); break;
          }
          case "recover": console.log(`COUNT ${await recover(db, 0, new Date(maxDate))}`); break;
          case "recover-fresh": console.log(`COUNT ${await recover(db, 0, new Date(0))}`); break;
          default: throw new Error("Unknown public recovery fixture command");
        }
      } catch (error) {
        if ((error as { code?: string }).code !== "SQLITE_BUSY") throw error;
        console.log("BUSY");
      }
    }
  } finally { input.close(); }
}
try {
  if (mode === "unit") await unit();
  else if (mode === "child") await child();
  else throw new Error("Missing public recovery fixture mode");
} finally {
  if (db.inTransaction) db.exec("ROLLBACK");
  db.close();
}
