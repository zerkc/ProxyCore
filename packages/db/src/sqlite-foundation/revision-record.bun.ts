import assert from "node:assert/strict";
import { Database, type Statement } from "bun:sqlite";
import { openSQLite } from "./open.js";
import { encodeTimestamp } from "./timestamp.js";
import { getSQLiteRevisionRecord, type SQLiteRevisionRecord } from "./revision-record.js";
import type { RevisionRecord } from "../ports.js";

// Structural checks guard shape, not declaration origin; the alias links origin.
type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2) ? true : false
    : false;
type Expect<T extends true> = T;
type CanonicalRecord = Expect<Identical<SQLiteRevisionRecord, RevisionRecord>>;
type CanonicalReturn = Expect<Identical<ReturnType<typeof getSQLiteRevisionRecord>, RevisionRecord | undefined>>;

const [path, originalID] = process.argv.slice(2);
const db = openSQLite(path);
const id = (n: number) => `aaaaaaaa-aaaa-aaaa-aaaa-${String(n).padStart(12, "0")}`;
const stamp = (seconds: bigint, nanos = 0) => encodeTimestamp({ kind: "finite", seconds, nanos })!;
const optional = ["actorUserId", "sourcePrimaryId", "sourceNodeId", "sourceRevisionId",
  "snapshotContentHash", "snapshotVersion", "replicationVersion", "leadershipGeneration", "appliedAt"];
const keys = ["id", "revisionNumber", "checksum", "snapshot", "source", "createdAt", ...optional];
let cases = 0;
function rows() { return db.query("SELECT * FROM main.config_revisions ORDER BY id").all(); }
function check(name: string, body: () => void) { body(); cases++; console.log(`PASS ${name}`); }
function test(name: string, body: () => void) {
  check(name, () => {
    const before = rows(); db.exec("BEGIN");
    try { body(); assert.equal(db.inTransaction, true); }
    finally { if (db.inTransaction) db.exec("ROLLBACK"); }
    assert.deepEqual(rows(), before);
  });
}
function insert() {
  db.query(`INSERT INTO main.config_revisions(id,revision_number,checksum,snapshot,created_at)
    VALUES(?,2,'deliberately unrelated checksum','null',?)`).run(id(1), stamp(0n));
}
function update(column: string, value: string | bigint | null, key = id(1)) {
  // Fixture-controlled column names only, never public input.
  db.query(`UPDATE main.config_revisions SET ${column}=? WHERE id=?`).run(value, key);
}
function read(key = id(1)) {
  const before = rows(), changes = db.query("SELECT total_changes() AS n").get();
  const result = getSQLiteRevisionRecord(db, key);
  assert.deepEqual(rows(), before); assert.deepEqual(db.query("SELECT total_changes() AS n").get(), changes);
  if (result) assert.deepEqual(Object.keys(result).sort(), [...keys].sort());
  return result;
}
function driver(value: unknown, errors: { prepare?: Error; get?: Error; finalize?: Error } = {}) {
  let prepared = 0, fetched = 0, finalized = 0;
  const fake = {
    prepare(sql: string) {
      prepared++; assert.equal(sql, "SELECT * FROM main.config_revisions WHERE id = ?");
      if (errors.prepare) throw errors.prepare;
      return {
        get(key: string) { fetched++; assert.equal(key, "literal ' ? ID"); if (errors.get) throw errors.get; return value; },
        finalize() { finalized++; if (errors.finalize) throw errors.finalize; },
      };
    },
    query() { assert.fail("must own an uncached statement"); },
  } as unknown as Database;
  return { run: () => getSQLiteRevisionRecord(fake, "literal ' ? ID"),
    done: (counts = [1, 1, 1]) => assert.deepEqual([prepared, fetched, finalized], counts) };
}
try {
  const original = db.query<Record<string, unknown>, [string]>("SELECT * FROM main.config_revisions WHERE id=?").get(originalID)!;
  const originalBytes = rows();
  check("committed-Go-enqueue-reused-revision", () => {
    const record = read(originalID)!;
    assert.equal(record.id, originalID); assert.equal(record.revisionNumber, 1);
    assert.equal(record.source, "ordinary"); assert.equal(record.checksum, original.checksum);
    assert.deepEqual(record.snapshot, JSON.parse(original.snapshot as string));
    assert.equal(record.createdAt.toISOString(), "1970-01-01T00:02:03.000Z");
    for (const key of optional) { assert.ok(Object.hasOwn(record, key)); assert.equal(record[key as keyof RevisionRecord], undefined); }
    assert.equal(read("missing"), undefined); assert.equal(db.inTransaction, false);
  });
  for (const source of ["ordinary", "import", "sync"]) {
    test(`source-${source}`, () => { insert(); update("source", source); assert.equal(read()!.source, source); });
  }
  test("all-public-fields-attribution-and-metadata", () => {
    insert();
    db.query(`INSERT INTO users(id,username,password_hash,role,created_at,updated_at)
      VALUES(?,'revision-fixture','opaque','owner','0','2')`).run(id(9));
    for (const column of ["actor_user_id", "source_primary_id", "source_node_id", "source_revision_id"]) update(column, id(9));
    update("source", "sync"); update("snapshot_content_hash", " 雪🦊\n ");
    update("snapshot_version", -2147483648n); update("replication_version", 2147483647n);
    update("leadership_generation", 9007199254740993n); update("applied_at", stamp(-1n, 999999999));
    update("snapshot", ' {"references":{"id":"not-a-UUID","revision":0},"items":[false,"",null]} ');
    assert.deepEqual(read(), {
      id: id(1), revisionNumber: 2, checksum: "deliberately unrelated checksum",
      snapshot: { references: { id: "not-a-UUID", revision: 0 }, items: [false, "", null] },
      actorUserId: id(9), source: "sync", sourcePrimaryId: id(9), sourceNodeId: id(9), sourceRevisionId: id(9),
      snapshotContentHash: " 雪🦊\n ", snapshotVersion: -2147483648, replicationVersion: 2147483647,
      leadershipGeneration: 9007199254740992, createdAt: new Date(0), appliedAt: new Date(-1),
    });
  });
  for (const [raw, expected] of [
    ["null", null], ["0", 0], ["false", false], ['""', ""], ["42", 42], ["true", true],
    ['"scalar"', "scalar"], ["[]", []], ["{}", {}], [' [null,0,false,"",{"snow":"雪🦊"}] ', [null, 0, false, "", { snow: "雪🦊" }]],
    [' \n{"big":922337203685477580812345} \t', { big: 9.223372036854776e23 }], ["1e400", Infinity],
  ] as [string, unknown][]) {
    test(`generic-JSON-${raw}`, () => {
      insert(); update("snapshot", raw); assert.deepEqual(read()!.snapshot, expected);
      const stored = db.query<{ snapshot: string; checksum: string }, [string]>("SELECT snapshot,checksum FROM config_revisions WHERE id=?").get(id(1))!;
      assert.equal(stored.snapshot, raw); assert.equal(stored.checksum, "deliberately unrelated checksum");
    });
  }
  test("nullable-TEXT-not-truthiness", () => {
    insert();
    for (const raw of [null, "", "  untouched 雪🦊\n "]) {
      update("snapshot_content_hash", raw); assert.equal(read()!.snapshotContentHash, raw ?? undefined);
    }
    update("checksum", ""); assert.equal(read()!.checksum, "");
  });
  for (const [column, key] of [["revision_number", "revisionNumber"], ["snapshot_version", "snapshotVersion"], ["replication_version", "replicationVersion"]] as const) {
    for (const raw of [-2147483648n, 0n, 2147483647n, ...(column === "revision_number" ? [] : [null])]) {
      test(`int32-${column}-${raw}`, () => { insert(); update(column, raw); assert.equal(read()![key], raw === null ? undefined : Number(raw)); });
    }
  }
  for (const [raw, expected] of [[null, undefined], [0n, 0], [-9223372036854775808n, -9223372036854775808],
    [9223372036854775807n, 9223372036854775808], [9007199254740993n, 9007199254740992], [-9007199254740993n, -9007199254740992]] as [bigint | null, number | undefined][]) {
    test(`int64-${raw}`, () => { insert(); update("leadership_generation", raw); assert.equal(read()!.leadershipGeneration, expected); });
  }
  // Independently known absolute millisecond goldens, including normalized negative fractions.
  const clocks: [string, number, string | undefined][] = [
    [stamp(0n), 0, "1970-01-01T00:00:00.000Z"],
    [stamp(0n, 1), 0, "1970-01-01T00:00:00.000Z"],
    [stamp(0n, 999999), 0, "1970-01-01T00:00:00.000Z"],
    [stamp(0n, 1000000), 1, "1970-01-01T00:00:00.001Z"],
    [stamp(-1n, 999999999), -1, "1969-12-31T23:59:59.999Z"],
    [stamp(-1n, 998999999), -2, "1969-12-31T23:59:59.998Z"],
    [stamp(-1n, 1), -1000, "1969-12-31T23:59:59.000Z"],
    [stamp(1n, 999999999), 1999, "1970-01-01T00:00:01.999Z"],
    [stamp(8640000000000n), 8640000000000000, "+275760-09-13T00:00:00.000Z"],
    [stamp(8640000000000n, 999999), 8640000000000000, "+275760-09-13T00:00:00.000Z"],
    [stamp(8639999999999n, 999999999), 8639999999999999, "+275760-09-12T23:59:59.999Z"],
    [stamp(8640000000000n, 1000000), NaN, undefined],
    [stamp(-8640000000000n), -8640000000000000, "-271821-04-20T00:00:00.000Z"],
    [stamp(-8640000000000n, 1000000), -8639999999999999, "-271821-04-20T00:00:00.001Z"],
    [stamp(-8640000000001n, 999999999), NaN, undefined],
    [stamp(-9223372036854775808n, 999999999), NaN, undefined],
    [stamp(9223372036854775807n), NaN, undefined], ["0", NaN, undefined], ["2", NaN, undefined],
  ];
  for (const [column, key] of [["created_at", "createdAt"], ["applied_at", "appliedAt"]] as const) {
    for (const [raw, expected, iso] of clocks) {
      test(`clock-${column}-${raw}`, () => {
        insert(); update(column, raw); const date = read()![key]!;
        assert.ok(date instanceof Date); assert.equal(date.getTime(), expected);
        if (iso) assert.equal(date.toISOString(), iso); else assert.throws(() => date.toISOString(), RangeError);
        assert.equal(db.query<Record<string, unknown>, [string]>("SELECT * FROM config_revisions WHERE id=?").get(id(1))![column], raw);
      });
    }
  }
  test("literal-ID-not-PG-UUID-coercion", () => {
    insert(); assert.equal(read()!.id, id(1));
    for (const key of ["", "missing", id(1).toUpperCase(), `{${id(1)}}`, id(1).replaceAll("-", ""),
      ` ${id(1)}`, `${id(1)} `, "' OR 1=1 --", `${id(1)}\0`, "?", "雪"]) assert.equal(read(key), undefined);
  });
  test("main-qualified-TEMP-shadow", () => {
    db.exec("CREATE TEMP TABLE config_revisions(id TEXT)");
    try { assert.equal(read(originalID)!.id, originalID); } finally { db.exec("DROP TABLE temp.config_revisions"); }
  });
  test("fresh-mutable-Date-JSON-and-record-isolation", () => {
    insert(); update("snapshot", '{"list":[{"n":1}]}'); update("applied_at", stamp(1n));
    const bytes = rows(), first = read()!;
    first.createdAt.setTime(123); first.appliedAt!.setTime(456); first.source = "import";
    (first.snapshot as unknown as { list: { n: number }[] }).list[0].n = 99;
    const next = read()!;
    assert.equal(next.createdAt.getTime(), 0); assert.equal(next.appliedAt!.getTime(), 1000);
    assert.equal(next.source, "ordinary"); assert.deepEqual(next.snapshot, { list: [{ n: 1 }] });
    assert.notStrictEqual(first, next); assert.notStrictEqual(first.snapshot, next.snapshot);
    assert.notStrictEqual(first.createdAt, next.createdAt); assert.notStrictEqual(first.appliedAt, next.appliedAt);
    assert.deepEqual(rows(), bytes);
  });
  check("pending-visibility-and-rollback", () => {
    const other = openSQLite(path), before = rows(); db.exec("BEGIN IMMEDIATE");
    try { insert(); assert.equal(read()!.id, id(1)); assert.equal(getSQLiteRevisionRecord(other, id(1)), undefined); assert.equal(db.inTransaction, true); }
    finally { if (db.inTransaction) db.exec("ROLLBACK"); other.close(); }
    assert.deepEqual(rows(), before); assert.equal(read(), undefined);
  });
  check("private-finalization-preserves-caller-cached-cursor", () => {
    // Bun 1.3.14 stops returning identical db.query() objects once a connection
    // has cached about 20 statements (1.4.2 keeps them), so this identity probe
    // uses a connection that holds only the statement under test.
    const cacheDb = openSQLite(path);
    const sql = "SELECT * FROM main.config_revisions WHERE id = ?", cached = cacheDb.query(sql), owned: Statement[] = [];
    const observed = new Proxy(cacheDb, { get(target, property) {
      if (property === "prepare") return (sql: string) => { const s = target.prepare(sql); owned.push(s); return s; };
      const value = Reflect.get(target, property); return typeof value === "function" ? value.bind(target) : value;
    } });
    assert.equal(getSQLiteRevisionRecord(observed, originalID)!.id, originalID);
    assert.equal(getSQLiteRevisionRecord(observed, "missing"), undefined);
    assert.equal(owned.length, 2); for (const s of owned) assert.throws(() => s.get(originalID));
    assert.deepEqual(cached.get(originalID), original); assert.strictEqual(cacheDb.query(sql), cached);
    cacheDb.close();
  });
  const invalid: [string, unknown][] = [["undefined-row", undefined], ["array-row", []], ["extra-column", { ...original, extra: 1 }]];
  for (const column of Object.keys(original)) {
    const missing = { ...original }; delete missing[column]; invalid.push([`missing-${column}`, missing]);
    invalid.push([`undefined-${column}`, { ...original, [column]: undefined }]);
  }
  for (const column of ["id", "checksum", "snapshot", "actor_user_id", "created_at", "applied_at", "source",
    "source_primary_id", "source_node_id", "source_revision_id", "snapshot_content_hash"]) {
    for (const value of [0, 1n, false, {}, "bad\0text"]) invalid.push([`TEXT-${column}-${String(value)}`, { ...original, [column]: value }]);
  }
  for (const column of ["id", "checksum", "snapshot", "created_at", "source", "revision_number"]) invalid.push([`required-null-${column}`, { ...original, [column]: null }]);
  for (const column of ["id", "actor_user_id", "source_primary_id", "source_node_id", "source_revision_id"]) {
    for (const value of ["", "not UUID", id(1).toUpperCase(), `{${id(1)}}`, id(1).replaceAll("-", ""), `${id(1)}\n`]) invalid.push([`UUID-${column}-${value}`, { ...original, [column]: value }]);
  }
  for (const column of ["revision_number", "snapshot_version", "replication_version", "leadership_generation"]) {
    for (const value of [0, 1.5, NaN, "1", true, 9223372036854775808n, -9223372036854775809n]) invalid.push([`INTEGER-${column}-${String(value)}`, { ...original, [column]: value }]);
    if (column !== "leadership_generation") for (const value of [2147483648n, -2147483649n]) invalid.push([`int32-${column}-${value}`, { ...original, [column]: value }]);
  }
  for (const value of ["", "ordinary ", "invalid"]) invalid.push([`source-${value}`, { ...original, source: value }]);
  for (const value of ["{", "undefined", "", "[1,]"]) invalid.push([`JSON-${value}`, { ...original, snapshot: value }]);
  for (const column of ["created_at", "applied_at"]) {
    for (const value of ["3", "1", "100000000000000000000.1000000000", "118446744073709551616.000000000", "100000000000000000000.00000000x"]) invalid.push([`tag-${column}-${value}`, { ...original, [column]: value }]);
  }
  for (const [name, value] of invalid) check(`raw-invalid-${name}`, () => { const probe = driver(value); assert.throws(probe.run); probe.done(); });
  check("missing-only-native-NULL", () => { const probe = driver(null); assert.equal(probe.run(), undefined); probe.done(); });
  for (const phase of ["prepare", "get", "finalize"] as const) {
    check(`error-identity-${phase}`, () => {
      const error = new Error(phase), probe = driver(original, { [phase]: error });
      assert.throws(probe.run, (caught) => caught === error); probe.done(phase === "prepare" ? [1, 0, 0] : [1, 1, 1]);
    });
  }
  check("finalizer-failure-on-missing-invalid-and-success", () => {
    for (const value of [null, { ...original, snapshot: "{" }, original]) {
      const error = new Error("finalizer"), probe = driver(value, { finalize: error });
      assert.throws(probe.run, (caught) => caught === error); probe.done();
    }
  });
  check("runtime-non-string-before-prepare", () => {
    const fake = { prepare() { assert.fail("must not prepare"); } } as unknown as Database;
    for (const value of [null, undefined, 1, {}, 1n]) assert.throws(() => getSQLiteRevisionRecord(fake, value as string), TypeError);
  });
  test("real-query-error-and-reuse", () => {
    db.exec("ALTER TABLE main.config_revisions RENAME TO revision_record_hidden");
    assert.throws(() => getSQLiteRevisionRecord(db, originalID));
    db.exec("ALTER TABLE main.revision_record_hidden RENAME TO config_revisions"); assert.equal(read(originalID)!.id, originalID);
  });
  test("real-corruption-rejected-without-writes", () => {
    insert(); db.exec("PRAGMA ignore_check_constraints = ON");
    try {
      for (const [column, value] of [["source", "invalid"], ["snapshot", "{"], ["created_at", "bad-tag"], ["snapshot_version", 2147483648n]] as [string, string | bigint][]) {
        const previous = db.query<Record<string, string | bigint | null>, [string]>("SELECT * FROM config_revisions WHERE id=?").get(id(1))![column];
        update(column, value); const before = rows(); assert.throws(() => getSQLiteRevisionRecord(db, id(1))); assert.deepEqual(rows(), before);
        update(column, previous); assert.equal(read()!.id, id(1));
      }
    } finally { db.exec("PRAGMA ignore_check_constraints = OFF"); }
  });
  check("closed-errors-not-missing", () => {
    const closed = openSQLite(path); closed.close();
    assert.throws(() => getSQLiteRevisionRecord(closed, originalID)); assert.throws(() => getSQLiteRevisionRecord(closed, "missing"));
  });
  check("committed-reopen-native-bytes-and-public-mutation", () => {
    const replacement = { ...original, snapshot: ' \n{"items":[0,false,"",null]} \t', created_at: stamp(-1n, 999999999), applied_at: "2",
      snapshot_version: -2147483648n, replication_version: 2147483647n, leadership_generation: 9223372036854775807n };
    function replace(row: Record<string, unknown>) {
      db.exec("BEGIN IMMEDIATE");
      try { for (const [column, value] of Object.entries(row)) if (column !== "id") update(column, value as string | bigint | null, originalID); db.exec("COMMIT"); }
      finally { if (db.inTransaction) db.exec("ROLLBACK"); }
    }
    try {
      replace(replacement); const reopened = openSQLite(path);
      try {
        assert.deepEqual(reopened.query("SELECT * FROM config_revisions WHERE id=?").get(originalID), replacement);
        const first = getSQLiteRevisionRecord(reopened, originalID)!;
        assert.equal(first.createdAt.getTime(), -1); assert.ok(Number.isNaN(first.appliedAt!.getTime()));
        assert.equal(first.leadershipGeneration, 9223372036854775808); assert.equal(first.snapshotVersion, -2147483648);
        assert.equal(first.replicationVersion, 2147483647); assert.deepEqual(first.snapshot, { items: [0, false, "", null] });
        first.createdAt.setTime(0); first.appliedAt!.setTime(0);
        (first.snapshot as unknown as { items: unknown[] }).items.push("mutated");
        assert.equal(getSQLiteRevisionRecord(reopened, originalID)!.createdAt.getTime(), -1);
        assert.ok(Number.isNaN(getSQLiteRevisionRecord(reopened, originalID)!.appliedAt!.getTime()));
        assert.deepEqual(getSQLiteRevisionRecord(reopened, originalID)!.snapshot, { items: [0, false, "", null] });
        assert.deepEqual(reopened.query("SELECT * FROM config_revisions WHERE id=?").get(originalID), replacement);
      } finally { reopened.close(); }
    } finally { replace(original); }
  });
  assert.deepEqual(rows(), originalBytes); assert.equal(db.inTransaction, false);
  console.log(`TOTAL ${cases} Bun public revision cases`);
} finally { if (db.inTransaction) db.exec("ROLLBACK"); db.close(); }
