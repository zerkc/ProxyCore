import assert from "node:assert/strict";
import { Database, type SQLQueryBindings } from "bun:sqlite";
import { createInterface } from "node:readline";
import { openSQLite } from "./open.js";
import { encodeTimestamp } from "./timestamp.js";
import { getSQLiteJobRow } from "./job-read.js";
import { getSQLiteJobRecord, projectSQLiteJobRecord } from "./job-record.js";
import { claimNextSQLiteJobRecord } from "./job-claim-record.js";
import type { JobRecord, JobStore } from "../ports.js";

type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2)
      ? true : false
    : false;
type Expect<T extends true> = T;
type CanonicalParameters = Expect<Identical<Parameters<typeof claimNextSQLiteJobRecord>,
  [db: Database, target?: Parameters<JobStore["claimNext"]>[0]]>>;
type CanonicalReturn = Expect<Identical<ReturnType<typeof claimNextSQLiteJobRecord>,
  ReturnType<JobStore["claimNext"]>>>;
type CanonicalMapper = Expect<Identical<ReturnType<typeof projectSQLiteJobRecord>, JobRecord>>;

const [mode, path, originalID, originalRevision, originalCorrelation] = process.argv.slice(2);
const db = openSQLite(path);
const id = (n: number) => `aaaaaaaa-aaaa-aaaa-aaaa-${String(n).padStart(12, "0")}`;
const stamp = (seconds: bigint, nanos = 0) => encodeTimestamp({ kind: "finite", seconds, nanos })!;
const outputFields = ["validation_output", "apply_output", "health_output"] as const;
const outputKeys = ["validationOutput", "applyOutput", "healthOutput"] as const;
const nullableKeys = ["actorUserId", "sourcePrimaryId", "sourceNodeId", "sourceRevisionId",
  "snapshotContentHash", "snapshotVersion", "replicationVersion", "leadershipGeneration",
  "finishedAt", ...outputKeys, "errorMessage"];
const allKeys = ["id", "revisionId", "target", "status", "source", "correlationId", "createdAt",
  "claimedAt", "startedAt", ...nullableKeys];
let cases = 0;
function rows() { return db.query("SELECT * FROM main.apply_jobs ORDER BY id").all(); }
function raw(n = 1) { return getSQLiteJobRow(db, id(n))!; }
function insert(n = 1, target = "combined", status = "queued", source = "ordinary", time = stamp(0n)) {
  db.query(`INSERT INTO main.apply_jobs(id,revision_id,target,status,source,correlation_id,created_at)
    VALUES(?,?,?,?,?,?,?)`).run(id(n), originalRevision, target, status, source, `correlation-${n}`, time);
}
function update(column: string, value: string | bigint | null) {
  db.query(`UPDATE main.apply_jobs SET ${column}=? WHERE id=?`).run(value, id(1));
}
async function check(name: string, body: () => void | Promise<void>) {
  await body(); cases++; console.log(`PASS ${name}`);
}
async function test(name: string, body: () => void | Promise<void>) {
  const before = rows();
  const revisions = db.query("SELECT * FROM main.config_revisions ORDER BY id").all();
  await check(name, async () => {
    try {
      await body();
      assert.equal(db.inTransaction, false);
      assert.deepEqual(db.query("SELECT * FROM main.config_revisions ORDER BY id").all(), revisions);
    } finally {
      if (db.inTransaction) db.exec("ROLLBACK");
      db.query("DELETE FROM main.apply_jobs WHERE id != ?").run(originalID);
    }
    assert.deepEqual(rows(), before);
  });
}
async function claim(connection = db, target?: Parameters<JobStore["claimNext"]>[0]) {
  const promise = claimNextSQLiteJobRecord(connection, target);
  assert(promise instanceof Promise, "public result/rejection is always a Promise");
  const record = await promise;
  if (record) assert.deepEqual(Object.keys(record).sort(), [...allKeys].sort());
  return record;
}
async function rollbackError(pattern: RegExp, connection = db, target?: Parameters<JobStore["claimNext"]>[0]) {
  const before = rows();
  await assert.rejects(claim(connection, target), pattern);
  assert.equal(db.inTransaction, false);
  assert.deepEqual(rows(), before);
}

// Test-local database facade. It delegates real BEGIN/UPDATE/COMMIT to SQLite,
// instruments only returned row access, and forbids a reader/query fallback.
// No production injection surface, module mocking, or mutable global hook.
function facade(options: { projectionError?: Error; cancelled?: boolean } = {}) {
  let updates = 0, rowReads = 0;
  const connection = {
    get inTransaction() { return db.inTransaction; },
    transaction: db.transaction.bind(db),
    query() { throw new Error("public claim must not reread via query"); },
    prepare(sql: string) {
      if (sql.startsWith("SELECT *")) rowReads++;
      assert(!(sql.startsWith("SELECT *") && sql.includes("WHERE id = ?")), "no native job reread");
      const statement = db.prepare<Record<string, unknown>, SQLQueryBindings[]>(sql);
      return {
        get(...bindings: SQLQueryBindings[]) {
          if (options.cancelled) throw new Error("fixture cancellation at native statement boundary");
          const value = statement.get(...bindings);
          if (!sql.startsWith("UPDATE")) return value;
          updates++;
          if (!options.projectionError || !value) return value;
          return new Proxy(value, {
            get(target, key, receiver) {
              if (!db.inTransaction && key === "created_at") throw options.projectionError;
              return Reflect.get(target, key, receiver);
            },
          });
        },
        finalize() { statement.finalize(); },
      };
    },
  } as unknown as Database;
  return { connection, counts: () => [rowReads, updates] };
}

async function unit() {
  const original = getSQLiteJobRow(db, originalID)!;
  await check("Go-enqueue-public-Promise-commit-and-reopen", async () => {
    const got = (await claim())!;
    assert.equal(got.id, originalID);
    assert.equal(got.revisionId, originalRevision);
    assert.equal(got.correlationId, originalCorrelation);
    assert.equal(got.status, "validating");
    assert.equal(got.createdAt.getTime(), 123000);
    assert(got.claimedAt instanceof Date);
    assert.equal(got.claimedAt.getTime(), got.startedAt!.getTime());
    assert.deepEqual(got, projectSQLiteJobRecord(getSQLiteJobRow(db, originalID)!));
    const reopened = openSQLite(path);
    try { assert.deepEqual(getSQLiteJobRecord(reopened, originalID), got); }
    finally { reopened.close(); }
    assert.deepEqual(getSQLiteJobRow(db, originalID), {
      ...original, status: "validating", claimed_at: getSQLiteJobRow(db, originalID)!.claimed_at,
      started_at: getSQLiteJobRow(db, originalID)!.claimed_at,
    });
  });
  await test("empty-repeat", async () => {
    assert.equal(await claim(), undefined);
    assert.equal(await claim(), undefined);
  });
  for (const target of ["coredns", "nginx", "combined", "certificate"] as const) {
    await test(`target-${target}`, async () => {
      insert(1, target); insert(2, target === "nginx" ? "coredns" : "nginx");
      assert.equal((await claim(db, target))!.id, id(1));
      assert.equal(await claim(db, target), undefined);
      assert.equal(raw(2).status, "queued");
    });
  }
  for (const source of ["ordinary", "import", "sync"]) {
    await test(`source-${source}`, async () => {
      insert(1, "combined", "queued", source);
      assert.equal((await claim())!.source, source);
    });
  }
  for (const status of ["validating", "applying", "applied", "failed", "rolled-back"]) {
    await test(`exclude-${status}`, async () => {
      insert(1, "combined", status); const before = raw();
      assert.equal(await claim(), undefined); assert.deepEqual(raw(), before);
    });
  }
  await test("created-ASC-no-source-priority-no-active-exclusion", async () => {
    insert(1, "combined", "queued", "ordinary", "2");
    insert(2, "nginx", "queued", "sync", stamp(1n));
    insert(3, "certificate", "queued", "import", "0");
    insert(4, "combined", "applying");
    for (const n of [3, 2, 1]) assert.equal((await claim())!.id, id(n));
    assert.equal(await claim(), undefined);
  });
  for (const started of [null, "0", "2", stamp(-1n, 999999999), stamp(123n)]) {
    await test(`retained-start-and-metadata-${started}`, async () => {
      insert();
      update("started_at", started); update("claimed_at", "2");
      update("finished_at", "0"); update("error_message", "retained 雪");
      update("source_primary_id", id(7)); update("source_node_id", id(8));
      update("source_revision_id", id(9)); update("snapshot_content_hash", "");
      update("validation_output", ' {"nested":[1]} ');
      const before = raw();
      const got = (await claim())!;
      const after = raw();
      assert.deepEqual(after, { ...before, status: "validating", claimed_at: after.claimed_at,
        started_at: started ?? after.claimed_at });
      assert.deepEqual(got, projectSQLiteJobRecord(after));
      assert.equal(got.errorMessage, "retained 雪");
      assert.equal(got.snapshotContentHash, "");
      assert.equal(got.sourcePrimaryId, id(7));
      assert.equal(got.sourceNodeId, id(8));
      assert.equal(got.sourceRevisionId, id(9));
    });
  }
  await test("SQLNULL-own-undefined-and-fresh-start", async () => {
    insert(); const got = (await claim())!;
    for (const key of nullableKeys) {
      assert(Object.hasOwn(got, key)); assert.equal(got[key as keyof JobRecord], undefined);
    }
    assert(got.claimedAt instanceof Date); assert(got.startedAt instanceof Date);
    assert.notEqual(got.claimedAt, got.startedAt);
  });
  const jsonGoldens: [string | null, unknown][] = [
    [null, undefined], ["null", undefined], ["0", 0], ["false", false], ['""', ""],
    ["42", 42], ["true", true], ['"scalar"', "scalar"], ["[]", []], ["{}", {}],
    [' [null,0,false,"",{"snow":"雪🦊"}] ', [null, 0, false, "", { snow: "雪🦊" }]],
    [' \n{"big":922337203685477580812345} \t', { big: 9.223372036854776e23 }],
    ["1e400", Infinity],
  ];
  for (let i = 0; i < outputFields.length; i++) {
    for (const [value, expected] of jsonGoldens) {
      await test(`JSON-${outputFields[i]}-${value}`, async () => {
        insert(); update(outputFields[i], value);
        assert.deepEqual((await claim())![outputKeys[i]], expected);
        assert.equal(raw()[outputFields[i]], value);
      });
    }
  }
  for (const [column, key] of [["snapshot_version", "snapshotVersion"],
    ["replication_version", "replicationVersion"], ["leadership_generation", "leadershipGeneration"]] as const) {
    const values = column === "leadership_generation"
      ? [null, 0n, -9223372036854775808n, 9223372036854775807n, 9007199254740993n, -9007199254740993n]
      : [null, 0n, -2147483648n, 2147483647n];
    for (const value of values) {
      await test(`integer-${column}-${value}`, async () => {
        insert(); update(column, value);
        assert.equal((await claim())![key], value === null ? undefined : Number(value));
        assert.equal(raw()[column], value);
      });
    }
  }
  const clocks: [string, number][] = [
    [stamp(0n), 0], [stamp(0n, 1), 0], [stamp(0n, 999999), 0], [stamp(0n, 1000000), 1],
    [stamp(-1n, 999999999), -1], [stamp(-1n, 998999999), -2], [stamp(-1n, 1), -1000],
    [stamp(1n, 999999999), 1999], [stamp(-62167219200n), -62167219200000],
    [stamp(253402300800n), 253402300800000],
    [stamp(8640000000000n), 8640000000000000],
    [stamp(8640000000000n, 999999), 8640000000000000],
    [stamp(8640000000000n, 1000000), NaN],
    [stamp(-8640000000000n), -8640000000000000],
    [stamp(-8640000000000n, 1000000), -8639999999999999],
    [stamp(-8640000000001n, 999999999), NaN],
    [stamp(-9223372036854775808n, 999999999), NaN], [stamp(9223372036854775807n), NaN],
    ["0", NaN], ["2", NaN],
  ];
  for (const [column, key] of [["created_at", "createdAt"], ["started_at", "startedAt"],
    ["finished_at", "finishedAt"]] as const) {
    for (const [value, expected] of clocks) {
      await test(`clock-${column}-${value}`, async () => {
        insert(); update(column, value);
        const date = (await claim())![key]!;
        assert(date instanceof Date); assert.equal(date.getTime(), expected);
        if (Number.isNaN(expected)) assert.throws(() => date.toISOString(), RangeError);
        assert.equal(raw()[column], value);
        // Claimed clock cannot be publicly injected. The extracted mapper seam
        // independently covers the same Date contract for that fourth field.
        assert.equal(projectSQLiteJobRecord({ ...raw(), claimed_at: value }).claimedAt!.getTime(), expected);
      });
    }
  }
  await test("actor-and-nullable-text", async () => {
    db.query(`INSERT INTO main.users(id,username,password_hash,role,created_at,updated_at)
      VALUES(?,'public-claim-fixture','opaque','owner','0','2')`).run(id(98));
    try {
      insert(); update("actor_user_id", id(98));
      update("snapshot_content_hash", "  untouched 雪\n "); update("error_message", "");
      const got = (await claim())!;
      assert.equal(got.actorUserId, id(98));
      assert.equal(got.snapshotContentHash, "  untouched 雪\n "); assert.equal(got.errorMessage, "");
    } finally {
      update("actor_user_id", null);
      db.query("DELETE FROM main.users WHERE id=?").run(id(98));
    }
  });
  await test("cached-caller-statement-remains-usable", async () => {
    insert();
    const cached = db.query<{ id: string }, [string]>("SELECT id FROM main.apply_jobs WHERE id=?");
    assert.equal(cached.get(id(1))!.id, id(1));
    await claim();
    assert.equal(cached.get(id(1))!.id, id(1));
  });
  await test("detached-mutable-Date-and-JSON-isolation", async () => {
    insert(); update("validation_output", '{"nested":[1]}'); update("apply_output", '[{"n":2}]');
    update("health_output", '{"healthy":true}'); update("finished_at", stamp(1n));
    const got = (await claim())!; const before = raw();
    got.createdAt.setTime(42); got.claimedAt!.setTime(43); got.startedAt!.setTime(44); got.finishedAt!.setTime(45);
    (got.validationOutput as { nested: number[] }).nested.push(2);
    (got.applyOutput as { n: number }[])[0].n = 7;
    (got.healthOutput as { healthy: boolean }).healthy = false;
    assert.deepEqual(raw(), before);
    const first = getSQLiteJobRecord(db, id(1))!, second = projectSQLiteJobRecord(before);
    assert.deepEqual(first, second); assert.notEqual(first.createdAt, second.createdAt);
    assert.notEqual(first.validationOutput, second.validationOutput);
    assert.notEqual(first.applyOutput, second.applyOutput); assert.notEqual(first.healthOutput, second.healthOutput);
  });
  await test("exact-returned-row-no-reread", async () => {
    insert(); update("error_message", "returned");
    db.exec(`CREATE TEMP TRIGGER after_return AFTER UPDATE OF status ON main.apply_jobs
      BEGIN UPDATE apply_jobs SET error_message='persisted-after-return' WHERE id=NEW.id; END`);
    try {
      const seam = facade();
      assert.equal((await claim(seam.connection))!.errorMessage, "returned");
      assert.equal(raw().error_message, "persisted-after-return");
      assert.deepEqual(seam.counts(), [1, 1]);
    } finally { db.exec("DROP TRIGGER temp.after_return"); }
  });
  await test("projection-exception-rejects-AFTER-committed-ownership", async () => {
    insert(); const error = new Error("unexpected mapper access failure");
    const seam = facade({ projectionError: error });
    let published: JobRecord | undefined;
    await assert.rejects(async () => { published = await claim(seam.connection); }, (got) => got === error);
    assert.equal(published, undefined); assert.equal(db.inTransaction, false);
    assert.equal(raw().status, "validating");
    const reopened = openSQLite(path);
    try { assert.equal(getSQLiteJobRow(reopened, id(1))!.status, "validating"); }
    finally { reopened.close(); }
    assert.equal(await claim(), undefined, "projection rejection cannot undo native COMMIT");
    assert.deepEqual(seam.counts(), [1, 1]);
  });
  await test("invalid-target-is-Promise-rejection", async () => {
    insert(); await rollbackError(/target/, db, "invalid" as Parameters<JobStore["claimNext"]>[0]);
  });
  await test("native-statement-cancellation-error-not-empty", async () => {
    insert(); await rollbackError(/fixture cancellation/, facade({ cancelled: true }).connection);
    assert.equal((await claim())!.id, id(1));
  });
  for (const assignment of ["target='other'", "source='other'", "created_at='bad'", "started_at='bad'",
    "validation_output='{'", "snapshot_version=2147483648", "replication_version=-2147483649",
    "source_node_id='bad'", "error_message=char(0)"]) {
    await test(`corrupt-row-${assignment}`, async () => {
      insert(); db.exec("PRAGMA ignore_check_constraints=ON");
      try { db.exec(`UPDATE main.apply_jobs SET ${assignment} WHERE id='${id(1)}'`); }
      finally { db.exec("PRAGMA ignore_check_constraints=OFF"); }
      await rollbackError(/Invalid|timestamp|JSON|Unexpected/i);
    });
  }
  for (const column of ["revision_id", "actor_user_id"]) {
    await test(`missing-reference-${column}`, async () => {
      insert(); db.exec("PRAGMA foreign_keys=OFF");
      try { update(column, id(99)); } finally { db.exec("PRAGMA foreign_keys=ON"); }
      await rollbackError(/foreign-key/);
    });
  }
  for (const [trigger, pattern] of [
    ["SELECT RAISE(ABORT,'fixture update failure');", /fixture update failure/],
    ["SELECT RAISE(IGNORE);", /lost its selected row/],
    ["UPDATE apply_jobs SET correlation_id=x'FF' WHERE id=NEW.id;", /BLOB|constraint/i],
  ] as const) {
    await test(`SQL-update-error-${trigger}`, async () => {
      insert(); db.exec(`CREATE TEMP TRIGGER reject_public_claim BEFORE UPDATE ON main.apply_jobs BEGIN ${trigger} END`);
      try { await rollbackError(pattern); }
      finally { db.exec("DROP TRIGGER temp.reject_public_claim"); }
      assert.equal((await claim())!.id, id(1));
    });
  }
  for (const assignment of ["validation_output='{'", "snapshot_version=2147483648", "finished_at='bad'", "source_node_id='bad'"]) {
    await test(`malformed-returned-row-${assignment}`, async () => {
      insert(); db.exec("PRAGMA ignore_check_constraints=ON");
      db.exec(`CREATE TEMP TRIGGER corrupt_public_return BEFORE UPDATE OF status ON main.apply_jobs
        BEGIN UPDATE apply_jobs SET ${assignment} WHERE id=NEW.id; END`);
      try { await rollbackError(/JSON|Unexpected|Invalid|timestamp/i); }
      finally { db.exec("DROP TRIGGER temp.corrupt_public_return; PRAGMA ignore_check_constraints=OFF"); }
      assert.equal((await claim())!.id, id(1));
    });
  }
  await test("nested-Promise-rejection-preserves-caller-transaction", async () => {
    insert(); db.exec("BEGIN IMMEDIATE");
    try {
      await assert.rejects(claim(), /Nested/);
      assert.equal(db.inTransaction, true); assert.equal(raw().status, "queued");
    } finally { db.exec("ROLLBACK"); }
    assert.equal((await claim())!.id, id(1));
  });
  await test("closed-and-unsafe-connections-reject-not-empty", async () => {
    insert(); update("leadership_generation", 9223372036854775807n);
    const unsafe = new Database(path, { strict: true, safeIntegers: false });
    try { await rollbackError(/Invalid job integer/, unsafe); } finally { unsafe.close(); }
    const closed = openSQLite(path); closed.close();
    await rollbackError(/closed|finalized|transaction/i, closed);
    assert.equal((await claim())!.leadershipGeneration, 9223372036854775808);
  });
  await test("actual-deferred-COMMIT-failure-full-rollback-next-success", async () => {
    insert(); const before = rows();
    const changes = () => db.query<{ n: bigint }, []>("SELECT total_changes() AS n").get()!.n;
    const count = changes();
    db.exec(`CREATE TABLE public_claim_guard(job_id TEXT, user_id TEXT REFERENCES users(id) DEFERRABLE INITIALLY DEFERRED);
      CREATE TEMP TRIGGER public_commit_guard AFTER UPDATE OF status ON main.apply_jobs
      BEGIN INSERT INTO public_claim_guard VALUES(NEW.id,'${id(99)}'); END`);
    try {
      let published: JobRecord | undefined;
      await assert.rejects(async () => { published = await claim(); }, /FOREIGN KEY/i);
      assert.equal(published, undefined); assert.equal(changes() - count, 2n);
      assert.equal(db.inTransaction, false); assert.deepEqual(rows(), before);
      assert.equal(db.query<{ n: bigint }, []>("SELECT count(*) AS n FROM public_claim_guard").get()!.n, 0n);
      const reopened = openSQLite(path);
      try { assert.equal(getSQLiteJobRow(reopened, id(1))!.status, "queued"); }
      finally { reopened.close(); }
    } finally { db.exec("DROP TRIGGER temp.public_commit_guard; DROP TABLE main.public_claim_guard"); }
    assert.equal((await claim())!.id, id(1));
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
      if (command === "hold") { db.exec("BEGIN IMMEDIATE"); console.log("HELD"); }
      else if (command === "release") { db.exec("ROLLBACK"); console.log("RELEASED"); }
      else if (command === "claim") {
        try {
          const got = await claim();
          console.log(got ? `OWNED ${got.id}` : "EMPTY");
        } catch (error) {
          if ((error as { code?: string }).code !== "SQLITE_BUSY") throw error;
          console.log("BUSY");
        }
      } else throw new Error("Unknown public fixture command");
    }
  } finally { input.close(); }
}
try {
  if (mode === "unit") await unit();
  else if (mode === "child") await child();
  else throw new Error("Missing public claim fixture mode");
} finally {
  if (db.inTransaction) db.exec("ROLLBACK");
  db.close();
}
