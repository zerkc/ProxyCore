import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import { createSQLiteEnrollmentAttemptRecord } from "./enrollment-attempt-create-record.js";
import type { EnrollmentAttemptRecord, ContinuityTransactionPort } from "../ports.js";

type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2) ? true : false
    : false;
type Expect<T extends true> = T;
type CanonicalInput = Expect<Identical<Parameters<typeof createSQLiteEnrollmentAttemptRecord>[1], EnrollmentAttemptRecord>>;
type CanonicalReturn = Expect<Identical<ReturnType<typeof createSQLiteEnrollmentAttemptRecord>,
  ReturnType<ContinuityTransactionPort["createEnrollmentAttempt"]>>>;

const path = process.argv[2];
function open() {
  const connection = new Database(path, { strict: true, safeIntegers: true });
  connection.exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=0");
  return connection;
}
const db = open();
const id = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const reference = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
const missing = "cccccccc-cccc-cccc-cccc-cccccccccccc";
const raw = "  Raw 雪 ? '  ";
const states = ["draft", "verified", "confirmed", "exchanged", "archived", "initial-apply-pending",
  "committed", "cancelled", "recoverable", "failed"];
const fields = ["id", "state", "primaryUrl", "expectedPrimaryId", "verifiedPrimaryId", "verifiedPrimaryNodeId",
  "verifiedLeadershipGeneration", "verifiedPrimaryTlsSpkiSha256", "verifiedPrimaryCaFingerprint", "previewDigest",
  "localNodeIp", "archiveId", "ephemeralPrivateKeyWrapped", "bootstrapPayload", "nodeCredentialSecretId",
  "clusterKeyId", "initialSnapshotHash", "initialSnapshotRevisionId", "initialApplyJobId", "failureCode",
  "confirmedAt", "createdAt", "updatedAt"];
const columns = ["id", "state", "primary_url", "expected_primary_id", "verified_primary_id", "verified_primary_node_id",
  "verified_leadership_generation", "verified_primary_tls_spki_sha256", "verified_primary_ca_fingerprint", "preview_digest",
  "local_node_ip", "archive_id", "ephemeral_private_key_wrapped", "bootstrap_payload", "node_credential_secret_id",
  "cluster_key_id", "initial_snapshot_hash", "initial_snapshot_revision_id", "initial_apply_job_id", "failure_code",
  "confirmed_at", "created_at", "updated_at"];
const optional = fields.filter(key => !["id", "state", "primaryUrl", "localNodeIp", "ephemeralPrivateKeyWrapped", "createdAt", "updatedAt"].includes(key));
const foreign = ["archiveId", "nodeCredentialSecretId", "clusterKeyId", "initialSnapshotRevisionId", "initialApplyJobId"];
const uuidFields = ["id", "expectedPrimaryId", "verifiedPrimaryId", "verifiedPrimaryNodeId", ...foreign];
const textFields = fields.filter(key => !["verifiedLeadershipGeneration", "confirmedAt", "createdAt", "updatedAt", "bootstrapPayload"].includes(key));
const minimal = (): Record<string, unknown> => ({ id, state: "committed", primaryUrl: raw,
  localNodeIp: "127.0.0.42", ephemeralPrivateKeyWrapped: "wrapped synthetic only", createdAt: new Date(0), updatedAt: new Date(1) });
function filled(): Record<string, unknown> {
  return { ...minimal(), expectedPrimaryId: reference, verifiedPrimaryId: reference, verifiedPrimaryNodeId: reference,
    verifiedLeadershipGeneration: 9007199254740992, verifiedPrimaryTlsSpkiSha256: "tls synthetic",
    verifiedPrimaryCaFingerprint: "ca synthetic", previewDigest: "preview synthetic", archiveId: reference,
    bootstrapPayload: new TextEncoder().encode("Bootstrap 雪"), nodeCredentialSecretId: reference, clusterKeyId: reference,
    initialSnapshotHash: "snapshot synthetic", initialSnapshotRevisionId: reference, initialApplyJobId: reference,
    failureCode: "failure synthetic", confirmedAt: new Date(-1) };
}
const save = (attempt: object, connection = db) => createSQLiteEnrollmentAttemptRecord(connection, attempt as EnrollmentAttemptRecord);
let units = 0, scenarios = 0;
function equal(actual: unknown, expected: unknown) { assert.deepEqual(actual, expected); units++; }
function ok(value: unknown) { assert.ok(value); units++; }
async function rejects(body: () => Promise<unknown>, expected?: Error | RegExp) {
  try { await body(); } catch (error) {
    if (expected instanceof Error) equal(error, expected);
    else if (expected) { assert.match(String(error), expected); units++; }
    else ok(error instanceof Error);
    return error;
  }
  assert.fail("expected rejection");
}
const all = (connection = db) => connection.query("SELECT * FROM main.enrollment_attempts ORDER BY id").all();
const row = (connection = db) => connection.query("SELECT * FROM main.enrollment_attempts WHERE id=?").get(id) as Record<string, unknown> | null;
async function unchanged(attempt: object, expected?: Error | RegExp, connection = db) {
  const before = all();
  await rejects(() => save(attempt, connection), expected);
  equal(all(), before);
}
async function test(name: string, body: () => Promise<void>) {
  db.exec("DELETE FROM main.enrollment_attempts");
  await body();
  scenarios++;
  console.log(`PASS ${name}`);
}
function facade(errors: { prepare?: Error; run?: Error; finalize?: Error } = {}, native = true) {
  let prepares = 0, runs = 0, finalizes = 0;
  let sql = "", bindings: unknown[] = [];
  const connection = {
    prepare(text: string) {
      prepares++; sql = text;
      ok(text.startsWith("INSERT INTO main.enrollment_attempts ("));
      ok(!/RETURNING|SELECT|CONFLICT|BEGIN|COMMIT/i.test(text));
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
    query() { assert.fail("no caller cache access"); }, exec() { assert.fail("no other SQL or owned transaction"); },
    close() { assert.fail("caller owns database"); }, transaction() { assert.fail("caller owns transaction"); },
    get inTransaction() { return assert.fail("no caller nesting guard"); },
  } as unknown as Database;
  return { connection, counts: () => [prepares, runs, finalizes], sql: () => sql, bindings: () => bindings };
}

function seed(sql: string, ...bindings: (string | number)[]) {
  const statement = db.prepare(sql);
  try { statement.run(...bindings); } finally { statement.finalize(); }
}

try {
  // Synthetic, disposable references only. No real ciphertext, key or archive data.
  seed("INSERT INTO secrets(id,purpose,ciphertext,created_at,updated_at) VALUES(?,?,?,?,?)",
    reference, "fixture", "synthetic inert ciphertext", "0", "0");
  seed("INSERT INTO cluster_keys(id,purpose,wrapped_kek,wrapping_key_version,created_at) VALUES(?,?,?,?,?)",
    reference, "fixture", "synthetic inert wrapper", 1, "0");
  seed("INSERT INTO config_revisions(id,revision_number,checksum,snapshot,created_at) VALUES(?,?,?,?,?)",
    reference, 1, "synthetic fixture checksum", "{}", "0");
  seed("INSERT INTO apply_jobs(id,revision_id,target,correlation_id,created_at) VALUES(?,?,?,?,?)",
    reference, reference, "combined", "synthetic correlation", "0");
  seed("INSERT INTO standalone_archives(id,archive_blob,capture_reason,captured_at,created_at) VALUES(?,?,?,?,?)",
    reference, "synthetic inert archive", "fixture", "0", "0");

  await test("canonical 23 fields exact bind order provided ID Dates void and reopen", async () => {
    const seam = facade();
    equal(await save(filled(), seam.connection), undefined);
    equal(seam.counts(), [1, 1, 1]);
    equal(seam.sql(), `INSERT INTO main.enrollment_attempts (${columns.join(", ")}) VALUES (${columns.map(() => "?").join(", ")})`);
    const expected = [id, "committed", raw, reference, reference, reference, 9007199254740992n,
      "tls synthetic", "ca synthetic", "preview synthetic", "127.0.0.42", reference, "wrapped synthetic only",
      "Bootstrap 雪", reference, reference, "snapshot synthetic", reference, reference, "failure synthetic",
      "109223372036854775807.999000000", "109223372036854775808.000000000", "109223372036854775808.001000000"];
    equal(seam.bindings(), expected);
    equal(columns.map(column => row()![column]), expected);
    const reopened = open();
    try { equal(all(reopened), all()); } finally { reopened.close(); }
    equal(Object.keys(row()!).length, 23);
  });
  for (const state of states) {
    await test(`valid state ${state}`, async () => {
      equal(await save({ ...minimal(), state }), undefined);
      equal(row()!.state, state);
    });
  }
  await test("invalid state validation before closed database prepare", async () => {
    const closed = open(); closed.close();
    for (const state of ["", "Draft", "draft ", "unknown", "draft\0", null, undefined]) {
      const seam = facade();
      await unchanged({ ...minimal(), state }, new RegExp(`invalid enrollment attempt state:`), seam.connection);
      equal(seam.counts(), [0, 0, 0]);
      await rejects(() => save({ ...minimal(), state }, closed), /invalid enrollment attempt state:/);
    }
    await rejects(() => save(minimal(), closed));
    await save(minimal());
  });
  for (const property of optional) {
    await test(`optional ${property} undefined omits except bootstrap explicit NULL`, async () => {
      const seam = facade(), attempt = filled(); attempt[property] = undefined;
      await save(attempt, seam.connection);
      const column = columns[fields.indexOf(property)];
      equal(row()![column], null);
      equal(seam.sql().includes(column), property === "bootstrapPayload");
    });
    await test(`optional ${property} explicit null bypasses encoder binds NULL`, async () => {
      const seam = facade(), attempt = filled(); attempt[property] = null;
      await save(attempt, seam.connection);
      equal(row()![columns[fields.indexOf(property)]], null);
      equal(seam.bindings()[fields.indexOf(property)], null);
    });
  }
  for (const property of ["id", "primaryUrl", "localNodeIp", "ephemeralPrivateKeyWrapped", "createdAt", "updatedAt"]) {
    for (const value of [undefined, null]) {
      await test(`required ${property} ${String(value)} native constraint no invented clock`, async () => {
        const seam = facade();
        await unchanged({ ...minimal(), [property]: value }, /NOT NULL/, seam.connection);
        equal(seam.counts(), [1, 1, 1]);
      });
    }
  }
  await test("bootstrap absent null empty Unicode replacement bytes and NUL", async () => {
    for (const [input, expected] of [
      [undefined, null], [null, null], [new Uint8Array(), ""],
      [new TextEncoder().encode("雪 😀"), "雪 😀"], [new Uint8Array([0xff, 0xc3, 0x28]), "��("],
      [false, null], [0, null],
    ] as const) {
      db.exec("DELETE FROM main.enrollment_attempts");
      await save({ ...minimal(), bootstrapPayload: input });
      equal(row()!.bootstrap_payload, expected);
    }
    db.exec("DELETE FROM main.enrollment_attempts");
    await unchanged({ ...minimal(), bootstrapPayload: new Uint8Array([0xff, 0]) }, /CHECK/);
  });
  await test("Date bounds negative fractional milliseconds finite tagged native TEXT", async () => {
    const cases = [
      [-8640000000000000, "109223363396854775808.000000000"],
      [8640000000000000, "109223380676854775808.000000000"],
      [-1001, "109223372036854775806.999000000"], [-1.9, "109223372036854775807.999000000"],
      [0, "109223372036854775808.000000000"], [1.9, "109223372036854775808.001000000"],
    ] as const;
    for (const property of ["confirmedAt", "createdAt", "updatedAt"]) {
      for (const [millis, expected] of cases) {
        db.exec("DELETE FROM main.enrollment_attempts");
        await save({ ...minimal(), [property]: new Date(millis) });
        equal(row()![columns[fields.indexOf(property)]], expected);
      }
      db.exec("DELETE FROM main.enrollment_attempts");
      const seam = facade();
      const error = await rejects(() => save({ ...minimal(), [property]: new Date(NaN) }, seam.connection));
      ok(error instanceof RangeError); equal(all(), []);
      equal(seam.counts(), [0, 0, 0]);
    }
    const order: string[] = [], attempt = filled(), seam = facade();
    for (const property of ["confirmedAt", "createdAt", "updatedAt"]) {
      const date = new Date(-1);
      date.toISOString = () => { order.push(property); return "1969-12-31T23:59:59.999Z"; };
      attempt[property] = date;
    }
    await save(attempt, seam.connection);
    equal(order, ["confirmedAt", "createdAt", "updatedAt"]);
    equal(seam.counts(), [1, 1, 1]);
  });
  await test("represented-number signed int64 accepts unsafe integers without additional rounding", async () => {
    for (const value of [0, -1, 9007199254740992, -9007199254740992, -(2 ** 63), 2 ** 63 - 1024]) {
      db.exec("DELETE FROM main.enrollment_attempts");
      const seam = facade();
      await save({ ...minimal(), verifiedLeadershipGeneration: value }, seam.connection);
      equal(row()!.verified_leadership_generation, BigInt(value));
      equal(seam.bindings()[3], BigInt(value));
    }
    db.exec("DELETE FROM main.enrollment_attempts");
    for (const value of [Number(9223372036854775807n), -(2 ** 63) - 2048, 0.5, NaN, Infinity, -Infinity, "1", 1n]) {
      const seam = facade();
      await unchanged({ ...minimal(), verifiedLeadershipGeneration: value }, /leadership|int64/i, seam.connection);
      equal(seam.counts(), [0, 0, 0]);
    }
  });
  await test("native UUID shape checks every UUID field without normalization", async () => {
    for (const property of uuidFields) {
      for (const value of ["", "not-a-uuid", reference.toUpperCase(), "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"]) {
        await unchanged({ ...minimal(), [property]: value }, /CHECK/);
      }
    }
  });
  await test("raw TEXT NUL constraints every text field and STRICT storage", async () => {
    for (const property of textFields) {
      await unchanged({ ...minimal(), [property]: "raw\0tail" }, property === "state" ? /invalid enrollment/ : /CHECK/);
    }
    for (const property of ["primaryUrl", "localNodeIp", "ephemeralPrivateKeyWrapped", "previewDigest"]) {
      await unchanged({ ...minimal(), [property]: new Uint8Array([65]) }, /BLOB|TEXT|datatype/i);
    }
    await save(minimal());
    equal(row()!.primary_url, raw);
    equal(db.query("SELECT typeof(created_at) AS date_type, typeof(primary_url) AS text_type FROM main.enrollment_attempts").get(),
      { date_type: "text", text_type: "text" });
    assert.throws(() => db.exec("UPDATE main.enrollment_attempts SET created_at='not tagged'"), /CHECK/); units++;
    equal(row()!.created_at, "109223372036854775808.000000000");
  });
  await test("all five FK edges missing references atomic and successful references", async () => {
    for (const property of foreign) await unchanged({ ...minimal(), [property]: missing }, /FOREIGN KEY/);
    await save(filled());
    for (const property of foreign) equal(row()![columns[fields.indexOf(property)]], reference);
  });
  await test("partial unique one active exact state edges and duplicate ID no overwrite", async () => {
    for (const state of states) {
      db.exec("DELETE FROM main.enrollment_attempts");
      await save({ ...minimal(), state });
      const second = { ...minimal(), id: missing, state };
      if (["committed", "cancelled", "failed"].includes(state)) {
        await save(second); equal(all().length, 2);
      } else await unchanged(second, /UNIQUE/);
      await unchanged({ ...minimal(), state, primaryUrl: "do not overwrite" }, /UNIQUE/);
      equal(row()!.primary_url, raw);
    }
    db.exec("DELETE FROM main.enrollment_attempts");
    await save({ ...minimal(), state: "draft" });
    for (const state of states.filter(state => !["committed", "cancelled", "failed"].includes(state))) {
      await unchanged({ ...minimal(), id: missing, state }, /UNIQUE/);
    }
  });
  await test("prepare run finalizer exact error identity and private uncached statement", async () => {
    for (const stage of ["prepare", "run", "finalize"] as const) {
      const error = new Error(`synthetic ${stage}`), seam = facade({ [stage]: error }, false);
      await unchanged(minimal(), error, seam.connection);
      equal(seam.counts(), stage === "prepare" ? [1, 0, 0] : [1, 1, 1]);
    }
    const run = new Error("run"), finalize = new Error("finalize");
    const seam = facade({ run, finalize }, false);
    await unchanged(minimal(), finalize, seam.connection);
    equal(seam.counts(), [1, 1, 1]);
    const success = facade({}, false);
    equal(await save(filled(), success.connection), undefined);
    equal(success.counts(), [1, 1, 1]);
    equal(all(), []);
  });
  await test("Date and bootstrap source construction order precedes prepare", async () => {
    const attempt = filled(), events: string[] = [], seam = facade();
    for (const property of fields) {
      const value = attempt[property];
      Object.defineProperty(attempt, property, { get() { events.push(property); return value; } });
    }
    await save(attempt, seam.connection);
    equal(events, ["state", ...fields]);
    equal(seam.counts(), [1, 1, 1]);
  });
  await test("unknown properties ignored and caller input unchanged", async () => {
    const attempt = filled(), before = { ...attempt }, seam = facade();
    Object.defineProperty(attempt, "unexpected_sql_column", { get() { assert.fail("unknown property accessed"); } });
    await save(attempt, seam.connection);
    equal(attempt, before); equal(seam.bindings().length, 23);
    ok(!seam.sql().includes("unexpected_sql_column"));
  });
  await test("Date encoder thrown identity and out of bounds Date reject before SQL", async () => {
    for (const property of ["confirmedAt", "createdAt", "updatedAt"]) {
      const error = new Error(`synthetic ${property} ISO error`), date = new Date(0), seam = facade();
      date.toISOString = () => { throw error; };
      await unchanged({ ...minimal(), [property]: date }, error, seam.connection);
      equal(seam.counts(), [0, 0, 0]);
      for (const millis of [-8640000000000001, 8640000000000001]) {
        const rangeError = await rejects(() => save({ ...minimal(), [property]: new Date(millis) }, seam.connection));
        ok(rangeError instanceof RangeError); equal(seam.counts(), [0, 0, 0]); equal(all(), []);
      }
    }
  });
  await test("native optional defaults are NULL while state alone defaults draft", async () => {
    const metadata = db.query("PRAGMA main.table_info(enrollment_attempts)").all() as { name: string; dflt_value: string | null }[];
    equal(metadata.length, 23);
    for (const column of columns) {
      equal(metadata.find(item => item.name === column)!.dflt_value, column === "state" ? "'draft'" : null);
    }
  });
  await test("caller transaction savepoint pending visibility rollback and later commit", async () => {
    const other = open();
    try {
      db.exec("BEGIN IMMEDIATE; SAVEPOINT caller_save");
      equal(await save(minimal()), undefined);
      equal(db.inTransaction, true); equal(all(other), []); equal(all().length, 1);
      db.exec("ROLLBACK TO caller_save; RELEASE caller_save");
      equal(all(), []);
      await save({ ...minimal(), primaryUrl: "pending rollback" });
      db.exec("ROLLBACK"); equal(all(), []); equal(all(other), []);
      db.exec("BEGIN IMMEDIATE"); await save(minimal());
      equal(all(other), []); db.exec("COMMIT"); equal(all(other), all());
      const reopened = open();
      try { equal(all(reopened), all()); } finally { reopened.close(); }
    } finally { if (db.inTransaction) db.exec("ROLLBACK"); other.close(); }
  });

  // Real deferred FK violation: the INSERT and trigger execute, implicit COMMIT fails.
  db.exec("CREATE TABLE enrollment_create_parent(id TEXT PRIMARY KEY); CREATE TABLE enrollment_create_guard(id TEXT REFERENCES enrollment_create_parent(id) DEFERRABLE INITIALLY DEFERRED)");
  const trigger = () => db.exec("CREATE TEMP TRIGGER enrollment_create_commit AFTER INSERT ON main.enrollment_attempts BEGIN INSERT INTO enrollment_create_guard VALUES('missing'); END");
  await test("implicit COMMIT native execution error finalized whole INSERT rollback reopen recovery", async () => {
    const seam = facade(), before = (db.query("SELECT total_changes() AS n").get() as { n: bigint }).n;
    trigger();
    try {
      let returned = false;
      await rejects(async () => { await save(filled(), seam.connection); returned = true; }, /FOREIGN KEY/);
      equal(returned, false); equal(seam.counts(), [1, 1, 1]); equal(db.inTransaction, false);
      equal(all(), []); equal(db.query("SELECT * FROM enrollment_create_guard").all(), []);
      ok((db.query("SELECT total_changes() AS n").get() as { n: bigint }).n > before);
      const reopened = open();
      try { equal(all(reopened), []); } finally { reopened.close(); }
    } finally { db.exec("DROP TRIGGER temp.enrollment_create_commit"); }
    equal(await save(minimal()), undefined); equal(all().length, 1);
  });
  await test("successful void inside caller transaction leaves external COMMIT failure caller owned", async () => {
    const other = open(); trigger(); db.exec("BEGIN IMMEDIATE");
    try {
      equal(await save(filled()), undefined); equal(all().length, 1); equal(all(other), []);
      assert.throws(() => db.exec("COMMIT"), /FOREIGN KEY/); units++;
      equal(db.inTransaction, true); db.exec("ROLLBACK"); equal(all(), []);
      equal(db.query("SELECT * FROM enrollment_create_guard").all(), []);
    } finally {
      if (db.inTransaction) db.exec("ROLLBACK");
      db.exec("DROP TRIGGER temp.enrollment_create_commit"); other.close();
    }
    await save(minimal());
  });
  await test("finalizer error after actual autocommit rejects but write is durable", async () => {
    const error = new Error("synthetic post-autocommit finalizer"), seam = facade({ finalize: error });
    await rejects(() => save(filled(), seam.connection), error);
    equal(seam.counts(), [1, 1, 1]); equal(db.inTransaction, false); equal(row()!.primary_url, raw);
    const reopened = open();
    try { equal(all(reopened), all()); } finally { reopened.close(); }
  });
  await test("cached caller cursor preserved TEMP shadow cannot redirect MAIN INSERT", async () => {
    const cursor = db.query("SELECT state FROM main.enrollment_attempts WHERE id=?");
    equal(cursor.get(id), null);
    db.exec("CREATE TEMP TABLE enrollment_attempts(marker TEXT)");
    try {
      await save(minimal()); equal(cursor.get(id), { state: "committed" });
      equal(db.query("SELECT * FROM temp.enrollment_attempts").all(), []);
      db.exec("DELETE FROM main.enrollment_attempts");
      await save({ ...minimal(), state: "failed" }); equal(cursor.get(id), { state: "failed" });
    } finally { cursor.finalize(); db.exec("DROP TABLE temp.enrollment_attempts"); }
  });
  await test("BUSY atomically rejects no partial row then releases and recovers", async () => {
    const other = open(), seam = facade(); other.exec("BEGIN IMMEDIATE");
    try {
      await unchanged(filled(), /locked|busy/i, seam.connection);
      equal(seam.counts(), [1, 1, 1]); equal(all(other), []);
    } finally { other.exec("ROLLBACK"); other.close(); }
    equal(await save(filled()), undefined); equal(all().length, 1);
    const reopened = open();
    try { equal(all(reopened), all()); } finally { reopened.close(); }
  });
  await test("final complete native oracle row all caller fields retained", async () => {
    equal(await save(filled()), undefined);
    equal(all().length, 1);
  });
  console.log(`enrollment create complete: ${units} assertion units; ${scenarios} scenarios (not additive)`);
} finally { db.close(); }
