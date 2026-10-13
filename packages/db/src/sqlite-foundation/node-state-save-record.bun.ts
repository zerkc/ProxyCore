import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import { openSQLite } from "./open.js";
import { encodeTimestamp } from "./timestamp.js";
import { saveSQLiteNodeStateRecord, type SQLiteNodeStateSaveRecord } from "./node-state-save-record.js";
import { getSQLiteNodeStateRecord } from "./node-state-record.js";
import type { NodeStateRecord, ContinuityTransactionPort } from "../ports.js";

type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2) ? true : false
    : false;
type Expect<T extends true> = T;
type CanonicalAlias = Expect<Identical<SQLiteNodeStateSaveRecord, NodeStateRecord>>;
type CanonicalInput = Expect<Identical<Parameters<typeof saveSQLiteNodeStateRecord>[1], NodeStateRecord>>;
type CanonicalReturn = Expect<Identical<ReturnType<typeof saveSQLiteNodeStateRecord>,
  ReturnType<ContinuityTransactionPort["saveNodeState"]>>>;

const path = process.argv[2], db = openSQLite(path);
const uuid = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const dates = { enrolledAt: "enrolled_at", lastSeenAt: "last_seen_at", lastAttemptAt: "last_attempt_at",
  lastSuccessAt: "last_success_at", nextAttemptAt: "next_attempt_at", updatedAt: "updated_at" } as const;
const strings = { enrollmentPrimaryId: "enrollment_primary_id", lastAppliedSnapshotId: "last_applied_snapshot_id",
  enrollmentAttemptId: "enrollment_attempt_id", primaryUrl: "primary_url", primaryInstallationId: "primary_installation_id",
  primaryTlsSpkiSha256: "primary_tls_spki_sha256", credentialId: "credential_id", lastErrorCode: "last_error_code" } as const;
const optional = [...Object.keys(strings), ...Object.keys(dates).filter(key => key !== "updatedAt")];
const fields = ["enrolledAt", "enrollmentPrimaryId", "lastSeenAt", "lastAppliedSnapshotId", "enrollmentAttemptId",
  "primaryUrl", "primaryInstallationId", "primaryTlsSpkiSha256", "credentialId", "syncEnabled", "lastAttemptAt",
  "lastSuccessAt", "consecutiveFailures", "nextAttemptAt", "lastErrorCode", "updatedAt"];
const columns = ["enrolled_at", "enrollment_primary_id", "last_seen_at", "last_applied_snapshot_id", "enrollment_attempt_id",
  "primary_url", "primary_installation_id", "primary_tls_spki_sha256", "credential_id", "sync_enabled", "last_attempt_at",
  "last_success_at", "consecutive_failures", "next_attempt_at", "last_error_code", "updated_at"];
const stamp = (ms: number) => {
  const millis = BigInt(ms), remainder = ((millis % 1000n) + 1000n) % 1000n;
  return encodeTimestamp({ kind: "finite", seconds: (millis - remainder) / 1000n, nanos: Number(remainder) * 1000000 });
};
const minimal = (id = "default") => ({ id, updatedAt: new Date(0) });
function filled(id = "default"): Record<string, unknown> {
  const state: Record<string, unknown> = { id, syncEnabled: true, consecutiveFailures: -7 };
  for (const key of Object.keys(dates)) state[key] = new Date(-1);
  for (const [key, column] of Object.entries(strings)) state[key] = column.endsWith("_id") ? uuid : "  Raw 雪 ? '  ";
  return state;
}
const row = (connection = db, id = "default") => connection.query<Record<string, unknown>, [string]>(
  "SELECT * FROM main.node_state WHERE id=?").get(id);
const all = () => db.query("SELECT * FROM main.node_state ORDER BY id").all();
let units = 0, scenarios = 0;
function equal(actual: unknown, expected: unknown) { assert.deepEqual(actual, expected); units++; }
function ok(value: unknown) { assert.ok(value); units++; }
async function rejects(body: () => Promise<unknown>, expected?: Error | RegExp) {
  if (expected instanceof Error) await assert.rejects(body, error => error === expected);
  else if (expected) await assert.rejects(body, expected);
  else await assert.rejects(body);
  units++;
}
const save = (state: object, connection = db) => saveSQLiteNodeStateRecord(connection, state as NodeStateRecord);
async function unchanged(state: object, expected?: Error | RegExp, connection = db) {
  const before = all(); await rejects(() => save(state, connection), expected); equal(all(), before);
}
async function test(name: string, body: () => Promise<void>) {
  db.exec("DELETE FROM main.node_state");
  await body(); scenarios++; console.log(`PASS ${name}`);
}

// A local facade owns no global patches and forbids every non-prepare operation.
function facade(errors: { prepare?: Error; run?: Error; finalize?: Error } = {}, native = true) {
  let prepares = 0, runs = 0, finalizes = 0;
  let sql = "", bindings: unknown[] = [];
  const connection = {
    prepare(text: string) {
      prepares++; sql = text;
      ok(text.startsWith("INSERT INTO main.node_state "));
      ok(!/RETURNING|enrollment_token_hash|BEGIN|COMMIT|SELECT/i.test(text));
      if (errors.prepare) throw errors.prepare;
      const statement = native ? db.prepare(text) : undefined;
      return {
        run(...args: any[]) {
          runs++; bindings = args;
          if (errors.run) throw errors.run;
          return statement?.run(...args);
        },
        finalize() { finalizes++; statement?.finalize(); if (errors.finalize) throw errors.finalize; },
      };
    },
    query() { assert.fail("no cached cursor"); }, exec() { assert.fail("no owned transaction or SQL"); },
    close() { assert.fail("caller owns connection"); }, transaction() { assert.fail("caller owns transaction"); },
    get inTransaction() { return assert.fail("no transaction guard"); },
  } as unknown as Database;
  return { connection, counts: () => [prepares, runs, finalizes], sql: () => sql, bindings: () => bindings };
}

try {
  await test("full provided fields canonical void and native bytes", async () => {
    const state = filled(), seam = facade();
    const promise = save(state, seam.connection); ok(promise instanceof Promise); equal(await promise, undefined);
    equal(seam.counts(), [1, 1, 1]);
    equal(seam.sql(), `INSERT INTO main.node_state (id, ${columns.join(", ")}) VALUES (${Array(17).fill("?").join(", ")})`
      + ` ON CONFLICT(id) DO UPDATE SET ${columns.map(column => `${column} = excluded.${column}`).join(", ")}`);
    const native = row()!; equal(native.id, "default"); equal(native.enrollment_token_hash, null);
    for (const [key, column] of Object.entries(dates)) equal(native[column], stamp((state[key] as Date).getTime()));
    for (const [key, column] of Object.entries(strings)) equal(native[column], state[key]);
    equal(native.sync_enabled, 1n); equal(native.consecutive_failures, -7n);
    equal(await getSQLiteNodeStateRecord(db), state);
    equal(Object.keys((await getSQLiteNodeStateRecord(db))!).sort(), ["id", ...fields].sort());
    const second = filled(); second.primaryUrl = "second"; second.syncEnabled = false; second.consecutiveFailures = 9;
    equal(await save(second), undefined); equal(await getSQLiteNodeStateRecord(db), second);
  });
  await test("provided nondefault ID and missing null ID native failures", async () => {
    await save(minimal("other")); equal(row(db, "other")!.id, "other"); equal(await getSQLiteNodeStateRecord(db), undefined);
    for (const id of [undefined, null]) await unchanged({ ...minimal(), id }, /NOT NULL/);
    for (const id of ["", "MiXeD", "' OR 1=1 --"]) {
      await save(minimal(id)); equal(row(db, id)!.id, id);
    }
  });
  await test("minimal undefined INSERT defaults versus full CONFLICT preservation", async () => {
    await save(minimal()); const native = row()!;
    equal(native.sync_enabled, 0n); equal(native.consecutive_failures, 0n);
    for (const column of [...Object.values(strings), ...Object.values(dates).filter(column => column !== "updated_at")]) equal(native[column], null);
    await save(filled()); const before = row()!;
    await save({ ...minimal(), updatedAt: new Date(123) });
    equal(row(), { ...before, updated_at: stamp(123) });
  });
  for (const key of optional) {
    await test(`${key} undefined INSERT NULL CONFLICT preserve explicit NULL clear`, async () => {
      const state = filled(); state[key] = undefined; await save(state);
      const column = (strings as Record<string, string>)[key] ?? (dates as Record<string, string>)[key];
      equal(row()![column], null);
      await save(filled()); const before = row()!; await save(state); equal(row(), before);
      await save({ ...state, [key]: null }); equal(row(), { ...before, [column]: null });
    });
  }
  await test("all nullable fields clear together hash bytes survive unknown getters", async () => {
    await save(filled());
    db.exec("UPDATE main.node_state SET enrollment_token_hash='synthetic-ignored-test-only'");
    const state: Record<string, unknown> = { ...minimal() };
    for (const key of optional) state[key] = null;
    Object.defineProperty(state, "enrollmentTokenHash", { get() { assert.fail("must not inspect ignored hash"); } });
    Object.defineProperty(state, "unknown", { get() { assert.fail("must not inspect unknown field"); } });
    await save(state);
    for (const column of [...Object.values(strings), ...Object.values(dates).filter(column => column !== "updated_at")]) equal(row()![column], null);
    equal(row()!.enrollment_token_hash, "synthetic-ignored-test-only");
    const other = { ...minimal("new"), enrollmentTokenHash: "synthetic-never-written" };
    await save(other); equal(row(db, "new")!.enrollment_token_hash, null);
  });
  for (const key of ["syncEnabled", "consecutiveFailures"]) {
    await test(`${key} undefined default preserve and explicit null NOT NULL`, async () => {
      await save({ ...filled(), [key]: undefined });
      const column = key === "syncEnabled" ? "sync_enabled" : "consecutive_failures";
      equal(row()![column], 0n); await save(filled()); const before = row();
      await save({ ...minimal(), [key]: undefined }); equal(row(), { ...before, updated_at: stamp(0) });
      await unchanged({ ...minimal(), [key]: null }, /NOT NULL/);
    });
  }
  await test("required Date undefined fails INSERT and even existing CONFLICT", async () => {
    for (const updatedAt of [undefined, null]) {
      await unchanged({ id: "default", syncEnabled: true, updatedAt }, /NOT NULL/);
      await save(filled()); await unchanged({ id: "default", syncEnabled: false, updatedAt }, /NOT NULL/);
      db.exec("DELETE FROM main.node_state");
    }
  });
  await test("all undefined SET rejects before prepare including closed connection", async () => {
    const seam = facade();
    await rejects(() => save({ id: "default" }, seam.connection), /No values to set/); equal(seam.counts(), [0, 0, 0]);
    const closed = openSQLite(path); closed.close();
    await rejects(() => save({ id: "default" }, closed), /No values to set/);
    await rejects(() => save(minimal(), closed)); await save(minimal());
  });
  for (const [key, column] of Object.entries(dates)) {
    await test(`${key} millisecond negative finite Date boundaries and invalid preSQL`, async () => {
      for (const ms of [-8640000000000000, -1001, -1000, -1, 0, 1, 999, 1000, 8640000000000000]) {
        await save({ ...minimal(), [key]: new Date(ms) }); equal(row()![column], stamp(ms));
        equal((await getSQLiteNodeStateRecord(db))![key as keyof typeof dates]!.getTime(), ms);
      }
      const seam = facade();
      await unchanged({ ...minimal(), [key]: new Date(NaN) }, /Invalid (Date|time)/, seam.connection);
      equal(seam.counts(), [0, 0, 0]);
      await unchanged({ ...minimal(), [key]: "not Date" });
    });
  }
  await test("Date encoder fixed order custom ISO milliseconds and original errors", async () => {
    const order: string[] = [], state = filled(), error = new Error("ISO error");
    for (const key of Object.keys(dates)) state[key] = { toISOString() { order.push(key); return "1969-12-31T23:59:59.999Z"; } };
    await save(state); equal(order, Object.keys(dates));
    equal(row()!.enrolled_at, stamp(-1));
    state.enrolledAt = { toISOString() { throw error; } }; const seam = facade();
    await unchanged(state, error, seam.connection); equal(seam.counts(), [0, 0, 0]);
    state.enrolledAt = null; state.lastSeenAt = { toISOString() { throw error; } };
    await unchanged(state, error, seam.connection); equal(seam.counts(), [0, 0, 0]);
  });
  await test("canonical booleans signed int32 edges and bounded input rejection", async () => {
    for (const syncEnabled of [true, false]) {
      for (const consecutiveFailures of [-2147483648, -1, -0, 0, 2147483647]) {
        await save({ ...minimal(), syncEnabled, consecutiveFailures });
        equal(row()!.sync_enabled, syncEnabled ? 1n : 0n); equal(row()!.consecutive_failures, BigInt(consecutiveFailures));
      }
    }
    for (const consecutiveFailures of [NaN, Infinity, -Infinity, 1.5, -2147483649, 2147483648, "0", 0n]) {
      const seam = facade(); await unchanged({ ...minimal(), consecutiveFailures }, /int32/, seam.connection); equal(seam.counts(), [0, 0, 0]);
    }
    for (const syncEnabled of [0, 1, "true", "false", {}]) {
      const seam = facade(); await unchanged({ ...minimal(), syncEnabled }, /boolean/, seam.connection); equal(seam.counts(), [0, 0, 0]);
    }
  });
  await test("raw text and native UUID constraints no parser coercion", async () => {
    for (const key of Object.keys(strings)) {
      const column = (strings as Record<string, string>)[key];
      if (column.endsWith("_id")) {
        for (const value of [uuid.toUpperCase(), "bad", `{${uuid}}`]) await unchanged({ ...minimal(), [key]: value });
      } else {
        for (const value of ["", "  MiXeD 雪\n' ?  "]) {
          await save({ ...minimal(), [key]: value }); equal(row()![column], value);
        }
      }
      await unchanged({ ...minimal(), [key]: "bad\0text" });
    }
  });
  await test("prepare step and finalizer error identity private cursor", async () => {
    for (const stage of ["prepare", "run", "finalize"] as const) {
      const error = new Error(stage), seam = facade({ [stage]: error }, false);
      await rejects(() => save(minimal(), seam.connection), error);
      equal(seam.counts(), stage === "prepare" ? [1, 0, 0] : [1, 1, 1]);
    }
    await save(minimal());
  });
  await test("MAIN TEMP shadow and caller cached statement survives", async () => {
    db.exec("CREATE TEMP TABLE node_state(id TEXT,updated_at TEXT); INSERT INTO temp.node_state VALUES('default','shadow')");
    const cached = db.query("SELECT id FROM main.node_state ORDER BY id");
    try {
      equal(cached.all(), []); await save(minimal()); equal(cached.all(), [{ id: "default" }]);
      await save({ ...minimal(), primaryUrl: "new" }); equal(cached.all(), [{ id: "default" }]);
      equal(db.query("SELECT * FROM temp.node_state").all(), [{ id: "default", updated_at: "shadow" }]);
    } finally { db.exec("DROP TABLE temp.node_state"); }
  });
  await test("caller BEGIN savepoint create update invisible rollback reopen", async () => {
    await save(filled()); const before = row(), other = openSQLite(path);
    db.exec("BEGIN IMMEDIATE; SAVEPOINT caller_save");
    try {
      equal(await save({ ...minimal(), primaryUrl: "pending" }), undefined); equal(db.inTransaction, true);
      await save(minimal("pending-new")); equal(row()!.primary_url, "pending"); equal(row(other), before); equal(row(other, "pending-new"), null);
      db.exec("ROLLBACK TO caller_save; RELEASE caller_save"); equal(row(), before); equal(row(db, "pending-new"), null);
      await save({ ...minimal(), primaryUrl: "second pending" }); equal(row(other), before);
    } finally { if (db.inTransaction) db.exec("ROLLBACK"); other.close(); }
    equal(row(), before);
    const reopened = openSQLite(path); try { equal(row(reopened), before); } finally { reopened.close(); }
    await save({ ...minimal(), primaryUrl: "success" });
  });

  // A fixture-only deferred FK makes a real successful native UPSERT fail at COMMIT.
  db.exec("CREATE TABLE node_save_parent(id TEXT PRIMARY KEY); CREATE TABLE node_save_guard(id TEXT REFERENCES node_save_parent(id) DEFERRABLE INITIALLY DEFERRED)");
  const trigger = (kind: "INSERT" | "UPDATE") => db.exec(`CREATE TEMP TRIGGER node_save_commit AFTER ${kind} ON main.node_state BEGIN INSERT INTO node_save_guard VALUES('missing'); END`);
  for (const kind of ["INSERT", "UPDATE"] as const) {
    await test(`${kind} implicit COMMIT failure full statement rollback cursor recovery`, async () => {
      if (kind === "UPDATE") await save(filled());
      const before = all(), seam = facade(), changes = db.query("SELECT total_changes() AS n").get() as { n: bigint };
      trigger(kind);
      try {
        let returned = false;
        await rejects(async () => { await save({ ...minimal(), primaryUrl: "must rollback" }, seam.connection); returned = true; }, /FOREIGN KEY/);
        equal(returned, false); equal(seam.counts(), [1, 1, 1]); equal(db.inTransaction, false);
        equal(all(), before); equal(db.query("SELECT * FROM node_save_guard").all(), []);
        ok((db.query("SELECT total_changes() AS n").get() as { n: bigint }).n > changes.n);
        const reopened = openSQLite(path); try { equal(row(reopened), before[0] ?? null); } finally { reopened.close(); }
      } finally { db.exec("DROP TRIGGER temp.node_save_commit"); }
      await save({ ...minimal(), primaryUrl: "recovered" }); equal(row()!.primary_url, "recovered");
    });
  }
  await test("caller void stays pending external COMMIT can fail caller rollback restores", async () => {
    await save(filled()); const before = row(), other = openSQLite(path); trigger("UPDATE"); db.exec("BEGIN IMMEDIATE");
    try {
      equal(await save({ ...minimal(), primaryUrl: "pending FK" }), undefined); equal(db.inTransaction, true);
      equal(row()!.primary_url, "pending FK"); equal(row(other), before);
      assert.throws(() => db.exec("COMMIT"), /FOREIGN KEY/); units++; equal(db.inTransaction, true);
      db.exec("ROLLBACK"); equal(row(), before); equal(db.query("SELECT * FROM node_save_guard").all(), []);
    } finally { if (db.inTransaction) db.exec("ROLLBACK"); db.exec("DROP TRIGGER temp.node_save_commit"); other.close(); }
    await save(minimal());
  });
  await test("finalizer fault AFTER actual autocommit rejects durable write honestly", async () => {
    const error = new Error("post-native finalize"), seam = facade({ finalize: error });
    await rejects(() => save({ ...minimal(), primaryUrl: "durable" }, seam.connection), error);
    equal(seam.counts(), [1, 1, 1]); equal(db.inTransaction, false); equal(row()!.primary_url, "durable");
    const reopened = openSQLite(path); try { equal(row(reopened)!.primary_url, "durable"); } finally { reopened.close(); }
  });
  await test("BUSY preserves full row then release conflicting UPSERT whole-row visibility", async () => {
    await save(filled()); const before = row(), other = openSQLite(path); db.exec("PRAGMA busy_timeout=150"); other.exec("BEGIN IMMEDIATE");
    try {
      const start = performance.now(); await unchanged({ ...minimal(), primaryUrl: "blocked", syncEnabled: false, consecutiveFailures: 123 }, /busy|locked/i);
      const elapsed = performance.now() - start; ok(elapsed >= 100 && elapsed < 2000); console.log(`BUSY ${elapsed.toFixed(2)}ms`);
      equal(row(other), before);
    } finally { other.exec("ROLLBACK"); db.exec("PRAGMA busy_timeout=5000"); }
    try {
      const state = filled(); state.primaryUrl = "first atomic"; state.consecutiveFailures = 42; state.syncEnabled = false;
      await save(state); equal(await getSQLiteNodeStateRecord(other), state);
      const second = filled(); second.primaryUrl = "second atomic"; second.consecutiveFailures = 99;
      other.exec("BEGIN IMMEDIATE"); await save(second, other); equal(await getSQLiteNodeStateRecord(db), state);
      other.exec("COMMIT"); equal(await getSQLiteNodeStateRecord(db), second);
    } finally { if (other.inTransaction) other.exec("ROLLBACK"); other.close(); }
  });
  await test("native stored infinity getter regression invalid write Date rejected", async () => {
    await save(minimal());
    for (const infinity of ["0", "2"]) {
      db.query("UPDATE main.node_state SET updated_at=?").run(infinity);
      ok(Number.isNaN((await getSQLiteNodeStateRecord(db))!.updatedAt.getTime()));
      await unchanged({ ...minimal(), updatedAt: new Date(NaN) }, /Invalid (Date|time)/); equal(row()!.updated_at, infinity);
    }
  });
  await test("native unique UUID conflict rejects whole-row replacement then recovers", async () => {
    await save(filled());
    await save({ ...minimal("other"), credentialId: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb" });
    await unchanged({ ...minimal(), credentialId: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
      primaryUrl: "must not replace", syncEnabled: false, consecutiveFailures: 123 }, /UNIQUE/);
    await save({ ...minimal(), credentialId: null, primaryUrl: "recovered" });
    equal(row()!.primary_url, "recovered"); equal(row()!.credential_id, null);
  });
  await test("ISO fractional nanoseconds reduce to source millisecond representation", async () => {
    await save({ ...minimal(), enrolledAt: { toISOString() { return "1969-12-31T23:59:59.999999999Z"; } } });
    equal(row()!.enrolled_at, stamp(-1)); equal((await getSQLiteNodeStateRecord(db))!.enrolledAt!.getTime(), -1);
  });
  await test("caller pending successful COMMIT is durable without helper ownership", async () => {
    const other = openSQLite(path); db.exec("BEGIN IMMEDIATE");
    try {
      equal(await save(minimal()), undefined); equal(row(other), null); equal(db.inTransaction, true);
      db.exec("COMMIT"); equal(row(other), row());
    } finally { if (db.inTransaction) db.exec("ROLLBACK"); other.close(); }
    const reopened = openSQLite(path); try { equal(row(reopened), row()); } finally { reopened.close(); }
  });
  await test("durable final Go native-byte oracle with synthetic hash untouched", async () => {
    db.exec("INSERT INTO main.node_state(id,updated_at,enrollment_token_hash) VALUES('default','109223372036854775808.000000000','synthetic-ignored-test-only')");
    await save(filled());
    equal(row()!.enrollment_token_hash, "synthetic-ignored-test-only");
    const reopened = openSQLite(path); try { equal(row(reopened), row()); } finally { reopened.close(); }
  });
  console.log(`COMPLETE ${units} assertion units ${scenarios} scenarios`);
} finally { if (db.inTransaction) db.exec("ROLLBACK"); db.close(); }
