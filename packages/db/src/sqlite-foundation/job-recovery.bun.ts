import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import { createInterface } from "node:readline";
import { openSQLite } from "./open.js";
import { SQLiteWriter } from "./write.js";
import { encodeTimestamp, type Timestamp } from "./timestamp.js";
import { claimNextSQLiteJob } from "./job-claim.js";
import { recoverStaleSQLiteJobs } from "./job-recovery.js";

type Finite = Extract<Timestamp, { kind: "finite" }>;
const now: Finite = { kind: "finite", seconds: 123n, nanos: 456 };
const stamp = (seconds: bigint, nanos = 0) => encodeTimestamp({ kind: "finite", seconds, nanos })!;
const raw = ' \n{"n":922337203685477580812345,"snow":"雪🦊"} \t';
const id = (n: number) => `00000000-0000-0000-0000-${String(n).padStart(12, "0")}`;
const db = openSQLite(process.argv[3]);
const revision = db.query<{ revision_id: string }, []>("SELECT revision_id FROM apply_jobs LIMIT 1").get()!.revision_id;
let cases = 0;
function rows() { return db.query("SELECT * FROM apply_jobs ORDER BY id").all(); }
function insert(n = 1, status = "validating", claimed: string | null = "0") {
  db.query(`INSERT INTO apply_jobs(id,revision_id,target,status,source,correlation_id,created_at,
    claimed_at,started_at,finished_at,validation_output,apply_output,health_output,error_message,
    source_primary_id,source_node_id,source_revision_id,snapshot_content_hash,snapshot_version,
    replication_version,leadership_generation)
    VALUES(?,?,'combined',?,'sync',?,'0',?,'2','0',?,'null',NULL,'old',?,?,?,'',-2147483648,
      2147483647,9223372036854775807)`)
    .run(id(n), revision, status, `correlation-${n}`, claimed, raw, id(50), id(51), id(52));
}
function test(name: string, body: () => void) {
  db.exec("DELETE FROM apply_jobs");
  body();
  assert.equal(db.inTransaction, false);
  new SQLiteWriter(db, () => now).write(() => {});
  assert.equal(db.query<{ n: bigint }, []>("SELECT 42 AS n").get()!.n, 42n);
  cases++;
  console.log(`PASS ${name}`);
}
function fails(body: () => unknown, pattern?: RegExp) {
  const before = rows();
  if (pattern === undefined) assert.throws(body);
  else assert.throws(body, pattern);
  assert.deepEqual(rows(), before);
  assert.equal(db.inTransaction, false);
  const reopened = openSQLite(process.argv[3]);
  try { assert.deepEqual(reopened.query("SELECT * FROM apply_jobs ORDER BY id").all(), before); }
  finally { reopened.close(); }
}
function unit() {
  test("empty", () => assert.equal(recoverStaleSQLiteJobs(db, { now }), 0n));
  for (const status of ["queued", "validating", "applying", "applied", "failed", "rolled-back"]) {
    for (const claimed of [null, "0", "2", stamp(3n, 455), stamp(3n, 456), stamp(3n, 457)]) {
      test(`strict-cutoff-${status}-${claimed}`, () => {
        insert(1, status, claimed);
        const before = rows()[0] as Record<string, unknown>;
        const eligible = ["validating", "applying"].includes(status) && claimed !== null && claimed < stamp(3n, 456);
        assert.equal(recoverStaleSQLiteJobs(db, { now }), eligible ? 1n : 0n);
        assert.deepEqual(rows()[0], eligible ? { ...before, status: "queued", claimed_at: null,
          started_at: null, error_message: "Recovered after worker lease expiration" } : before);
        assert.equal(recoverStaleSQLiteJobs(db, { now }), 0n);
      });
    }
  }
  for (const output of [null, "null", raw]) {
    for (const leadership of [-9223372036854775808n, 9007199254740993n, null]) {
      test(`metadata-${output}-${leadership}`, () => {
        insert();
        db.query("UPDATE apply_jobs SET validation_output=?,leadership_generation=?").run(output, leadership);
        const before = rows()[0] as Record<string, unknown>;
        assert.equal(recoverStaleSQLiteJobs(db, { now }), 1n);
        assert.deepEqual(rows()[0], { ...before, status: "queued", claimed_at: null,
          started_at: null, error_message: "Recovered after worker lease expiration" });
      });
    }
  }
  for (const source of ["ordinary", "import", "sync"]) {
    for (const historical of [null, "0", "2", stamp(-9223372036854775808n), stamp(9223372036854775807n, 999999999)]) {
      test(`source-historical-${source}-${historical}`, () => {
        insert();
        db.query(`INSERT OR IGNORE INTO users(id,username,password_hash,role,created_at,updated_at)
          VALUES(?,'recovery-fixture','opaque','owner','0','2')`).run(id(99));
        db.query(`UPDATE apply_jobs SET source=?,actor_user_id=?,started_at=?,finished_at=?`)
          .run(source, id(99), historical, historical);
        const before = rows()[0] as Record<string, unknown>;
        assert.equal(recoverStaleSQLiteJobs(db, { now }), 1n);
        assert.deepEqual(rows()[0], { ...before, status: "queued", claimed_at: null,
          started_at: null, error_message: "Recovered after worker lease expiration" });
      });
    }
  }
  test("many-repeat-reopen", () => {
    insert(1); insert(2, "applying"); insert(3, "applied");
    assert.equal(recoverStaleSQLiteJobs(db, { now }), 2n);
    assert.equal(recoverStaleSQLiteJobs(db, { now }), 0n);
    const reopened = openSQLite(process.argv[3]);
    try { assert.deepEqual(reopened.query("SELECT * FROM apply_jobs ORDER BY id").all(), rows()); }
    finally { reopened.close(); }
  });
  test("default-clock-default-lease", () => {
    insert(1, "validating", stamp(BigInt(Math.floor(Date.now() / 1000)) - 121n));
    insert(2, "applying", "2");
    assert.equal(recoverStaleSQLiteJobs(db), 1n);
  });
  const leaseCases: [string, Finite, number, string, string][] = [
    ["zero", now, 0, stamp(123n, 455), stamp(123n, 456)],
    ["minimum-int64", { kind: "finite", seconds: -9223372036854775808n, nanos: 1 }, 0,
      stamp(-9223372036854775808n), stamp(-9223372036854775808n, 1)],
    ["maximum-int64", { kind: "finite", seconds: 9223372036854775807n, nanos: 999999999 }, 0,
      stamp(9223372036854775807n, 999999998), stamp(9223372036854775807n, 999999999)],
    ["negative", now, -1, stamp(123n, 1000455), stamp(123n, 1000456)],
    ["negative-epoch-floor", { kind: "finite", seconds: 0n, nanos: 456 }, 1, stamp(-1n, 999000455), stamp(-1n, 999000456)],
    ["positive-safe-endpoint", now, Number.MAX_SAFE_INTEGER, stamp(-9007199254618n, 9000455), stamp(-9007199254618n, 9000456)],
    ["negative-safe-endpoint", now, -Number.MAX_SAFE_INTEGER, stamp(9007199254863n, 991000455), stamp(9007199254863n, 991000456)],
    ["int64-above-number", { kind: "finite", seconds: 9007199254740993n, nanos: 456 }, 1,
      stamp(9007199254740992n, 999000455), stamp(9007199254740992n, 999000456)],
  ];
  for (const [name, time, leaseMs, before, equal] of leaseCases) {
    test(name, () => {
      insert(1, "validating", before); insert(2, "applying", equal);
      assert.equal(recoverStaleSQLiteJobs(db, { now: time, leaseMs }), 1n);
      assert.equal((rows()[1] as Record<string, unknown>).status, "applying");
    });
  }
  for (const leaseMs of [0.5, NaN, Infinity, -Infinity, Number.MAX_SAFE_INTEGER + 1, -Number.MAX_SAFE_INTEGER - 1,
    null, "1", 1n] as unknown as number[]) {
    test(`invalid-lease-${leaseMs}`, () => {
      insert(); fails(() => recoverStaleSQLiteJobs(db, { now, leaseMs }));
    });
  }
  for (const time of [
    { kind: "positive-infinity" }, { kind: "null" }, { kind: "finite", seconds: 0, nanos: 0 },
    { ...now, seconds: 1n << 63n }, { ...now, seconds: -(1n << 63n) - 1n },
    { ...now, nanos: -1 }, { ...now, nanos: 1000000000 }, { ...now, nanos: 0.5 },
    { ...now, nanos: NaN }, { ...now, extra: true },
  ]) {
    test(`invalid-now-${cases}`, () => {
      insert(); fails(() => recoverStaleSQLiteJobs(db, { now: time as Finite }));
    });
  }
  for (const [seconds, nanos, leaseMs] of [[-(1n << 63n), 0, 1], [(1n << 63n) - 1n, 999999999, -1]] as const) {
    test(`cutoff-overflow-${leaseMs}`, () => {
      insert(); fails(() => recoverStaleSQLiteJobs(db, { now: { kind: "finite", seconds, nanos }, leaseMs }));
    });
  }
  test("explicit-clock-captured-once", () => {
    insert(1, "validating", stamp(3n, 455));
    insert(2, "applying", stamp(3n, 456));
    let reads = 0;
    assert.equal(recoverStaleSQLiteJobs(db, { get now() {
      return reads++ === 0 ? now : { ...now, seconds: 300n };
    } }), 1n);
    assert.equal(reads, 1);
    assert.equal((rows()[1] as Record<string, unknown>).status, "applying");
  });
  for (const boundary of ["before-acquire", "before-update", "after-count"] as const) {
    test(`cancel-${boundary}`, () => {
      insert(); const controller = new AbortController();
      if (boundary === "before-acquire") controller.abort();
      let checks = 0;
      fails(() => recoverStaleSQLiteJobs(db, { now, get signal() {
        checks++;
        if (checks === (boundary === "before-update" ? 2 : 3)) controller.abort();
        return controller.signal;
      } }));
      if (boundary === "after-count") {
        assert.equal(db.query<{ n: bigint }, []>("SELECT changes() AS n").get()!.n, 1n);
      }
    });
  }
  test("nested-write", () => {
    insert(); const before = rows();
    new SQLiteWriter(db, () => now).write(() => assert.throws(() => recoverStaleSQLiteJobs(db, { now }), /Nested/));
    assert.deepEqual(rows(), before);
  });
  test("constraint-rollback", () => {
    insert();
    db.exec("CREATE TEMP TRIGGER recovery_fail BEFORE UPDATE ON main.apply_jobs BEGIN SELECT RAISE(ABORT,'fixture-update'); END");
    try { fails(() => recoverStaleSQLiteJobs(db, { now }), /fixture-update/); }
    finally { db.exec("DROP TRIGGER temp.recovery_fail"); }
    assert.equal(recoverStaleSQLiteJobs(db, { now }), 1n);
  });
  test("real-deferred-commit-failure", () => {
    insert();
    db.exec(`CREATE TABLE main.recovery_guard(job_id TEXT REFERENCES apply_jobs(id) DEFERRABLE INITIALLY DEFERRED);
      CREATE TEMP TRIGGER recovery_guard_insert AFTER UPDATE ON main.apply_jobs BEGIN
        INSERT INTO recovery_guard VALUES('missing-fixture-job'); END`);
    try {
      fails(() => recoverStaleSQLiteJobs(db, { now }), /FOREIGN KEY/);
      // changes() survives transaction rollback: UPDATE actually changed a row
      // before the deferred FK was checked by the real COMMIT.
      assert.equal(db.query<{ n: bigint }, []>("SELECT changes() AS n").get()!.n, 1n);
      assert.equal(db.query<{ n: bigint }, []>("SELECT count(*) AS n FROM recovery_guard").get()!.n, 0n);
    } finally { db.exec("DROP TRIGGER temp.recovery_guard_insert; DROP TABLE main.recovery_guard"); }
    assert.equal(recoverStaleSQLiteJobs(db, { now }), 1n);
  });
  test("numeric-changes-projection-rollback", () => {
    insert();
    const unsafe = new Database(process.argv[3], { strict: true });
    try { fails(() => recoverStaleSQLiteJobs(unsafe, { now }), /bigint/); }
    finally { unsafe.close(); }
    assert.equal(recoverStaleSQLiteJobs(db, { now }), 1n);
  });
  test("own-statements-not-caller-cached", () => {
    insert(); const cached = db.query<{ count: bigint }, []>("SELECT changes() AS count");
    assert.equal(recoverStaleSQLiteJobs(db, { now }), 1n);
    assert.equal(cached.get()!.count, 1n);
    db.exec("ALTER TABLE apply_jobs ADD COLUMN fixture_only TEXT");
    db.exec("ALTER TABLE apply_jobs DROP COLUMN fixture_only");
    assert.equal(recoverStaleSQLiteJobs(db, { now }), 0n);
  });
  test("sql-error-cleanup", () => {
    insert(); db.exec("ALTER TABLE apply_jobs RENAME TO fixture_hidden_jobs");
    try { assert.throws(() => recoverStaleSQLiteJobs(db, { now }), /apply_jobs/); }
    finally { db.exec("ALTER TABLE fixture_hidden_jobs RENAME TO apply_jobs"); }
    assert.equal(recoverStaleSQLiteJobs(db, { now }), 1n);
  });
  test("closed-connection", () => {
    const closed = openSQLite(process.argv[3]); closed.close();
    assert.throws(() => recoverStaleSQLiteJobs(closed, { now }));
  });
  console.log(`CASES ${cases}`);
}

async function child() {
  db.exec("PRAGMA busy_timeout=150");
  const input = createInterface({ input: process.stdin });
  console.log("READY");
  try {
    for await (const command of input) {
      try {
        switch (command) {
          case "hold": db.exec("BEGIN IMMEDIATE"); console.log("HELD"); break;
          case "release": db.exec("COMMIT"); console.log("RELEASED"); break;
          case "claim-old":
          case "claim-new": {
            const clock = () => command === "claim-old" ? now : { ...now, seconds: 300n };
            const result = claimNextSQLiteJob(db, { clock });
            console.log(result ? `OWNED ${result.id}` : "EMPTY"); break;
          }
          case "recover": console.log(`COUNT ${recoverStaleSQLiteJobs(db, { now: { ...now, seconds: 300n } })}`); break;
          case "exit": return;
          default: throw new Error("unknown recovery fixture command");
        }
      } catch (error) {
        if ((error as { code?: string }).code === "SQLITE_BUSY") console.log("BUSY");
        else throw error;
      }
    }
  } finally { input.close(); }
}
try { if (process.argv[2] === "unit") unit(); else await child(); }
finally { if (db.inTransaction) db.exec("ROLLBACK"); db.close(); }
