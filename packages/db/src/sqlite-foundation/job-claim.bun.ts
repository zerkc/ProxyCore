import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import { createInterface } from "node:readline";
import { resolve } from "node:path";
import { openSQLite } from "./open.js";
import { migrateSQLite } from "./migrate.js";
import { SQLiteWriter } from "./write.js";
import { encodeTimestamp, decodeTimestamp } from "./timestamp.js";
import { claimNextSQLiteJob, type SQLiteClaimOptions } from "./job-claim.js";

const clock = () => ({ kind: "finite" as const, seconds: 123n, nanos: 456 });
const stamp = encodeTimestamp(clock());
const id = (n: number) => `00000000-0000-0000-0000-${String(n).padStart(12, "0")}`;
const raw = ' \n{"n":922337203685477580812345,"snow":"雪🦊"} \t';
let cases = 0;
function test(name: string, body: (db: ReturnType<typeof openSQLite>, path: string) => void): void {
  const path = resolve(`claim-${cases++}.sqlite`);
  const db = openSQLite(path);
  try {
    migrateSQLite(db);
    db.exec(`INSERT INTO config_revisions(id,revision_number,checksum,snapshot,created_at)
      VALUES('${id(1)}',1,'opaque','null','0')`);
    body(db, path);
    assert.equal(db.inTransaction, false);
    const cached = db.query<{ n: bigint }, []>("SELECT 42 AS n");
    assert.equal(cached.get()!.n, 42n);
    new SQLiteWriter(db, clock).write(() => {});
    console.log(`PASS ${name}`);
  } finally {
    db.close();
  }
}
function insert(db: ReturnType<typeof openSQLite>, n = 2, target = "combined", status = "queued", source = "ordinary", time = "0"): void {
  db.query(`INSERT INTO apply_jobs(id,revision_id,target,status,source,correlation_id,created_at)
    VALUES(?,?,?,?,?,?,?)`).run(id(n), id(1), target, status, source, `correlation-${n}`, time);
}
function row(db: ReturnType<typeof openSQLite>, n = 2) {
  return db.query<Record<string, unknown>, [string]>("SELECT * FROM apply_jobs WHERE id=?").get(id(n));
}
function rollbackError(db: ReturnType<typeof openSQLite>, pattern: RegExp, options: SQLiteClaimOptions = { clock }): void {
  const before = db.query("SELECT * FROM apply_jobs ORDER BY id").all();
  assert.throws(() => claimNextSQLiteJob(db, options), pattern);
  assert.equal(db.inTransaction, false);
  assert.deepEqual(db.query("SELECT * FROM apply_jobs ORDER BY id").all(), before);
}

async function unit(): Promise<void> {
  test("empty", (db) => assert.equal(claimNextSQLiteJob(db, { clock }), undefined));
  for (const target of ["coredns", "nginx", "combined", "certificate"] as const) {
    test(`target-${target}`, (db) => {
      insert(db, 2, target);
      insert(db, 3, target === "nginx" ? "coredns" : "nginx");
      assert.equal(claimNextSQLiteJob(db, { target, clock })!.id, id(2));
      assert.equal(claimNextSQLiteJob(db, { target, clock }), undefined);
      assert.equal(row(db, 3)!.status, "queued");
    });
  }
  for (const source of ["ordinary", "import", "sync"]) {
    test(`source-${source}`, (db) => {
      insert(db, 2, "combined", "queued", source);
      assert.equal(claimNextSQLiteJob(db, { clock })!.source, source);
    });
  }
  for (const status of ["validating", "applying", "applied", "failed", "rolled-back"]) {
    test(`excluded-${status}`, (db) => {
      insert(db, 2, "combined", status);
      const before = row(db);
      assert.equal(claimNextSQLiteJob(db, { clock }), undefined);
      assert.deepEqual(row(db), before);
    });
  }
  test("created-order-no-active-guard", (db) => {
    insert(db, 2, "combined", "queued", "ordinary", "2");
    insert(db, 3, "nginx", "queued", "sync", stamp!);
    insert(db, 4, "certificate", "queued", "import", "0");
    insert(db, 5, "combined", "applying");
    assert.equal(claimNextSQLiteJob(db, { clock })!.id, id(4));
    assert.equal(claimNextSQLiteJob(db, { clock })!.id, id(3));
    assert.equal(claimNextSQLiteJob(db, { clock })!.id, id(2));
    assert.equal(claimNextSQLiteJob(db, { clock }), undefined);
  });
  for (const started of [null, "0", "2", "100000000000000000000.000000000", "118446744073709551615.999999999", stamp]) {
    for (const leadership of [-9223372036854775808n, 9223372036854775807n, 9007199254740993n, null]) {
      test(`raw-${started}-${leadership}`, (db, path) => {
        insert(db);
        db.query(`INSERT INTO users(id,username,password_hash,role,created_at,updated_at)
          VALUES(?,'fixture','opaque','owner','0','2')`).run(id(9));
        db.query(`UPDATE apply_jobs SET actor_user_id=?,source='sync',source_primary_id=?,source_node_id=?,
          source_revision_id=?,snapshot_content_hash='',snapshot_version=-2147483648,replication_version=2147483647,
          leadership_generation=?,validation_output=?,apply_output='null',health_output=NULL,error_message='retained',
          claimed_at='2',started_at=?,finished_at='0'`).run(id(9), id(7), id(8), id(6), leadership, raw, started);
        const before = row(db)!;
        let calls = 0;
        const got = claimNextSQLiteJob(db, { clock: () => { calls++; return clock(); } });
        assert.equal(calls, 1);
        assert.deepEqual(got, { ...before, status: "validating", claimed_at: stamp, started_at: started ?? stamp });
        assert.equal(got!.leadership_generation, leadership);
        assert.equal(got!.validation_output, raw);
        assert.equal(got!.apply_output, "null");
        assert.equal(got!.health_output, null);
        assert(Object.isFrozen(got));
        const reopened = openSQLite(path);
        try { assert.deepEqual(row(reopened), got); } finally { reopened.close(); }
      });
    }
  }
  test("nullable-defaults-and-cached-cursor", (db) => {
    insert(db);
    const cached = db.query<{ id: string }, []>("SELECT id FROM apply_jobs");
    const before = row(db)!;
    const got = claimNextSQLiteJob(db, { clock });
    assert.deepEqual(got, { ...before, status: "validating", claimed_at: stamp, started_at: stamp });
    assert.equal(cached.get()!.id, id(2));
    assert.equal(got!.actor_user_id, null);
    assert.equal(got!.snapshot_version, null);
  });
  for (const seconds of [-(1n << 63n), (1n << 63n) - 1n]) {
    test(`finite-clock-${seconds}`, (db) => {
      insert(db);
      const value = { kind: "finite" as const, seconds, nanos: 999999999 };
      assert.equal(claimNextSQLiteJob(db, { clock: () => value })!.claimed_at, encodeTimestamp(value));
    });
  }
  const badClocks = [
    { kind: "positive-infinity" }, { kind: "negative-infinity" }, { kind: "null" },
    { kind: "finite", seconds: 1, nanos: 0 }, { kind: "finite", seconds: 1n << 63n, nanos: 0 },
    { kind: "finite", seconds: -(1n << 63n) - 1n, nanos: 0 },
    { kind: "finite", seconds: 0n, nanos: -1 }, { kind: "finite", seconds: 0n, nanos: 1000000000 },
    { kind: "finite", seconds: 0n, nanos: NaN }, { kind: "finite", seconds: 0n, nanos: 0.5 },
    { kind: "finite", seconds: 0n, nanos: Infinity }, { kind: "finite", seconds: 0n, nanos: 0, extra: true },
  ];
  for (const [index, value] of badClocks.entries()) {
    test(`bad-clock-${index}`, (db) => {
      insert(db);
      rollbackError(db, /clock|Timestamp|timestamp/, { clock: (() => value) as SQLiteClaimOptions["clock"] });
    });
  }
  test("bad-target", (db) => {
    insert(db);
    rollbackError(db, /target/, { target: "other" as SQLiteClaimOptions["target"] });
  });
  test("abort-before-acquisition", (db) => {
    insert(db);
    const signal = AbortSignal.abort(new Error("preabort"));
    let called = false;
    rollbackError(db, /preabort/, { signal, clock: () => { called = true; return clock(); } });
    assert.equal(called, false);
  });
  test("abort-clock-callback", (db) => {
    insert(db);
    const controller = new AbortController();
    rollbackError(db, /boundary/, { signal: controller.signal, clock: () => {
      controller.abort(new Error("boundary"));
      return clock();
    } });
    assert.equal(claimNextSQLiteJob(db, { clock })!.id, id(2));
  });
  test("nested-rejected", (db) => {
    insert(db);
    db.exec("BEGIN IMMEDIATE");
    try {
      assert.throws(() => claimNextSQLiteJob(db, { clock }), /Nested/);
      assert.equal(db.inTransaction, true);
      assert.equal(row(db)!.status, "queued");
    } finally { db.exec("ROLLBACK"); }
    assert.equal(claimNextSQLiteJob(db, { clock })!.id, id(2));
  });
  for (const column of ["revision_id", "actor_user_id"]) {
    test(`missing-reference-${column}`, (db, path) => {
      insert(db);
      db.exec("PRAGMA foreign_keys=OFF");
      try { db.query(`UPDATE apply_jobs SET ${column}=?`).run(id(99)); }
      finally { db.exec("PRAGMA foreign_keys=ON"); }
      rollbackError(db, /foreign-key/);
      const reopened = openSQLite(path);
      try { assert.equal(row(reopened)!.status, "queued"); } finally { reopened.close(); }
    });
  }
  const corruptions = ["target='other'", "source='other'", "id='bad'", "created_at='bad'",
    "started_at='bad'", "finished_at='bad'", "validation_output='{'", "snapshot_version=2147483648",
    "replication_version=-2147483649", "error_message=char(0)", "source_node_id='bad'"];
  for (const [index, assignment] of corruptions.entries()) {
    test(`malformed-selected-${index}`, (db) => {
      insert(db);
      db.exec("PRAGMA ignore_check_constraints=ON");
      try { db.exec(`UPDATE apply_jobs SET ${assignment}`); }
      finally { db.exec("PRAGMA ignore_check_constraints=OFF"); }
      rollbackError(db, /Invalid|timestamp|JSON|Unexpected|constraint/i);
    });
  }
  test("update-failure", (db) => {
    insert(db);
    db.exec("CREATE TEMP TRIGGER reject_claim BEFORE UPDATE ON main.apply_jobs BEGIN SELECT RAISE(ABORT,'fixture update failure'); END");
    try { rollbackError(db, /fixture update failure/); }
    finally { db.exec("DROP TRIGGER temp.reject_claim"); }
    assert.equal(claimNextSQLiteJob(db, { clock })!.id, id(2));
  });
  for (const assignment of ["validation_output='{'", "snapshot_version=2147483648", "finished_at='bad'", "source_node_id='bad'"]) {
    test(`malformed-returning-${assignment}`, (db) => {
      insert(db);
      db.exec("PRAGMA ignore_check_constraints=ON");
      db.exec(`CREATE TEMP TRIGGER corrupt_return BEFORE UPDATE OF status ON main.apply_jobs
        BEGIN UPDATE apply_jobs SET ${assignment} WHERE id=NEW.id; END`);
      try { rollbackError(db, /JSON|Unexpected|Invalid|timestamp/i); }
      finally {
        db.exec("DROP TRIGGER temp.corrupt_return; PRAGMA ignore_check_constraints=OFF");
      }
      assert.equal(claimNextSQLiteJob(db, { clock })!.id, id(2));
    });
  }
  test("real-deferred-commit-failure", (db, path) => {
    insert(db);
    // SQLite total_changes includes rolled-back writes, unlike row visibility.
    // The delta proves both UPDATE and AFTER-trigger INSERT really executed.
    const changes = () => db.query<{ n: bigint }, []>("SELECT total_changes() AS n").get()!.n;
    const beforeChanges = changes();
    db.exec(`CREATE TABLE claim_guard(job_id TEXT, user_id TEXT REFERENCES users(id) DEFERRABLE INITIALLY DEFERRED);
      CREATE TEMP TRIGGER claim_commit_guard AFTER UPDATE OF status ON main.apply_jobs
      BEGIN INSERT INTO claim_guard VALUES(NEW.id,'${id(99)}'); END`);
    let published: ReturnType<typeof claimNextSQLiteJob>;
    assert.throws(() => { published = claimNextSQLiteJob(db, { clock }); }, /FOREIGN KEY/i);
    assert.equal(published, undefined);
    assert.equal(changes() - beforeChanges, 2n, "UPDATE and guard INSERT ran before deferred COMMIT failure");
    assert.equal(db.inTransaction, false);
    assert.equal(row(db)!.status, "queued");
    assert.equal(db.query<{ n: bigint }, []>("SELECT count(*) AS n FROM claim_guard").get()!.n, 0n);
    const reopened = openSQLite(path);
    try { assert.equal(row(reopened)!.status, "queued"); } finally { reopened.close(); }
    db.exec("DROP TRIGGER temp.claim_commit_guard; DROP TABLE main.claim_guard");
    assert.equal(claimNextSQLiteJob(db, { clock })!.id, id(2));
  });
  test("update-type-error-rollback", (db) => {
    insert(db);
    db.exec(`CREATE TEMP TRIGGER wrong_type BEFORE UPDATE OF status ON main.apply_jobs
      BEGIN UPDATE apply_jobs SET correlation_id=x'FF' WHERE id=NEW.id; END`);
    try { rollbackError(db, /cannot store BLOB|constraint/i); }
    finally { db.exec("DROP TRIGGER temp.wrong_type"); }
    assert.equal(claimNextSQLiteJob(db, { clock })!.id, id(2));
  });
  test("ignored-update-is-error-not-empty", (db) => {
    insert(db);
    db.exec("CREATE TEMP TRIGGER ignored_claim BEFORE UPDATE ON main.apply_jobs BEGIN SELECT RAISE(IGNORE); END");
    try { rollbackError(db, /lost its selected row/); }
    finally { db.exec("DROP TRIGGER temp.ignored_claim"); }
    assert.equal(claimNextSQLiteJob(db, { clock })!.id, id(2));
  });
  test("unsafe-integer-projection-rejected", (db, path) => {
    insert(db);
    db.query("UPDATE apply_jobs SET leadership_generation=?").run(9223372036854775807n);
    const unsafe = new Database(path, { strict: true, safeIntegers: false });
    try {
      rollbackError(unsafe, /Invalid job integer/);
      assert.equal(row(db)!.leadership_generation, 9223372036854775807n);
    } finally { unsafe.close(); }
    assert.equal(claimNextSQLiteJob(db, { clock })!.leadership_generation, 9223372036854775807n);
  });
  test("default-clock", (db) => {
    insert(db);
    const got = claimNextSQLiteJob(db)!;
    assert.equal(decodeTimestamp(got.claimed_at).kind, "finite");
    assert.equal(got.started_at, got.claimed_at);
  });
  test("throwing-clock", (db) => {
    insert(db);
    rollbackError(db, /clock failure/, { clock: () => { throw new Error("clock failure"); } });
  });
  test("closed-database-error", (db, path) => {
    const closed = openSQLite(path);
    closed.close();
    assert.throws(() => claimNextSQLiteJob(closed), /closed|finalized|transaction/i);
    assert.equal(claimNextSQLiteJob(db, { clock }), undefined);
  });
  for (const native of [true, false]) {
    test(`rejected-clock-${native ? "promise" : "thenable"}`, (db) => {
      insert(db);
      const rejected = () => native ? Promise.reject(new Error("abandoned fixture clock")) : {
        then(_resolve: unknown, reject: (error: Error) => void) { reject(new Error("abandoned thenable")); },
      };
      rollbackError(db, /Async claim clocks/, { clock: rejected as unknown as SQLiteClaimOptions["clock"] });
      assert.equal(claimNextSQLiteJob(db, { clock })!.id, id(2));
    });
  }
  // Uncontained native/custom rejection would fail this real Bun process.
  await new Promise((resolve) => setTimeout(resolve, 10));
  console.log(`CASES ${cases}`);
}

async function child(path: string): Promise<void> {
  const db = openSQLite(path);
  db.exec("PRAGMA busy_timeout=150");
  const input = createInterface({ input: process.stdin });
  console.log("READY");
  try {
    for await (const command of input) {
      if (command === "exit") break;
      if (command === "hold") {
        db.exec("BEGIN IMMEDIATE");
        console.log("HELD");
      } else if (command === "release") {
        db.exec("ROLLBACK");
        console.log("RELEASED");
      } else if (command === "claim") {
        try {
          const claimed = claimNextSQLiteJob(db, { clock });
          console.log(claimed ? `OWNED ${claimed.id}` : "EMPTY");
        } catch (error) {
          if ((error as { code?: string }).code !== "SQLITE_BUSY") throw error;
          console.log("BUSY");
        }
      } else throw new Error("Unknown fixture command");
    }
  } finally {
    input.close();
    if (db.inTransaction) db.exec("ROLLBACK");
    db.close();
  }
}

if (process.argv[2] === "unit") await unit();
else if (process.argv[2]) await child(process.argv[2]);
else throw new Error("Missing fixture mode/path");
