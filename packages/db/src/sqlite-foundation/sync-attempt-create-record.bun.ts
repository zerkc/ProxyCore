import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import { createSQLiteSyncAttemptRecord } from "./sync-attempt-create-record.js";
import type { SyncAttemptRecord, ContinuityTransactionPort } from "../ports.js";

const path = process.argv[2];
function open() {
  const connection = new Database(path, { strict: true, safeIntegers: true });
  connection.exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=0");
  return connection;
}
const db = open();
const id = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const node = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
const revision = "cccccccc-cccc-cccc-cccc-cccccccccccc";
const job = "dddddddd-dddd-dddd-dddd-dddddddddddd";
const second = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee";
const missing = "ffffffff-ffff-ffff-ffff-ffffffffffff";
const fields = ["id", "nodeId", "trigger", "status", "sourcePrimaryId",
  "leadershipGeneration", "snapshotVersion", "replicationVersion", "contentHash",
  "revisionId", "applyJobId", "resultCode", "startedAt", "finishedAt", "createdAt", "updatedAt"] as const;
const columns = ["id", "node_id", '"trigger"', "status", "source_primary_id",
  "leadership_generation", "snapshot_version", "replication_version", "content_hash",
  "revision_id", "apply_job_id", "result_code", "started_at", "finished_at", "created_at", "updated_at"];
const canonical: (db: Database, record: Parameters<ContinuityTransactionPort["recordSyncAttempt"]>[0]) => Promise<void>
  = createSQLiteSyncAttemptRecord;
function filled(): Record<string, unknown> {
  return {
    id, nodeId: node, trigger: "manual", status: "running", sourcePrimaryId: node,
    leadershipGeneration: 9007199254740992, snapshotVersion: -2147483648,
    replicationVersion: 2147483647, contentHash: "  hash 雪 🦊  ", revisionId: revision,
    applyJobId: job, resultCode: "  result 雪  ", startedAt: new Date(-1),
    finishedAt: new Date(1), createdAt: new Date(-1), updatedAt: new Date(1),
  };
}
const write = (value: object, connection = db) => canonical(connection, value as SyncAttemptRecord);
function all(connection = db) {
  const statement = connection.prepare(`SELECT ${columns.join(", ")} FROM main.sync_attempts ORDER BY id`);
  try { return statement.values(); } finally { statement.finalize(); }
}
let units = 0, scenarios = 0;
function equal(actual: unknown, expected: unknown) { assert.deepEqual(actual, expected); units++; }
function ok(value: unknown) { assert.ok(value); units++; }
async function rejects(body: () => Promise<unknown>, expected?: Error | RegExp) {
  let caught: unknown;
  try { await body(); } catch (error) { caught = error; }
  ok(caught instanceof Error);
  if (expected instanceof Error) equal(caught, expected);
  else if (expected) { assert.match((caught as Error).message, expected); units++; }
  return caught;
}
async function unchanged(value: object, expected?: Error | RegExp, connection = db) {
  const before = all();
  await rejects(() => write(value, connection), expected);
  equal(all(), before);
}
async function test(name: string, body: () => Promise<void>) {
  db.exec("DELETE FROM main.sync_attempts");
  await body();
  scenarios++;
  console.log(`ok ${scenarios}: ${name}`);
}
function facade(errors: { prepare?: Error; run?: Error; finalize?: Error } = {}, native = true) {
  let prepares = 0, runs = 0, finalizes = 0;
  let sql = "", bindings: unknown[] = [];
  const connection = {
    prepare(text: string) {
      prepares++; sql = text;
      ok(text.startsWith("INSERT INTO main.sync_attempts "));
      ok(!/RETURNING|SELECT|CONFLICT|BEGIN|COMMIT/i.test(text));
      if (errors.prepare) throw errors.prepare;
      const statement = native ? db.prepare(text) : undefined;
      return {
        run(...args: any[]) {
          runs++; bindings = args;
          if (errors.run) throw errors.run;
          return statement?.run(...args);
        },
        finalize() {
          finalizes++; statement?.finalize();
          if (errors.finalize) throw errors.finalize;
        },
      };
    },
    query() { assert.fail("no caller cached statements"); },
    exec() { assert.fail("no other SQL or owned transaction"); },
    close() { assert.fail("caller owns connection"); },
    transaction() { assert.fail("caller owns transaction"); },
    get inTransaction() { return assert.fail("no nesting policy"); },
  } as unknown as Database;
  return { connection, counts: () => [prepares, runs, finalizes], sql: () => sql, bindings: () => bindings };
}
const required = ["id", "nodeId", "trigger", "createdAt", "updatedAt"] as const;
const optional = fields.filter(key => !required.includes(key as typeof required[number]) && key !== "status");
const dates = ["startedAt", "finishedAt", "createdAt", "updatedAt"] as const;
const uuidFields = ["id", "nodeId", "sourcePrimaryId", "revisionId", "applyJobId"] as const;
const expectedRow = [id, node, "manual", "running", node, 9007199254740992n,
  -2147483648n, 2147483647n, "  hash 雪 🦊  ", revision, job, "  result 雪  ",
  "109223372036854775807.999000000", "109223372036854775808.001000000",
  "109223372036854775807.999000000", "109223372036854775808.001000000"];
try {
  await test("all sixteen canonical fields bound in one MAIN statement, void after finalization", async () => {
    const seam = facade();
    equal(await write({ ...filled(), ignored: "not a column" }, seam.connection), undefined);
    equal(seam.counts(), [1, 1, 1]);
    equal(seam.sql(), `INSERT INTO main.sync_attempts (${columns.join(", ")}) VALUES (${fields.map(() => "?").join(", ")})`);
    // Native integer storage becomes bigint; int32 inputs stay number in bindings.
    const bindings: unknown[] = [...expectedRow]; bindings[6] = -2147483648; bindings[7] = 2147483647;
    equal(seam.bindings(), bindings);
    equal(all(), [expectedRow]);
  });
  await test("every undefined field omitted and every explicit null bypasses its encoder", async () => {
    for (const key of fields) {
      const index = fields.indexOf(key), omitted = facade({}, false), nulled = facade({}, false);
      await write({ ...filled(), [key]: undefined }, omitted.connection);
      equal(omitted.counts(), [1, 1, 1]);
      equal(omitted.bindings().length, 15);
      ok(!omitted.sql().split("(")[1].split(")")[0].split(", ").includes(columns[index]));
      await write({ ...filled(), [key]: null }, nulled.connection);
      equal(nulled.counts(), [1, 1, 1]);
      equal(nulled.bindings().length, 16); equal(nulled.bindings()[index], null);
    }
    const empty = facade({}, false);
    await write(Object.fromEntries(fields.map(key => [key, undefined])), empty.connection);
    equal(empty.sql(), "INSERT INTO main.sync_attempts DEFAULT VALUES");
    equal(empty.bindings(), []); equal(empty.counts(), [1, 1, 1]);
    await unchanged({});
  });
  await test("optional omitted undefined and SQL NULL paths, native queued default", async () => {
    for (const key of optional) {
      for (const mode of ["absent", "undefined", "null"]) {
        const value = filled();
        if (mode === "absent") delete value[key];
        else value[key] = mode === "null" ? null : undefined;
        await write(value);
        equal(all()[0][fields.indexOf(key)], null);
        db.exec("DELETE FROM main.sync_attempts");
      }
    }
    for (const mode of ["absent", "undefined"]) {
      const value = filled();
      if (mode === "absent") delete value.status; else value.status = undefined;
      await write(value); equal(all()[0][3], "queued");
      equal(all()[0].slice(14), expectedRow.slice(14));
      db.exec("DELETE FROM main.sync_attempts");
    }
    await unchanged({ ...filled(), status: null }, /NOT NULL/);
  });
  await test("missing required values receive no synthetic UUID or clocks", async () => {
    for (const key of required) {
      const absent = filled(); delete absent[key];
      await unchanged(absent, /NOT NULL/);
      await unchanged({ ...filled(), [key]: undefined }, /NOT NULL/);
      await unchanged({ ...filled(), [key]: null }, /NOT NULL/);
    }
  });
  await test("Date ISO validation RangeError identity occurs before any SQL", async () => {
    for (const key of dates) {
      const invalid = new Date(NaN);
      let original: unknown;
      try { invalid.toISOString(); } catch (error) { original = error; }
      ok(original instanceof RangeError);
      invalid.toISOString = () => { throw original; };
      const seam = facade();
      await unchanged({ ...filled(), [key]: invalid }, original as Error, seam.connection);
      equal(seam.counts(), [0, 0, 0]);
      const native = facade();
      const error = await rejects(() => write({ ...filled(), [key]: new Date(NaN) }, native.connection));
      ok(error instanceof RangeError); equal(native.counts(), [0, 0, 0]);
      for (const malformed of [0, "2020-01-01", {}, true]) {
        const wrong = facade();
        await unchanged({ ...filled(), [key]: malformed }, undefined, wrong.connection);
        equal(wrong.counts(), [0, 0, 0]);
      }
    }
  });
  await test("Date TimeClip endpoints and negative fractional seconds use canonical ISO", async () => {
    for (const millis of [-8640000000000000, -1001, -1, 0, 1, 1001, 8640000000000000]) {
      await write({ ...filled(), startedAt: new Date(millis), finishedAt: new Date(millis),
        createdAt: new Date(millis), updatedAt: new Date(millis) });
      const ms = BigInt(millis), remainder = ((ms % 1000n) + 1000n) % 1000n;
      const expected = `1${((ms - remainder) / 1000n + (1n << 63n)).toString().padStart(20, "0")}.${(remainder * 1000000n).toString().padStart(9, "0")}`;
      equal(all()[0].slice(12), [expected, expected, expected, expected]);
      db.exec("DELETE FROM main.sync_attempts");
    }
  });
  await test("represented-number int64 bounds preserve unsafe integers without rounding", async () => {
    for (const value of [0, -0, 1, -1, 9007199254740992, -9007199254740992,
      -(2 ** 63), 2 ** 63 - 1024]) {
      await write({ ...filled(), leadershipGeneration: value });
      equal(all()[0][5], BigInt(value)); db.exec("DELETE FROM main.sync_attempts");
    }
    for (const value of [2 ** 63, -(2 ** 63) - 2048, NaN, Infinity, -Infinity,
      1.5, "1", 1n, true, {}]) {
      const seam = facade();
      await unchanged({ ...filled(), leadershipGeneration: value }, /int64/, seam.connection);
      equal(seam.counts(), [0, 0, 0]);
    }
  });
  await test("both int32 versions accept endpoints and reject fractions types and infinities", async () => {
    for (const key of ["snapshotVersion", "replicationVersion"] as const) {
      for (const value of [-2147483648, -1, 0, 2147483647]) {
        await write({ ...filled(), [key]: value });
        equal(all()[0][fields.indexOf(key)], BigInt(value)); db.exec("DELETE FROM main.sync_attempts");
      }
      for (const value of [-2147483649, 2147483648, NaN, Infinity, -Infinity, 0.5, "1", 1n, true, {}]) {
        const seam = facade();
        await unchanged({ ...filled(), [key]: value }, /int32/, seam.connection);
        equal(seam.counts(), [0, 0, 0]);
      }
    }
  });
  await test("native UUID shape lowercase and non-NUL checks on every UUID field", async () => {
    for (const key of uuidFields) {
      for (const value of ["", "not-a-uuid", node.toUpperCase(), node + "\n", node + "\0", node.replaceAll("-", "")]) {
        await unchanged({ ...filled(), [key]: value }, /CHECK/);
      }
    }
    await write({ ...filled(), nodeId: missing, sourcePrimaryId: missing });
    equal(all()[0][1], missing); equal(all()[0][4], missing); // Neither has a FK.
  });
  await test("native trigger and status checks without extra application state policy", async () => {
    for (const trigger of ["enrollment", "periodic", "manual", "restart"]) {
      for (const status of ["queued", "running", "current", "applied", "failed", "pending-ack"]) {
        await write({ ...filled(), trigger, status, startedAt: new Date(1), finishedAt: new Date(-1) });
        equal(all()[0].slice(2, 4), [trigger, status]); db.exec("DELETE FROM main.sync_attempts");
      }
    }
    for (const key of ["trigger", "status"]) {
      for (const value of ["", "unknown", "QUEUED", "manual\0", "queued\n"]) {
        await unchanged({ ...filled(), [key]: value }, /CHECK/);
      }
    }
  });
  await test("native text NUL rejection and bound whitespace unicode literal identity", async () => {
    for (const key of ["contentHash", "resultCode"]) {
      await unchanged({ ...filled(), [key]: "prefix\0suffix" }, /CHECK/);
      const value = "  '); SELECT 雪 🦊\n  ";
      await write({ ...filled(), [key]: value });
      equal(all()[0][fields.indexOf(key as typeof fields[number])], value);
      db.exec("DELETE FROM main.sync_attempts");
    }
  });
  await test("valid nonNULL FK parents and independently invalid revision/job references", async () => {
    await write(filled()); equal(all(), [expectedRow]);
    db.exec("DELETE FROM main.sync_attempts");
    await unchanged({ ...filled(), revisionId: missing }, /FOREIGN KEY/);
    await unchanged({ ...filled(), applyJobId: missing }, /FOREIGN KEY/);
    await write({ ...filled(), revisionId: null, applyJobId: null });
    equal(all()[0].slice(9, 11), [null, null]);
  });
  await test("duplicate ID native query error identity and per-owned-statement finalization", async () => {
    await write(filled());
    let nativeError: unknown, finalized = 0;
    const connection = {
      prepare(sql: string) {
        const statement = db.prepare(sql);
        return {
          run(...args: any[]) {
            try { return statement.run(...args); } catch (error) { nativeError = error; throw error; }
          },
          finalize() { statement.finalize(); finalized++; },
        };
      },
    } as unknown as Database;
    const before = all(), propagated = await rejects(() => write(filled(), connection));
    equal(propagated, nativeError); equal(finalized, 1); equal(all(), before);
    const seam = facade(); await unchanged(filled(), /UNIQUE/, seam.connection);
    equal(seam.counts(), [1, 1, 1]);
  });
  await test("prepare/run sentinel identity and actual native prepare failure", async () => {
    for (const phase of ["prepare", "run"] as const) {
      const error = new Error(`synthetic ${phase}`), seam = facade({ [phase]: error });
      await unchanged(filled(), error, seam.connection);
      equal(seam.counts(), phase === "prepare" ? [1, 0, 0] : [1, 1, 1]);
    }
    let nativeError: unknown;
    const broken = { prepare(sql: string) {
      try { return db.prepare(sql + " invalid SQL"); } catch (error) { nativeError = error; throw error; }
    } } as unknown as Database;
    const propagated = await rejects(() => write(filled(), broken));
    equal(propagated, nativeError); equal(all(), []);
    const closed = open(); closed.close(); await unchanged(filled(), undefined, closed);
    await write(filled()); equal(all().length, 1);
  });
  await test("post-native-finalizer rejection can leave durable autocommit success", async () => {
    const error = new Error("synthetic after real native finalize"), seam = facade({ finalize: error });
    await rejects(() => write(filled(), seam.connection), error);
    equal(seam.counts(), [1, 1, 1]); equal(db.inTransaction, false); equal(all(), [expectedRow]);
    const other = open(); try { equal(all(other), all()); } finally { other.close(); }
    await write({ ...filled(), id: second }); equal(all().length, 2);
  });
  await test("finalizer local error priority after actual native duplicate rejection", async () => {
    await write(filled());
    const error = new Error("synthetic finalizer priority"), seam = facade({ finalize: error });
    await unchanged(filled(), error, seam.connection); equal(seam.counts(), [1, 1, 1]);
  });
  await test("caller cached statement survives success and rejection", async () => {
    const cached = db.query("SELECT count(*) FROM main.sync_attempts");
    equal(cached.values(), [[0n]]); await write(filled());
    equal(db.query("SELECT count(*) FROM main.sync_attempts"), cached);
    equal(cached.values(), [[1n]]); await unchanged(filled(), /UNIQUE/);
    equal(cached.values(), [[1n]]); cached.finalize(); equal(all().length, 1);
  });
  await test("caller pending transaction savepoint and outer rollback preserve external invisibility", async () => {
    const other = open(); db.exec("BEGIN IMMEDIATE");
    try {
      await write(filled()); equal(db.inTransaction, true); equal(all().length, 1); equal(all(other), []);
      db.exec("SAVEPOINT caller_owned"); await write({ ...filled(), id: second });
      equal(all().length, 2); equal(all(other), []); await unchanged(filled(), /UNIQUE/);
      equal(db.inTransaction, true); db.exec("ROLLBACK TO caller_owned; RELEASE caller_owned");
      equal(all().length, 1); db.exec("ROLLBACK"); equal(all(), []); equal(all(other), []);
    } finally { if (db.inTransaction) db.exec("ROLLBACK"); other.close(); }
    const reopened = open(); try { equal(all(reopened), []); } finally { reopened.close(); }
  });
  await test("post-native-finalizer rejection remains pending and caller rollbackable", async () => {
    const other = open(), error = new Error("synthetic pending finalizer");
    db.exec("BEGIN IMMEDIATE; SAVEPOINT caller_finalizer");
    try {
      const seam = facade({ finalize: error });
      await rejects(() => write(filled(), seam.connection), error); equal(seam.counts(), [1, 1, 1]);
      equal(db.inTransaction, true); equal(all().length, 1); equal(all(other), []);
      db.exec("ROLLBACK TO caller_finalizer; RELEASE caller_finalizer; ROLLBACK");
      equal(all(), []); equal(all(other), []);
    } finally { if (db.inTransaction) db.exec("ROLLBACK"); other.close(); }
  });
  await test("actual deferred-FK COMMIT failure via synthetic TEMP fixture and recovery", async () => {
    db.exec(`CREATE TEMP TABLE sync_guard_parent(id INTEGER PRIMARY KEY);
      CREATE TEMP TABLE sync_guard_child(ref INTEGER REFERENCES sync_guard_parent(id) DEFERRABLE INITIALLY DEFERRED);
      CREATE TEMP TRIGGER sync_guard AFTER INSERT ON main.sync_attempts
      BEGIN INSERT INTO sync_guard_child VALUES(1); END;`);
    const other = open();
    try {
      db.exec("BEGIN IMMEDIATE; SAVEPOINT caller_commit_failure");
      equal(await write(filled()), undefined); equal(all().length, 1);
      db.exec("RELEASE caller_commit_failure"); equal(all(other), []);
      assert.throws(() => db.exec("COMMIT"), /FOREIGN KEY/); units++;
      equal(db.inTransaction, true); equal(all().length, 1); equal(all(other), []);
      db.exec("ROLLBACK"); equal(all(), []);
      const guard = db.prepare("SELECT * FROM temp.sync_guard_child");
      try { equal(guard.values(), []); } finally { guard.finalize(); }
      db.exec("INSERT INTO temp.sync_guard_parent VALUES(1); BEGIN IMMEDIATE");
      await write(filled()); equal(all(other), []); db.exec("COMMIT"); equal(all(other), all());
    } finally {
      if (db.inTransaction) db.exec("ROLLBACK");
      db.exec("DROP TRIGGER temp.sync_guard; DROP TABLE temp.sync_guard_child; DROP TABLE temp.sync_guard_parent");
      other.close();
    }
    const reopened = open(); try { equal(all(reopened), [expectedRow]); } finally { reopened.close(); }
  });
  await test("caller commit after released savepoint is durable on reopen", async () => {
    const other = open(); db.exec("BEGIN IMMEDIATE; SAVEPOINT caller_commit");
    try {
      await write(filled()); db.exec("RELEASE caller_commit");
      equal(db.inTransaction, true); equal(all(other), []); db.exec("COMMIT");
      equal(db.inTransaction, false); equal(all(other), [expectedRow]);
    } finally { if (db.inTransaction) db.exec("ROLLBACK"); other.close(); }
    const reopened = open(); try { equal(all(reopened), [expectedRow]); } finally { reopened.close(); }
  });
  await test("MAIN target ignores synthetic caller TEMP shadow", async () => {
    db.exec("CREATE TEMP TABLE sync_attempts(caller_marker TEXT)");
    try {
      await write(filled()); const statement = db.prepare("SELECT * FROM temp.sync_attempts");
      try { equal(statement.values(), []); } finally { statement.finalize(); }
      equal(all(), [expectedRow]);
    } finally { db.exec("DROP TABLE temp.sync_attempts"); }
  });
  await test("bounded native BUSY rejection and next-write recovery", async () => {
    const other = open(), seam = facade(); other.exec("BEGIN IMMEDIATE");
    try { await unchanged(filled(), /locked|busy/i, seam.connection); equal(seam.counts(), [1, 1, 1]); }
    finally { other.exec("ROLLBACK"); other.close(); }
    await write(filled()); equal(all(), [expectedRow]);
  });
  // Leave only the complete synthetic row for the Go raw-field oracle.
  db.exec("DELETE FROM main.sync_attempts"); await write(filled()); equal(all(), [expectedRow]);
  const foreign = db.prepare("PRAGMA foreign_key_check");
  try { equal(foreign.values(), []); } finally { foreign.finalize(); }
  const other = open(); try { equal(all(other), all()); } finally { other.close(); }
  console.log(`sync-attempt-create checks=${units} scenarios=${scenarios}`);
} finally { db.close(); }
