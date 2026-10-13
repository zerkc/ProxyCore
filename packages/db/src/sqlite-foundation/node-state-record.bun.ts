import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import { openSQLite } from "./open.js";
import { encodeTimestamp } from "./timestamp.js";
import { getSQLiteNodeStateRecord, type SQLiteNodeStateRecord } from "./node-state-record.js";
import type { NodeStateRecord, ContinuityTransactionPort } from "../ports.js";

type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2) ? true : false
    : false;
type Expect<T extends true> = T;
type CanonicalAlias = Expect<Identical<SQLiteNodeStateRecord, NodeStateRecord>>;
type CanonicalReturn = Expect<Identical<ReturnType<typeof getSQLiteNodeStateRecord>,
  ReturnType<ContinuityTransactionPort["getNodeState"]>>>;

const [path] = process.argv.slice(2);
const db = openSQLite(path);
const stamp = (seconds: bigint, nanos = 0) => encodeTimestamp({ kind: "finite", seconds, nanos })!;
const uuid = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const dates = { enrolled_at: "enrolledAt", last_seen_at: "lastSeenAt", last_attempt_at: "lastAttemptAt",
  last_success_at: "lastSuccessAt", next_attempt_at: "nextAttemptAt", updated_at: "updatedAt" } as const;
const strings = { enrollment_primary_id: "enrollmentPrimaryId", last_applied_snapshot_id: "lastAppliedSnapshotId",
  enrollment_attempt_id: "enrollmentAttemptId", primary_url: "primaryUrl", primary_installation_id: "primaryInstallationId",
  primary_tls_spki_sha256: "primaryTlsSpkiSha256", credential_id: "credentialId", last_error_code: "lastErrorCode" } as const;
const columns = ["id", "enrolled_at", "enrollment_primary_id", "last_seen_at", "last_applied_snapshot_id",
  "enrollment_attempt_id", "primary_url", "primary_installation_id", "primary_tls_spki_sha256", "credential_id",
  "sync_enabled", "last_attempt_at", "last_success_at", "consecutive_failures", "next_attempt_at", "last_error_code", "updated_at"];
const keys = ["id", ...Object.values(dates), ...Object.values(strings), "syncEnabled", "consecutiveFailures"];
const optionalKeys = keys.filter(key => !["id", "updatedAt", "syncEnabled", "consecutiveFailures"].includes(key));
const select = `SELECT ${columns.join(", ")} FROM main.node_state WHERE id = ? LIMIT 1`;
let units = 0, scenarios = 0;
function equal(actual: unknown, expected: unknown) { assert.deepEqual(actual, expected); units++; }
function ok(value: unknown) { assert.ok(value); units++; }
async function rejects(body: () => Promise<unknown>, expected?: Error) {
  if (expected) await assert.rejects(body, error => error === expected);
  else await assert.rejects(body);
  units++;
}
function rows(connection = db) {
  // All rows here were created in this disposable fixture; the hash is synthetic.
  return connection.query("SELECT * FROM main.node_state ORDER BY id").all();
}
async function check(name: string, body: () => Promise<void>) {
  await body(); scenarios++; console.log(`PASS ${name}`);
}
async function test(name: string, body: () => Promise<void>) {
  await check(name, async () => {
    const before = rows(); db.exec("BEGIN");
    try { await body(); equal(db.inTransaction, true); }
    finally { if (db.inTransaction) db.exec("ROLLBACK"); }
    equal(rows(), before);
  });
}
function update(column: string, value: string | bigint | null) {
  db.query(`UPDATE main.node_state SET ${column}=? WHERE id='default'`).run(value);
}
async function read() {
  const before = rows(), changes = db.query("SELECT total_changes() AS n").get(), tx = db.inTransaction;
  const result = await getSQLiteNodeStateRecord(db);
  equal(rows(), before); equal(db.query("SELECT total_changes() AS n").get(), changes); equal(db.inTransaction, tx);
  if (result) equal(Object.keys(result).sort(), [...keys].sort());
  return result;
}
const raw = () => Object.fromEntries(columns.map(key => [key,
  key === "id" ? "default" : key === "updated_at" ? stamp(0n) :
    key === "sync_enabled" || key === "consecutive_failures" ? 0n : null]));
function driver(value: unknown, errors: { prepare?: Error; fetch?: Error; finalize?: Error } = {}) {
  let prepared = 0, fetched = 0, finalized = 0;
  const fake = {
    prepare(sql: string) {
      prepared++; equal(sql, select); ok(!sql.includes("enrollment_token_hash"));
      if (errors.prepare) throw errors.prepare;
      return {
        all(...args: unknown[]) { fetched++; equal(args, ["default"]); if (errors.fetch) throw errors.fetch; return value; },
        finalize() { finalized++; if (errors.finalize) throw errors.finalize; },
      };
    },
    query() { assert.fail("no cached queries"); }, exec() { assert.fail("no writes or transactions"); },
    close() { assert.fail("caller owns connection"); }, transaction() { assert.fail("caller owns transaction"); },
  } as unknown as Database;
  return { run: () => getSQLiteNodeStateRecord(fake), finalized: () => finalized,
    done: (counts = [1, 1, 1]) => equal([prepared, fetched, finalized], counts) };
}
try {
  const original = rows();
  await check("Go migrated defaults, canonical Promise and own nullable keys", async () => {
    const promise = getSQLiteNodeStateRecord(db); ok(promise instanceof Promise);
    const record = (await promise)!; equal(Object.keys(record).sort(), [...keys].sort());
    equal(record.id, "default"); equal(record.syncEnabled, false); equal(record.consecutiveFailures, 0);
    equal(record.updatedAt.getTime(), 0);
    for (const key of optionalKeys) { ok(Object.hasOwn(record, key)); equal(record[key as keyof NodeStateRecord], undefined); }
    equal(rows(), original);
  });
  await test("all filled fields and raw string case", async () => {
    for (const column of Object.keys(dates)) update(column, stamp(-1n, 999999999));
    for (const column of Object.keys(strings)) update(column, column.endsWith("_id") ? uuid : "  MiXeD raw ? '  ");
    update("sync_enabled", 1n); update("consecutive_failures", -1n);
    const record = (await read())!;
    for (const key of Object.values(dates)) equal(record[key]!.getTime(), -1);
    for (const [column, key] of Object.entries(strings)) equal(record[key], column.endsWith("_id") ? uuid : "  MiXeD raw ? '  ");
    equal(record.syncEnabled, true); equal(record.consecutiveFailures, -1);
  });
  for (const value of [-2147483648n, -1n, 0n, 2147483647n]) {
    await test(`signed failure count ${value}`, async () => {
      update("consecutive_failures", value); equal((await read())!.consecutiveFailures, Number(value));
    });
  }
  await test("default only despite other IDs", async () => {
    db.exec("DELETE FROM main.node_state WHERE id='default'");
    db.query("INSERT INTO main.node_state(id,updated_at) VALUES('other',?)").run(stamp(0n));
    equal(await read(), undefined);
  });
  await test("ignored synthetic hash and added column", async () => {
    db.exec("ALTER TABLE main.node_state ADD COLUMN test_only TEXT");
    db.exec("UPDATE main.node_state SET test_only='not DTO',enrollment_token_hash='synthetic-ignored-test-only'");
    const record = (await read())!; ok(!Object.hasOwn(record, "enrollmentTokenHash")); ok(!Object.hasOwn(record, "test_only"));
  });
  const finite = [
    [0n, 0, 0], [0n, 999999, 0], [0n, 1000000, 1], [-1n, 1, -1000], [-1n, 999999999, -1],
    [-8640000000000n, 0, -8640000000000000], [8640000000000n, 0, 8640000000000000],
    [-8640000000001n, 999999999, NaN], [8640000000000n, 1000000, NaN],
    [-(1n << 63n), 0, NaN], [(1n << 63n) - 1n, 999999999, NaN],
  ] as const;
  for (const [column, key] of Object.entries(dates)) {
    for (const [seconds, nanos, expected] of finite) {
      await test(`${column} finite ${seconds}/${nanos}`, async () => {
        update(column, stamp(seconds, nanos)); equal((await read())![key]!.getTime(), expected);
      });
    }
    for (const infinity of ["0", "2"]) {
      await test(`${column} general infinity ${infinity}`, async () => {
        update(column, infinity); ok(Number.isNaN((await read())![key]!.getTime()));
      });
    }
    await test(`${column} detached fresh Date`, async () => {
      update(column, stamp(123n, 456789123));
      const first = (await read())![key]!, second = (await read())![key]!;
      ok(first !== second); first.setTime(999); equal(second.getTime(), 123456);
      equal((await read())![key]!.getTime(), 123456);
    });
  }
  await check("single SELECT absent and finalize before row validation and projection", async () => {
    const empty = driver([]); equal(await empty.run(), undefined); empty.done();
    const value = raw(), d = driver([value]);
    Object.defineProperty(value, "updated_at", { enumerable: true, get() { equal(d.finalized(), 1); return stamp(0n); } });
    equal((await d.run())!.updatedAt.getTime(), 0); d.done();
    const invalid = driver([null]); await rejects(invalid.run); invalid.done();
    const validation = driver([new Proxy(raw(), { ownKeys(target) {
      equal(validation.finalized(), 1); return Reflect.ownKeys(target);
    } })]);
    await validation.run(); validation.done();
  });
  await check("prepare fetch finalize error identities", async () => {
    for (const stage of ["prepare", "fetch", "finalize"] as const) {
      for (const value of [[], [raw()]]) {
        const error = new Error(stage), d = driver(value, { [stage]: error });
        await rejects(d.run, error); d.done(stage === "prepare" ? [1, 0, 0] : [1, 1, 1]);
      }
    }
  });
  await check("malformed rows and keys reject rather than empty", async () => {
    for (const value of [null, undefined, {}, [null], [false], [raw(), raw()], [{ ...raw(), extra: null }]]) {
      const d = driver(value); await rejects(d.run); d.done();
    }
    for (const column of columns) {
      const value = raw(); delete value[column]; const d = driver([value]); await rejects(d.run); d.done();
    }
  });
  await check("all scalar type and domain failures", async () => {
    for (const column of ["id", ...Object.keys(strings)]) {
      for (const value of [undefined, 5, 1n, {}, "bad\0text", ...(column.endsWith("_id") ? ["BAD", uuid.toUpperCase()] : [])]) {
        const d = driver([{ ...raw(), [column]: value }]); await rejects(d.run); d.done();
      }
    }
    for (const [column, values] of [
      ["sync_enabled", [0, 1, -1n, 2n, null, undefined, "1", true]],
      ["consecutive_failures", [0, 1.5, -2147483649n, 2147483648n, null, undefined, "0"]],
    ] as const) {
      for (const value of values) { const d = driver([{ ...raw(), [column]: value }]); await rejects(d.run); d.done(); }
    }
    for (const column of Object.keys(dates)) {
      for (const value of [undefined, 0, 0n, new Date(0), "3", "", "1bad", "118446744073709551616.000000000",
        "109223372036854775808.00000000x", "109223372036854775808.0000000.1", ...(column === "updated_at" ? [null] : [])]) {
        const d = driver([{ ...raw(), [column]: value }]); await rejects(d.run); d.done();
      }
    }
  });
  await test("TEMP shadow and cached cursor remain caller owned", async () => {
    db.exec("CREATE TEMP TABLE node_state(id TEXT)");
    const cursor = db.query("SELECT id FROM main.node_state");
    equal(cursor.get(), { id: "default" }); equal((await read())!.id, "default");
    equal(cursor.get(), { id: "default" }); cursor.finalize(); db.exec("DROP TABLE temp.node_state");
  });
  await test("pending update delete insert and savepoint rollback invisible externally", async () => {
    const external = openSQLite(path);
    try {
      const committed = await getSQLiteNodeStateRecord(external);
      db.exec("SAVEPOINT caller"); update("primary_url", "pending");
      equal((await read())!.primaryUrl, "pending"); equal(await getSQLiteNodeStateRecord(external), committed);
      db.exec("DELETE FROM main.node_state WHERE id='default'");
      equal(await read(), undefined); equal(await getSQLiteNodeStateRecord(external), committed);
      db.query("INSERT INTO main.node_state(id,updated_at,primary_url) VALUES('default',?,'insert-pending')").run(stamp(5n));
      equal((await read())!.primaryUrl, "insert-pending"); equal(await getSQLiteNodeStateRecord(external), committed);
      db.exec("ROLLBACK TO caller; RELEASE caller"); equal(await read(), committed);
    } finally { external.close(); }
  });
  await test("real corrupt data rejects and next read recovers without changing status", async () => {
    db.exec("PRAGMA ignore_check_constraints=ON");
    try {
      for (const [column, value] of [["sync_enabled", 2n], ["consecutive_failures", 2147483648n], ["updated_at", "bad"]] as const) {
        db.exec("SAVEPOINT corrupt"); update(column, value); const before = rows();
        await rejects(() => getSQLiteNodeStateRecord(db)); equal(rows(), before); equal(db.inTransaction, true);
        db.exec("ROLLBACK TO corrupt; RELEASE corrupt"); equal((await read())!.id, "default");
      }
    } finally { db.exec("PRAGMA ignore_check_constraints=OFF"); }
  });
  await check("native finalized cursor before injected failures and projection", async () => {
    for (const stage of ["fetch", "finalize", "projection"] as const) {
      let finalizations = 0; const error = new Error(stage);
      const facade = { prepare(sql: string) {
        const statement = db.prepare(sql);
        return { all(...args: [string]) {
          if (stage === "fetch") throw error;
          const values = statement.all(...args);
          if (stage === "projection") Object.defineProperty(values[0], "id", { enumerable: true, get() { throw error; } });
          return values;
        }, finalize() { statement.finalize(); finalizations++; if (stage === "finalize") throw error; } };
      } } as unknown as Database;
      await rejects(() => getSQLiteNodeStateRecord(facade), error); equal(finalizations, 1);
      equal((await read())!.id, "default"); equal(db.inTransaction, false);
    }
  });
  await test("native prepare and step failures finalize then recover", async () => {
    db.exec("SAVEPOINT unavailable; ALTER TABLE main.node_state RENAME TO fixture_node_state");
    await rejects(() => getSQLiteNodeStateRecord(db)); equal(db.inTransaction, true);
    db.exec("ROLLBACK TO unavailable; RELEASE unavailable");
    let finalizations = 0;
    db.exec("SAVEPOINT invalidated");
    const facade = { prepare(sql: string) {
      const statement = db.prepare(sql);
      db.exec("ALTER TABLE main.node_state RENAME TO fixture_node_state");
      return { all(...args: [string]) { return statement.all(...args); },
        finalize() { statement.finalize(); finalizations++; } };
    } } as unknown as Database;
    await rejects(() => getSQLiteNodeStateRecord(facade)); equal(finalizations, 1); equal(db.inTransaction, true);
    db.exec("ROLLBACK TO invalidated; RELEASE invalidated"); equal((await read())!.id, "default");
  });
  await check("detached records and no connection metadata or lifecycle access", async () => {
    const first = (await read())!, second = (await read())!; ok(first !== second);
    first.primaryUrl = "mutated"; equal(second.primaryUrl, undefined); equal((await read())!.primaryUrl, undefined);
    const d = driver([raw()]); const facade = new Proxy({} as Database, { get(_target, key) {
      if (key !== "prepare") assert.fail(`unexpected connection access ${String(key)}`);
      return (sql: string) => {
        equal(sql, select);
        return { all: () => [raw()], finalize: () => {} };
      };
    } });
    equal((await getSQLiteNodeStateRecord(facade))!.id, "default");
    await d.run(); d.done();
  });
  await check("reopen native bytes preserved and closed connection rejects", async () => {
    equal(rows(), original); const reopened = openSQLite(path);
    equal(rows(reopened), original); equal(await getSQLiteNodeStateRecord(reopened), await read());
    reopened.close(); await rejects(() => getSQLiteNodeStateRecord(reopened)); equal((await read())!.id, "default");
  });
  console.log(`COMPLETE ${units} Bun assertion units; ${scenarios} scenarios`);
} finally { db.close(); }
