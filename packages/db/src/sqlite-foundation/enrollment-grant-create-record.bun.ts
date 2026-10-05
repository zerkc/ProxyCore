import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import { createSQLiteEnrollmentGrantRecord } from "./enrollment-grant-create-record.js";
import type { EnrollmentGrantRecord, ContinuityTransactionPort } from "../ports.js";

const path = process.argv[2];
function open() {
  const connection = new Database(path, { strict: true, safeIntegers: true });
  connection.exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=0");
  return connection;
}
const db = open();
const id = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const reference = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
const token = "cccccccc-cccc-cccc-cccc-cccccccccccc";
const second = "dddddddd-dddd-dddd-dddd-dddddddddddd";
const third = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee";
const missing = "ffffffff-ffff-ffff-ffff-ffffffffffff";
const fields = ["attemptId", "tokenId", "installationId", "nodeId", "primaryId",
  "primaryGeneration", "verifiedPreviewDigest", "nodeEphemeralPublicKey",
  "sealedBootstrapPayload", "payloadHash", "createdAt", "expiresAt"] as const;
const columns = ["attempt_id", "token_id", "installation_id", "node_id", "primary_id",
  "primary_generation", "verified_preview_digest", "node_ephemeral_public_key",
  "sealed_bootstrap_payload", "payload_hash", "created_at", "expires_at"];
const required = fields.filter(key => key !== "verifiedPreviewDigest");
const uuidFields = ["attemptId", "tokenId", "installationId", "nodeId", "primaryId"];
const rawTextFields = ["verifiedPreviewDigest", "nodeEphemeralPublicKey", "sealedBootstrapPayload", "payloadHash"];
const created = "109223372036854775807.999000000";
const expires = "109223372036854775808.001000000";
const canonical: (db: Database, grant: Parameters<ContinuityTransactionPort["createEnrollmentGrant"]>[0]) => Promise<void>
  = createSQLiteEnrollmentGrantRecord;
function filled(): Record<string, unknown> {
  return {
    attemptId: id, tokenId: token,
    installationId: reference, nodeId: reference, primaryId: reference,
    primaryGeneration: 9007199254740992,
    verifiedPreviewDigest: "  digest 雪 🦊  ",
    nodeEphemeralPublicKey: "  public-key 雪  ",
    sealedBootstrapPayload: "  sealed-payload 雪  ",
    payloadHash: "  payload-hash 雪  ",
    createdAt: new Date(-1), expiresAt: new Date(1),
  };
}
const write = (value: object, connection = db) => canonical(connection, value as EnrollmentGrantRecord);
let units = 0, scenarios = 0;
function equal(actual: unknown, expected: unknown) { assert.deepEqual(actual, expected); units++; }
function ok(value: unknown) { assert.ok(value); units++; }
async function rejects(body: () => Promise<unknown>, expected?: Error | RegExp) {
  let caught: unknown;
  try { await body(); } catch (error) { caught = error; }
  ok(caught instanceof Error);
  if (expected instanceof Error) equal(caught, expected);
  else if (expected) { assert.match((caught as Error).message, expected); units++; }
}
function all(connection = db) {
  const statement = connection.prepare(`SELECT ${columns.join(", ")} FROM main.enrollment_grants ORDER BY attempt_id`);
  try { return statement.values(); } finally { statement.finalize(); }
}
function exec(sql: string) { db.exec(sql); }
async function unchanged(value: object, expected?: Error | RegExp, connection = db) {
  const before = all();
  await rejects(() => write(value, connection), expected);
  equal(all(), before);
}
async function test(name: string, body: () => Promise<void>) {
  exec("DELETE FROM main.enrollment_grants");
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
      ok(text.startsWith("INSERT INTO main.enrollment_grants "));
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
    query() { assert.fail("no caller cache access"); },
    exec() { assert.fail("no other SQL or owned transaction"); },
    close() { assert.fail("caller owns database"); },
    transaction() { assert.fail("caller owns transaction"); },
    get inTransaction() { return assert.fail("no caller nesting guard"); },
  } as unknown as Database;
  return { connection, counts: () => [prepares, runs, finalizes], sql: () => sql, bindings: () => bindings };
}
try {
  await test("canonical twelve fields, supplied dates, void, exact order, durable reopen", async () => {
    const seam = facade();
    equal(await write({ ...filled(), ignored: "not SQL" }, seam.connection), undefined);
    equal(seam.counts(), [1, 1, 1]);
    equal(seam.sql(), `INSERT INTO main.enrollment_grants (${columns.join(", ")}) VALUES (${columns.map(() => "?").join(", ")})`);
    const expected = fields.map(key => filled()[key]);
    expected[5] = 9007199254740992n;
    expected[10] = created; expected[11] = expires;
    equal(seam.bindings(), expected);
    equal(all(), [expected]);
    const reopened = open();
    try { equal(all(reopened), [expected]); } finally { reopened.close(); }
  });
  await test("undefined digest omits to native NULL, explicit NULL binds, empty and Unicode untouched", async () => {
    for (const value of [undefined, null, "", "雪 🦊 é ? '  "]) {
      exec("DELETE FROM main.enrollment_grants");
      const seam = facade();
      await write({ ...filled(), verifiedPreviewDigest: value }, seam.connection);
      equal(all()[0]![6], value === undefined ? null : value);
      equal(seam.sql().includes("verified_preview_digest"), value !== undefined);
      equal(seam.bindings().length, value === undefined ? 11 : 12);
      if (value === null) equal(seam.bindings()[6], null);
      equal(seam.counts(), [1, 1, 1]);
    }
  });
  await test("every required undefined omits DEFAULT and native NOT NULL rejects; no generated dates", async () => {
    for (const key of required) {
      const seam = facade();
      await unchanged({ ...filled(), [key]: undefined }, /NOT NULL/, seam.connection);
      equal(seam.sql().includes(columns[fields.indexOf(key)]!), false);
      equal(seam.bindings().length, 11);
      equal(seam.counts(), [1, 1, 1]);
    }
    const seam = facade();
    await unchanged({}, /NOT NULL/, seam.connection);
    equal(seam.sql(), "INSERT INTO main.enrollment_grants DEFAULT VALUES");
    equal(seam.bindings(), []);
  });
  await test("every required explicit NULL bypasses encoder and rejects at native constraints", async () => {
    for (const key of required) {
      const seam = facade();
      await unchanged({ ...filled(), [key]: null }, /NOT NULL/, seam.connection);
      equal(seam.bindings()[fields.indexOf(key)], null);
      equal(seam.counts(), [1, 1, 1]);
    }
  });
  await test("invalid Dates reject before any prepare; custom encoder error identity", async () => {
    for (const key of ["createdAt", "expiresAt"]) {
      const seam = facade();
      await unchanged({ ...filled(), [key]: new Date(NaN) }, /Invalid Date|Invalid time value/, seam.connection);
      equal(seam.counts(), [0, 0, 0]);
      const error = new Error(`synthetic ${key} ISO failure`);
      await unchanged({ ...filled(), [key]: { toISOString() { throw error; } } }, error, seam.connection);
      equal(seam.counts(), [0, 0, 0]);
    }
  });
  await test("ISO calls precede prepare, source unchanged, floor negative milliseconds", async () => {
    const order: string[] = [];
    const value = filled();
    value.createdAt = { toISOString() { order.push("created ISO"); return new Date(-1001).toISOString(); } };
    value.expiresAt = { toISOString() { order.push("expires ISO"); return new Date(-999).toISOString(); } };
    const seam = facade();
    const original = seam.connection.prepare.bind(seam.connection);
    seam.connection.prepare = ((sql: string) => { order.push("prepare"); return original(sql); }) as Database["prepare"];
    await write(value, seam.connection);
    equal(order, ["created ISO", "expires ISO", "prepare"]);
    equal(seam.bindings().slice(10), ["109223372036854775806.999000000", "109223372036854775807.001000000"]);
    ok(typeof (value.createdAt as Date).toISOString === "function");
  });
  await test("Date ISO extremes encoded as supplied, no current clock substitution", async () => {
    const seam = facade();
    await write({ ...filled(), createdAt: new Date(-8640000000000000), expiresAt: new Date(8640000000000000) }, seam.connection);
    equal(seam.bindings().slice(10), ["109223363396854775808.000000000", "109223380676854775808.000000000"]);
    equal(all()[0]!.slice(10), seam.bindings().slice(10));
  });
  await test("signed int64 represented bounds and unsafe numbers preserved without extra rounding", async () => {
    const values = [0, -0, -1, 1, Number.MAX_SAFE_INTEGER, Number.MAX_SAFE_INTEGER + 1,
      Number.MAX_SAFE_INTEGER + 2, -(2 ** 63), 2 ** 63 - 1024];
    for (const value of values) {
      exec("DELETE FROM main.enrollment_grants");
      const seam = facade();
      await write({ ...filled(), primaryGeneration: value }, seam.connection);
      equal(seam.bindings()[5], BigInt(value));
      equal(all()[0]![5], BigInt(value));
    }
    // Canonical number cannot represent +int64 max; conversion rounds it to +2^63.
    equal(Number((1n << 63n) - 1n), 2 ** 63);
    equal(Number(-(1n << 63n) - 1n), -(2 ** 63));
  });
  await test("rounded upper bound, below lower bound, fractions NaN infinities reject before SQL", async () => {
    for (const value of [Number((1n << 63n) - 1n), 2 ** 63, -(2 ** 63) - 2048,
      0.5, -1.5, NaN, Infinity, -Infinity, "1", 1n]) {
      const seam = facade();
      await unchanged({ ...filled(), primaryGeneration: value }, /primaryGeneration/, seam.connection);
      equal(seam.counts(), [0, 0, 0]);
    }
  });
  await test("noncanonical text identity not coerced or narrowed before native binding", async () => {
    for (const key of [...uuidFields, ...rawTextFields]) {
      const identity = { synthetic: key };
      const seam = facade({}, false);
      await write({ ...filled(), [key]: identity }, seam.connection);
      equal(seam.bindings()[fields.indexOf(key as typeof fields[number])], identity);
      equal(seam.counts(), [1, 1, 1]);
    }
    equal(all(), []);
  });
  await test("native UUID shape and lowercase checks each identifier", async () => {
    for (const key of uuidFields) {
      for (const value of ["not-a-uuid", reference.toUpperCase(), reference + " ", ""]) {
        const seam = facade();
        await unchanged({ ...filled(), [key]: value }, /CHECK/, seam.connection);
        equal(seam.bindings()[fields.indexOf(key as typeof fields[number])], value);
        equal(seam.counts(), [1, 1, 1]);
      }
    }
  });
  await test("native text NUL checks and raw empty Unicode required text", async () => {
    for (const key of rawTextFields) {
      await unchanged({ ...filled(), [key]: "synthetic\0NUL" }, /CHECK/);
      for (const value of ["", "  雪 🦊 ? '  "]) {
        exec("DELETE FROM main.enrollment_grants");
        await write({ ...filled(), [key]: value });
        equal(all()[0]![fields.indexOf(key as typeof fields[number])], value);
      }
      exec("DELETE FROM main.enrollment_grants");
    }
  });
  await test("native expiry ordering rejects equality and earlier expiresAt", async () => {
    for (const expiresAt of [new Date(-1), new Date(-2)]) {
      const seam = facade();
      await unchanged({ ...filled(), expiresAt }, /CHECK/, seam.connection);
      equal(seam.counts(), [1, 1, 1]);
    }
  });
  await test("native PK unique-token FK errors preserve earlier grant and finalize", async () => {
    await write(filled());
    for (const value of [filled(), { ...filled(), attemptId: second },
      { ...filled(), attemptId: second, tokenId: missing }]) {
      const seam = facade();
      await unchanged(value, /UNIQUE|FOREIGN KEY/, seam.connection);
      equal(seam.counts(), [1, 1, 1]);
    }
    const seam = facade();
    await write({ ...filled(), attemptId: second, tokenId: second }, seam.connection);
    equal(all().length, 2);
    equal(seam.counts(), [1, 1, 1]);
  });
  await test("prepare/run error identities, no unintended INSERT, no caller lifecycle", async () => {
    for (const phase of ["prepare", "run"] as const) {
      const error = new Error(`synthetic ${phase} failure`);
      const seam = facade({ [phase]: error });
      await unchanged(filled(), error, seam.connection);
      equal(seam.counts(), phase === "prepare" ? [1, 0, 0] : [1, 1, 1]);
    }
    const closed = open(); closed.close();
    await unchanged(filled(), undefined, closed);
    await write(filled());
    equal(all().length, 1);
  });
  await test("native query error identity after real prepare and private finalization", async () => {
    await write(filled());
    let nativeError: unknown, finalizes = 0;
    const connection = {
      prepare(sql: string) {
        const statement = db.prepare(sql);
        return {
          run(...bindings: any[]) {
            try { statement.run(...bindings); } catch (error) { nativeError = error; throw error; }
          },
          finalize() { statement.finalize(); finalizes++; },
        };
      },
    } as unknown as Database;
    const before = all();
    let propagated: unknown;
    try { await write(filled(), connection); } catch (error) { propagated = error; }
    ok(nativeError instanceof Error);
    equal(propagated, nativeError);
    equal(finalizes, 1);
    equal(all(), before);
  });
  await test("finalizer failure AFTER native finalize rejects durable autocommit insert", async () => {
    const error = new Error("synthetic post-native-finalize failure");
    const seam = facade({ finalize: error });
    await rejects(() => write(filled(), seam.connection), error);
    equal(seam.counts(), [1, 1, 1]);
    equal(db.inTransaction, false);
    equal(all().length, 1);
    const other = open();
    try { equal(all(other), all()); } finally { other.close(); }
    // A new private statement still runs after the previous one was finalized.
    await write({ ...filled(), attemptId: second, tokenId: second });
    equal(all().length, 2);
  });
  await test("finalizer error priority remains local when native query also fails", async () => {
    await write(filled());
    const error = new Error("synthetic finalizer overrides native constraint error");
    const seam = facade({ finalize: error });
    await unchanged(filled(), error, seam.connection);
    equal(seam.counts(), [1, 1, 1]);
  });
  await test("cached caller statements survive private success and native rejection", async () => {
    const cached = db.query("SELECT count(*) FROM main.enrollment_grants");
    equal(cached.values(), [[0n]]);
    await write(filled());
    equal(db.query("SELECT count(*) FROM main.enrollment_grants"), cached);
    equal(cached.values(), [[1n]]);
    await unchanged(filled(), /UNIQUE/);
    equal(cached.values(), [[1n]]);
    cached.finalize();
    equal(all().length, 1);
  });
  await test("caller transaction pending visibility, savepoint rollback and outer rollback", async () => {
    const other = open();
    exec("BEGIN IMMEDIATE");
    try {
      await write(filled());
      equal(db.inTransaction, true);
      equal(all().length, 1); equal(all(other), []);
      exec("SAVEPOINT caller_owned");
      await write({ ...filled(), attemptId: second, tokenId: second });
      equal(all().length, 2); equal(all(other), []);
      await unchanged(filled(), /UNIQUE/);
      equal(db.inTransaction, true);
      exec("ROLLBACK TO caller_owned; RELEASE caller_owned");
      equal(all().length, 1);
      exec("ROLLBACK");
      equal(all(), []); equal(all(other), []);
    } finally {
      if (db.inTransaction) exec("ROLLBACK");
      other.close();
    }
    const reopened = open();
    try { equal(all(reopened), []); } finally { reopened.close(); }
  });
  await test("caller actual COMMIT publishes insert after released savepoint and reopen", async () => {
    const other = open();
    exec("BEGIN IMMEDIATE; SAVEPOINT caller_commit");
    try {
      await write(filled());
      exec("RELEASE caller_commit");
      equal(db.inTransaction, true); equal(all(other), []);
      exec("COMMIT");
      equal(db.inTransaction, false); equal(all(other), all());
    } finally {
      if (db.inTransaction) exec("ROLLBACK");
      other.close();
    }
    const reopened = open();
    try { equal(all(reopened), all()); } finally { reopened.close(); }
  });
  await test("post-native-finalizer rejection leaves caller pending INSERT rollback owned", async () => {
    const other = open(), error = new Error("synthetic pending finalizer failure");
    exec("BEGIN IMMEDIATE; SAVEPOINT caller_finalizer");
    try {
      const seam = facade({ finalize: error });
      await rejects(() => write(filled(), seam.connection), error);
      equal(seam.counts(), [1, 1, 1]);
      equal(db.inTransaction, true); equal(all().length, 1); equal(all(other), []);
      exec("ROLLBACK TO caller_finalizer; RELEASE caller_finalizer; ROLLBACK");
      equal(all(), []); equal(all(other), []);
    } finally {
      if (db.inTransaction) exec("ROLLBACK");
      other.close();
    }
  });
  await test("writer targets MAIN despite caller TEMP shadow", async () => {
    exec("CREATE TEMP TABLE enrollment_grants (caller_marker TEXT)");
    try {
      await write(filled());
      const cursor = db.prepare("SELECT * FROM temp.enrollment_grants");
      try { equal(cursor.values(), []); } finally { cursor.finalize(); }
      equal(all().length, 1);
    } finally { exec("DROP TABLE temp.enrollment_grants"); }
  });
  await test("native busy error recovery preserves caller connection and ownership", async () => {
    const other = open(), seam = facade();
    other.exec("BEGIN IMMEDIATE");
    try {
      await unchanged(filled(), /locked|busy/i, seam.connection);
      equal(seam.counts(), [1, 1, 1]);
    } finally { other.exec("ROLLBACK"); other.close(); }
    await write(filled());
    equal(all().length, 1);
  });
  // Leave exactly the complete synthetic grant for independent Go raw-field inspection.
  exec("DELETE FROM main.enrollment_grants");
  await write(filled());
  equal(all().length, 1);
  const foreign = db.prepare("PRAGMA foreign_key_check");
  try { equal(foreign.values(), []); } finally { foreign.finalize(); }
  const reopened = open();
  try { equal(all(reopened), all()); } finally { reopened.close(); }
  console.log(`enrollment-grant-create checks=${units} scenarios=${scenarios}`);
} finally { db.close(); }
