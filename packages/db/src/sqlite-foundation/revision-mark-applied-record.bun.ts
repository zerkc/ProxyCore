import assert from "node:assert/strict";
import { Database, type SQLQueryBindings } from "bun:sqlite";
import { createInterface } from "node:readline";
import { openSQLite } from "./open.js";
import { encodeTimestamp, decodeTimestamp } from "./timestamp.js";
import { getSQLiteRevisionRecord } from "./revision-record.js";
import { markAppliedSQLiteRevisionRecord } from "./revision-mark-applied-record.js";
import type { RevisionRecord, RevisionStore } from "../ports.js";

type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2) ? true : false : false;
type Expect<T extends true> = T;
type CanonicalParameters = Expect<Identical<Parameters<typeof markAppliedSQLiteRevisionRecord>,
  [db: Database, ...args: Parameters<RevisionStore["markApplied"]>]>>;
type CanonicalReturn = Expect<Identical<ReturnType<typeof markAppliedSQLiteRevisionRecord>, Promise<RevisionRecord>>>;

const [mode, path, revision] = process.argv.slice(2);
const db = openSQLite(path);
const id = (n: number) => `aaaaaaaa-aaaa-aaaa-aaaa-${String(n).padStart(12, "0")}`;
const stamp = (seconds: bigint, nanos = 0) => encodeTimestamp({ kind: "finite", seconds, nanos })!;
const applied = new Date(-1);
const optional = ["actorUserId", "sourcePrimaryId", "sourceNodeId", "sourceRevisionId",
  "snapshotContentHash", "snapshotVersion", "replicationVersion", "leadershipGeneration"] as const;
const keys = ["id", "revisionNumber", "checksum", "snapshot", "source", "createdAt", "appliedAt", ...optional];
function revisions() { return db.query("SELECT * FROM main.config_revisions ORDER BY id").all(); }
function settings() { return db.query("SELECT * FROM main.installation_settings ORDER BY id").all(); }
function bytes() { return [revisions(), settings()]; }
function row() { return db.query<Record<string, unknown>, [string]>("SELECT * FROM main.config_revisions WHERE id=?").get(revision)!; }
function setting() { return db.query<Record<string, unknown>, []>("SELECT * FROM main.installation_settings WHERE id='default'").get()!; }
function update(column: string, value: SQLQueryBindings) {
  db.query(`UPDATE main.config_revisions SET ${column}=? WHERE id=?`).run(value, revision);
}
let cases = 0;
async function check(name: string, body: () => void | Promise<void>) {
  await body(); cases++; console.log(`PASS ${name}`);
}
const original = row(), originalSettings = settings() as Record<string, unknown>[];
function restore() {
  db.exec("PRAGMA ignore_check_constraints=OFF");
  db.transaction(() => {
    for (const [key, value] of Object.entries(original)) if (key !== "id") update(key, value as SQLQueryBindings);
    db.exec("DELETE FROM main.installation_settings");
    for (const saved of originalSettings) {
      const columns = Object.keys(saved);
      db.query(`INSERT INTO main.installation_settings (${columns.join(",")}) VALUES (${columns.map(() => "?").join(",")})`)
        .run(...Object.values(saved) as SQLQueryBindings[]);
    }
  }).immediate();
}
async function test(name: string, body: () => void | Promise<void>) {
  await check(name, async () => {
    try { await body(); assert.equal(db.inTransaction, false); }
    finally { if (db.inTransaction) db.exec("ROLLBACK"); restore(); }
    assert.deepEqual(row(), original); assert.deepEqual(settings(), originalSettings);
  });
}
async function mark(connection = db, date: Date | undefined = applied, key = revision) {
  const promise = markAppliedSQLiteRevisionRecord(connection, key, date);
  assert(promise instanceof Promise);
  const record = await promise;
  assert.equal(db.inTransaction, false);
  assert.deepEqual(Object.keys(record).sort(), [...keys].sort());
  return record;
}
async function rejected(connection = db, date: Date | undefined = applied, key = revision, pattern?: RegExp) {
  const before = bytes();
  if (pattern) await assert.rejects(mark(connection, date, key), pattern);
  else await assert.rejects(mark(connection, date, key));
  assert.deepEqual(bytes(), before); assert.equal(db.inTransaction, false);
}
function expected(snapshot: unknown = JSON.parse(original.snapshot as string)): RevisionRecord {
  return { id: revision, revisionNumber: 1, checksum: original.checksum as string,
    snapshot: snapshot as RevisionRecord["snapshot"], source: "ordinary", createdAt: new Date(123000),
    appliedAt: new Date(-1), actorUserId: undefined, sourcePrimaryId: undefined, sourceNodeId: undefined,
    sourceRevisionId: undefined, snapshotContentHash: undefined, snapshotVersion: undefined,
    replicationVersion: undefined, leadershipGeneration: undefined };
}
// Local facade wraps real SQLite operations, never global methods or caller cache.
// Finalizer faults are observed AFTER native finalize: unable to force SQLite's
// own native finalizer to fail reproducibly; that narrower limitation is explicit.
function facade(target: string, stage: "prepare" | "step" | "finalize" | "row" = "row",
  alter: (row: Record<string, unknown>) => void = () => {}, error = new Error(stage)) {
  const events: string[] = [], statements: ReturnType<Database["prepare"]>[] = [];
  let selected = 0;
  const connection = {
    get inTransaction() { return db.inTransaction; },
    transaction: db.transaction.bind(db),
    prepare(sql: string) {
      events.push(sql); assert(sql.includes("main."));
      const matched = sql.startsWith(target);
      if (matched) selected++;
      if (matched && stage === "prepare") throw error;
      const statement = db.prepare<Record<string, unknown>, SQLQueryBindings[]>(sql);
      statements.push(statement);
      return {
        all(...bindings: SQLQueryBindings[]) {
          if (matched && stage === "step") throw error;
          const result = statement.all(...bindings);
          if (matched && stage === "row") for (const value of result) alter(value);
          return result;
        },
        finalize() {
          statement.finalize(); events.push("FINALIZED");
          if (matched && stage === "finalize") throw error;
        },
      };
    },
  } as unknown as Database;
  return { connection, events, statements, selected: () => selected, error };
}

if (mode === "child") {
  db.exec("PRAGMA busy_timeout=150");
  const reader = createInterface({ input: process.stdin });
  console.log("READY");
  for await (const command of reader) {
    if (command === "exit") break;
    if (command === "hold") { db.exec("BEGIN IMMEDIATE"); console.log("HELD"); }
    else if (command === "release") { db.exec("ROLLBACK"); console.log("RELEASED"); }
    else if (command.startsWith("mark ")) {
      try {
        const value = await markAppliedSQLiteRevisionRecord(db, revision, new Date(Number(command.slice(5))));
        console.log(`APPLIED ${value.appliedAt!.getTime()}`);
      } catch (error) {
        if (/locked|busy/i.test(String(error))) console.log("BUSY");
        else throw error;
      }
    } else throw new Error(`Unknown command ${command}`);
  }
  reader.close();
  if (db.inTransaction) db.exec("ROLLBACK");
} else if (mode === "unit") {
  await test("canonical fifteen fields, own nullable undefined, bytes and real reader comparison", async () => {
    const before = row(), result = await mark();
    assert.deepEqual(result, expected()); assert.deepEqual(getSQLiteRevisionRecord(db, revision), expected());
    assert.deepEqual(row(), { ...before, applied_at: stamp(-1n, 999000000) });
    assert.equal(setting().updated_at, row().applied_at); assert.equal(setting().current_applied_revision_id, revision);
    for (const key of optional) { assert(Object.hasOwn(result, key)); assert.equal(result[key], undefined); }
  });
  await test("missing settings: defaults, nullable pointers, independent two initialization clocks", async () => {
    db.exec(`DELETE FROM main.installation_settings;
      CREATE TABLE main.mark_initial(created TEXT,updated TEXT);
      CREATE TRIGGER main.mark_initial AFTER INSERT ON installation_settings BEGIN
        INSERT INTO mark_initial VALUES(NEW.created_at,NEW.updated_at); END`);
    const lower = Date.now();
    try {
      await mark(); const upper = Date.now(), stored = setting();
      const initial = db.query<{ created: string; updated: string }, []>("SELECT * FROM main.mark_initial").get()!;
      assert.equal(initial.created, initial.updated); assert.equal(stored.created_at, initial.created);
      const clock = decodeTimestamp(initial.created); assert.equal(clock.kind, "finite");
      if (clock.kind !== "finite") assert.fail("finite writer clock");
      const milliseconds = Number(clock.seconds * 1000n) + clock.nanos / 1000000;
      assert(milliseconds >= lower && milliseconds <= upper);
      assert.notEqual(stored.created_at, stored.updated_at);
      assert.equal(stored.updated_at, stamp(-1n, 999000000));
      assert.equal(stored.retention_max_age_days, 7n); assert.equal(stored.retention_max_size_mb, 50n);
      assert.equal(stored.forwarding_rules, "[]"); assert.equal(stored.default_resolver_pool, null);
      for (const key of ["current_desired_revision_id", "ingress_ipv4", "ingress_ipv6", "enrollment_hostnames"]) assert.equal(stored[key], null);
      assert.equal(stored.current_applied_revision_id, revision);
    } finally { db.exec("DROP TRIGGER main.mark_initial; DROP TABLE main.mark_initial"); }
  });
  await test("existing settings: desired pointer, JSON/retention/native bytes preserved", async () => {
    db.query(`UPDATE main.installation_settings SET current_desired_revision_id=?,forwarding_rules=?,
      default_resolver_pool=?,enrollment_hostnames=?,retention_max_age_days=-7,retention_max_size_mb=99,
      ingress_ipv4='not parsed',ingress_ipv6='also untouched',created_at='2' WHERE id='default'`)
      .run(id(90), ' [0,false,"",null] ', ' {"opaque":true} ', ' ["雪"] ');
    const before = setting(); await mark();
    assert.deepEqual(setting(), { ...before, updated_at: stamp(-1n, 999000000), current_applied_revision_id: revision });
  });
  for (const source of ["ordinary", "import", "sync"] as const) {
    await test(`source ${source}`, async () => { update("source", source); assert.equal((await mark()).source, source); });
  }
  await test("all metadata, independent expected record and unchecked int64 rounding", async () => {
    for (const column of ["source_primary_id", "source_node_id", "source_revision_id"]) update(column, id(8));
    update("actor_user_id", null); update("source", "sync"); update("snapshot_content_hash", " 雪🦊\n ");
    update("snapshot_version", -2147483648n); update("replication_version", 2147483647n);
    update("leadership_generation", 9007199254740993n);
    const result = await mark();
    assert.deepEqual(result, { ...expected(), source: "sync", sourcePrimaryId: id(8), sourceNodeId: id(8),
      sourceRevisionId: id(8), snapshotContentHash: " 雪🦊\n ", snapshotVersion: -2147483648,
      replicationVersion: 2147483647, leadershipGeneration: 9007199254740992 });
    assert.equal(row().leadership_generation, 9007199254740993n);
  });
  await test("actual FK-valid actor attribution preserved in all fifteen fields", async () => {
    db.query(`INSERT INTO main.users(id,username,password_hash,role,created_at,updated_at)
      VALUES(?,'mark-applied-fixture','opaque','owner','0','2')`).run(id(9));
    try {
      update("actor_user_id", id(9)); const before = row(), result = await mark();
      assert.deepEqual(result, { ...expected(), actorUserId: id(9) });
      assert.deepEqual(getSQLiteRevisionRecord(db, revision), { ...expected(), actorUserId: id(9) });
      assert.deepEqual(row(), { ...before, applied_at: stamp(-1n, 999000000) });
    } finally { update("actor_user_id", null); db.query("DELETE FROM main.users WHERE id=?").run(id(9)); }
  });
  for (const [raw, value] of [["null", null], ["false", false], ["0", 0], ['""', ""], ["[]", []],
    [' {"items":[null,0,false,"",{"snow":"雪"}]} ', { items: [null, 0, false, "", { snow: "雪" }] }],
    ["922337203685477580812345", 9.223372036854776e23], ["1e400", Infinity]] as [string, unknown][]) {
    await test(`generic JSON ${raw}`, async () => {
      update("snapshot", raw); const before = row();
      assert.deepEqual((await mark()).snapshot, value); assert.equal(row().snapshot, raw);
      assert.deepEqual(row(), { ...before, applied_at: stamp(-1n, 999000000) });
    });
  }
  for (const raw of [null, "", " untouched "]) {
    await test(`optional text ${raw}`, async () => { update("snapshot_content_hash", raw); assert.equal((await mark()).snapshotContentHash, raw ?? undefined); });
  }
  for (const column of ["revision_number", "snapshot_version", "replication_version", "leadership_generation"]) {
    const key = { revision_number: "revisionNumber", snapshot_version: "snapshotVersion", replication_version: "replicationVersion", leadership_generation: "leadershipGeneration" }[column]! as keyof RevisionRecord;
    const values = column === "leadership_generation" ? [-9223372036854775808n, 0n, 9223372036854775807n, -9007199254740993n]
      : [-2147483648n, 0n, 2147483647n];
    for (const raw of values) {
      await test(`integer ${column} ${raw}`, async () => { update(column, raw); assert.equal((await mark())[key], Number(raw)); assert.equal(row()[column], raw); });
    }
  }
  const clocks: [string, number][] = [[stamp(0n), 0], [stamp(0n, 999999), 0], [stamp(0n, 1000000), 1],
    [stamp(-1n, 999999999), -1], [stamp(-1n, 998999999), -2], [stamp(-1n, 1), -1000],
    [stamp(8640000000000n), 8640000000000000], [stamp(8640000000000n, 999999), 8640000000000000],
    [stamp(8640000000000n, 1000000), NaN], [stamp(-8640000000000n), -8640000000000000],
    [stamp(-8640000000001n, 999999999), NaN], [stamp(9223372036854775807n), NaN],
    [stamp(-9223372036854775808n), NaN], ["0", NaN], ["2", NaN]];
  for (const [raw, value] of clocks) {
    await test(`stored GENERAL createdAt ${raw}`, async () => {
      update("created_at", raw); assert.equal((await mark()).createdAt.getTime(), value); assert.equal(row().created_at, raw);
    });
    await test(`returned GENERAL appliedAt ${raw}`, async () => {
      const f = facade("UPDATE main.config_revisions", "row", (returned) => { returned.applied_at = raw; });
      const result = await mark(f.connection); assert.equal(result.appliedAt!.getTime(), value);
      // The facade changes only returned bytes, not native storage.
      assert.equal(row().applied_at, stamp(-1n, 999000000));
    });
  }
  for (const millis of [-8640000000000000, -1001, -1, 0, 1, 1999, 8640000000000000]) {
    await test(`explicit Date input ${millis}`, async () => {
      const result = await mark(db, new Date(millis)); assert.equal(result.appliedAt!.getTime(), millis);
      assert.equal(setting().updated_at, row().applied_at);
    });
  }
  for (const variant of ["omitted", "own undefined"]) {
    await test(`default Date ${variant} captured once, private clock honest bounds`, async () => {
      const before = Date.now();
      const value = variant === "omitted" ? await markAppliedSQLiteRevisionRecord(db, revision)
        : await markAppliedSQLiteRevisionRecord(db, revision, { appliedAt: undefined }.appliedAt);
      assert(value.appliedAt!.getTime() >= before && value.appliedAt!.getTime() <= Date.now());
      assert.equal(row().applied_at, setting().updated_at);
    });
  }
  for (const value of [new Date(NaN), null, "2020-01-01", 0, {}, true]) {
    await test(`invalid/unsupported input ${String(value)} before SQL`, async () => {
      const f = facade("UPDATE main.config_revisions");
      await rejected(f.connection, value as Date); assert.equal(f.events.length, 0);
    });
  }
  await test("missing literal ID exact error before settings creation", async () => {
    db.exec("DELETE FROM main.installation_settings");
    for (const key of ["missing", "", "' OR 1=1 --", revision.toUpperCase(), `${revision}\0`]) {
      const before = bytes();
      await assert.rejects(markAppliedSQLiteRevisionRecord(db, key, applied), (error: unknown) => error instanceof Error && error.message === `Revision not found: ${key}`);
      assert.deepEqual(bytes(), before);
    }
  });
  await test("caller BEGIN and savepoint reject, no pending state/cache/statement touched", async () => {
    const cached = db.query("SELECT * FROM main.config_revisions WHERE id=?");
    for (const boundary of ["BEGIN", "SAVEPOINT caller"]) {
      db.exec(boundary); update("checksum", "pending caller");
      const before = bytes(), f = facade("UPDATE main.config_revisions");
      await assert.rejects(markAppliedSQLiteRevisionRecord(f.connection, revision, applied), /Nested write transaction/);
      assert(db.inTransaction); assert.deepEqual(bytes(), before); assert.equal(f.events.length, 0);
      assert.equal((cached.get(revision) as Record<string, unknown>).checksum, "pending caller"); db.exec("ROLLBACK");
    }
  });
  await test("error ordering: invalid Date precedes caller-boundary policy", async () => {
    db.exec("BEGIN");
    try { await assert.rejects(markAppliedSQLiteRevisionRecord(db, revision, new Date(NaN)), RangeError); assert(db.inTransaction); }
    finally { db.exec("ROLLBACK"); }
  });
  for (const target of ["UPDATE main.config_revisions", "INSERT INTO main.installation_settings", "UPDATE main.installation_settings"]) {
    for (const stage of ["prepare", "step", "finalize"] as const) {
      await test(`owned statement error ${target} ${stage}`, async () => {
        if (target.startsWith("INSERT")) db.exec("DELETE FROM main.installation_settings");
        const before = bytes(), f = facade(target, stage), cached = db.query("SELECT * FROM main.config_revisions WHERE id=?");
        await assert.rejects(mark(f.connection), (error: unknown) => error === f.error);
        assert.equal(f.selected(), 1); assert.deepEqual(bytes(), before); assert.equal(db.inTransaction, false);
        for (const statement of f.statements) assert.throws(() => statement.all());
        assert.deepEqual(cached.get(revision), row()); await mark();
      });
    }
  }
  await test("drain/finalize every owned stage, no caller cache or revision reread", async () => {
    db.exec("DELETE FROM main.installation_settings"); const f = facade("UPDATE main.config_revisions");
    const cached = db.query("SELECT * FROM main.config_revisions WHERE id=?"); await mark(f.connection);
    assert.equal(f.events.length, 8);
    for (let index = 1; index < f.events.length; index += 2) assert.equal(f.events[index], "FINALIZED");
    assert(f.events[4].includes("ON CONFLICT DO NOTHING RETURNING id"));
    assert(!f.events.some((sql) => sql.startsWith("SELECT") && sql.includes("config_revisions")));
    for (const statement of f.statements) assert.throws(() => statement.all());
    assert.deepEqual(cached.get(revision), row());
  });
  for (const create of [true, false]) {
    await test(`controlled local conflict/ignored insert reread ${create}`, async () => {
      db.exec(`DELETE FROM main.installation_settings;
        CREATE TRIGGER main.mark_conflict BEFORE INSERT ON installation_settings WHEN NEW.id='default' BEGIN
        ${create ? "INSERT INTO installation_settings(id,forwarding_rules,created_at,updated_at) VALUES('default','[]',NEW.created_at,NEW.updated_at);" : ""}
        SELECT RAISE(IGNORE); END`);
      try {
        const f = facade("SELECT id FROM main.installation_settings");
        if (create) { await mark(f.connection); assert.equal(setting().current_applied_revision_id, revision); }
        else await rejected(f.connection, applied, revision, /Installation settings could not be initialized/);
        assert.equal(f.selected(), 2);
      } finally { db.exec("DROP TRIGGER main.mark_conflict"); }
    });
  }
  await test("first RETURNING row survives AFTER revision and settings triggers", async () => {
    db.exec(`CREATE TRIGGER main.mark_after AFTER UPDATE OF applied_at ON config_revisions BEGIN
        UPDATE config_revisions SET snapshot='false',checksum='after update' WHERE id=NEW.id; END;
      CREATE TRIGGER main.mark_pointer AFTER UPDATE OF current_applied_revision_id ON installation_settings BEGIN
        UPDATE config_revisions SET snapshot='null',applied_at=NULL WHERE id=NEW.current_applied_revision_id; END`);
    try {
      assert.deepEqual(await mark(), expected());
      // The pointer trigger's applied_at UPDATE fires mark_after again. The
      // stored snapshot is false, while the first RETURNING snapshot is original.
      assert.equal(row().snapshot, "false"); assert.equal(row().applied_at, null);
      assert.equal(getSQLiteRevisionRecord(db, revision)!.snapshot, false);
    } finally { db.exec("DROP TRIGGER main.mark_after; DROP TRIGGER main.mark_pointer"); }
  });
  const invalid: [string, (returned: Record<string, unknown>) => void][] = [];
  for (const column of Object.keys(original)) {
    invalid.push([`missing ${column}`, (returned) => { delete returned[column]; }]);
    invalid.push([`undefined ${column}`, (returned) => { returned[column] = undefined; }]);
  }
  invalid.push(["extra", (returned) => { returned.extra = 1; }]);
  for (const column of ["id", "checksum", "snapshot", "actor_user_id", "created_at", "applied_at", "source",
    "source_primary_id", "source_node_id", "source_revision_id", "snapshot_content_hash"]) {
    for (const value of [0, 1n, false, {}, "bad\0text"]) invalid.push([`text ${column} ${String(value)}`, (returned) => { returned[column] = value; }]);
  }
  for (const column of ["id", "revision_number", "checksum", "snapshot", "created_at", "source"]) invalid.push([`null ${column}`, (returned) => { returned[column] = null; }]);
  for (const column of ["id", "actor_user_id", "source_primary_id", "source_node_id", "source_revision_id"]) {
    for (const value of ["", "bad", id(1).toUpperCase(), `${id(1)}\n`]) invalid.push([`UUID ${column} ${value}`, (returned) => { returned[column] = value; }]);
  }
  for (const column of ["revision_number", "snapshot_version", "replication_version", "leadership_generation"]) {
    for (const value of [0, 1.5, NaN, "1", true, 9223372036854775808n, -9223372036854775809n]) invalid.push([`integer ${column} ${value}`, (returned) => { returned[column] = value; }]);
    if (column !== "leadership_generation") for (const value of [-2147483649n, 2147483648n]) invalid.push([`int32 ${column} ${value}`, (returned) => { returned[column] = value; }]);
  }
  for (const [column, values] of [["snapshot", ["{", "undefined", "", "[1,]"]], ["source", ["", "ordinary ", "invalid"]],
    ["created_at", ["3", "1", "100000000000000000000.1000000000", "118446744073709551616.000000000"]],
    ["applied_at", ["3", "1", "100000000000000000000.1000000000", "118446744073709551616.000000000"]]] as [string, string[]][]) {
    for (const value of values) invalid.push([`${column} ${value}`, (returned) => { returned[column] = value; }]);
  }
  for (const [name, alter] of invalid) {
    await test(`returned validation/projection rollback ${name}`, async () => {
      db.exec("DELETE FROM main.installation_settings"); const f = facade("UPDATE main.config_revisions", "row", alter);
      await rejected(f.connection); assert.equal(f.selected(), 1);
    });
  }
  await test("nullable returned appliedAt stays own undefined", async () => {
    const f = facade("UPDATE main.config_revisions", "row", (returned) => { returned.applied_at = null; });
    const result = await mark(f.connection); assert(Object.hasOwn(result, "appliedAt")); assert.equal(result.appliedAt, undefined);
  });
  await test("real invalid stored JSON causes complete rollback after settings work", async () => {
    db.exec("PRAGMA ignore_check_constraints=ON"); update("snapshot", "{");
    await rejected(); db.exec("PRAGMA ignore_check_constraints=OFF");
  });
  for (const missingSettings of [false, true]) await test(`actual deferred FK COMMIT failure, missing settings ${missingSettings}: both writes executed, no record, full rollback and recovery`, async () => {
    if (missingSettings) db.exec("DELETE FROM main.installation_settings");
    db.exec(`CREATE TABLE main.mark_guard(id TEXT REFERENCES config_revisions(id) DEFERRABLE INITIALLY DEFERRED);
      CREATE TRIGGER main.mark_commit AFTER UPDATE OF current_applied_revision_id ON installation_settings BEGIN
        INSERT INTO mark_guard VALUES('${id(99)}'); END`);
    const before = bytes(); let writesObserved = false, published: RevisionRecord | undefined;
    const f = facade("UPDATE main.installation_settings", "row");
    // Verify pending physical writes just before finalization of the last stage.
    const observed = { ...f.connection, prepare(sql: string) {
      const statement = f.connection.prepare(sql);
      return { all(...bindings: SQLQueryBindings[]) {
        const result = statement.all(...bindings);
        if (sql.startsWith("UPDATE main.installation_settings")) {
          assert(db.inTransaction); assert.equal(row().applied_at, stamp(-1n, 999000000));
          assert.equal(setting().current_applied_revision_id, revision); writesObserved = true;
        }
        return result;
      }, finalize: statement.finalize.bind(statement) };
    } } as unknown as Database;
    try {
      await assert.rejects(async () => { published = await markAppliedSQLiteRevisionRecord(observed, revision, applied); }, /FOREIGN KEY/i);
      assert(writesObserved); assert.equal(published, undefined); assert.equal(db.inTransaction, false);
      assert.deepEqual(bytes(), before); assert.deepEqual(db.query("SELECT * FROM main.mark_guard").all(), []);
      const reopened = openSQLite(path);
      try {
        assert.deepEqual(reopened.query("SELECT * FROM main.config_revisions WHERE id=?").get(revision), original);
        assert.deepEqual(reopened.query("SELECT * FROM main.installation_settings ORDER BY id").all(), before[1]);
      }
      finally { reopened.close(); }
    } finally { db.exec("DROP TRIGGER main.mark_commit; DROP TABLE main.mark_guard"); }
    await mark();
  });
  await test("TEMP shadows cannot redirect either table", async () => {
    db.exec("CREATE TEMP TABLE config_revisions(sentinel TEXT); CREATE TEMP TABLE installation_settings(sentinel TEXT)");
    try { await mark(); assert.deepEqual(db.query("SELECT * FROM temp.config_revisions").all(), []); assert.deepEqual(db.query("SELECT * FROM temp.installation_settings").all(), []); }
    finally { db.exec("DROP TABLE temp.config_revisions; DROP TABLE temp.installation_settings"); }
  });
  await test("fresh mutable public dates and JSON, independent subsequent records", async () => {
    update("snapshot", '{"items":[{"n":1}]}'); const first = await mark(), before = bytes();
    first.createdAt.setTime(99); first.appliedAt!.setTime(99);
    (first.snapshot as unknown as { items: { n: number }[] }).items[0].n = 99;
    const second = await mark(); assert.notStrictEqual(first, second);
    assert.equal(second.createdAt.getTime(), 123000); assert.equal(second.appliedAt!.getTime(), -1);
    assert.deepEqual(second.snapshot, { items: [{ n: 1 }] }); assert.deepEqual(bytes(), before);
  });
  await test("number-mode rejects numeric RETURNING and rolls back", async () => {
    const connection = new Database(path, { strict: true, safeIntegers: false });
    connection.exec("PRAGMA foreign_keys=ON");
    try { await rejected(connection, applied, revision, /INTEGER/); } finally { connection.close(); }
  });
  await test("closed DB errors, next valid connection recovers", async () => {
    const closed = openSQLite(path); closed.close(); await rejected(closed); await mark();
  });
  await test("non-string ID rejected without SQL", async () => {
    const f = facade("UPDATE main.config_revisions");
    for (const value of [null, undefined, 1, {}, 1n]) await assert.rejects(markAppliedSQLiteRevisionRecord(f.connection, value as string, applied), TypeError);
    assert.equal(f.events.length, 0);
  });
  const durable = await mark(); const reopened = openSQLite(path);
  try { assert.deepEqual(getSQLiteRevisionRecord(reopened, revision), durable); }
  finally { reopened.close(); }
  assert.equal(cases, 263, `complete fixture coverage ${cases}`);
  console.log(`COMPLETE markApplied cases=${cases}`);
} else throw new Error(`Unknown mode ${mode}`);
db.close();
