import assert from "node:assert/strict";
import { Database, type Statement } from "bun:sqlite";
import { openSQLite } from "./open.js";
import { encodeTimestamp } from "./timestamp.js";
import { getSQLiteJobRow } from "./job-read.js";
import { getSQLiteJobRecord, type SQLiteJobRecord } from "./job-record.js";
import type { JobRecord } from "../ports.js";

// Structural identity guards fields and optional modifiers, not declaration origin.
// The production alias provides canonical linkage; these checks emit no runtime code.
type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2)
      ? true : false
    : false;
type Expect<T extends true> = T;
type CanonicalRecord = Expect<Identical<SQLiteJobRecord, JobRecord>>;
type CanonicalReturn = Expect<Identical<ReturnType<typeof getSQLiteJobRecord>, JobRecord | undefined>>;

const [path, originalID, originalRevision, originalCorrelation] = process.argv.slice(2);
const db = openSQLite(path);
const id = (n: number) => `aaaaaaaa-aaaa-aaaa-aaaa-${String(n).padStart(12, "0")}`;
const stamp = (seconds: bigint, nanos = 0) => encodeTimestamp({ kind: "finite", seconds, nanos })!;
const clockFields = ["created_at", "claimed_at", "started_at", "finished_at"] as const;
const clockKeys = ["createdAt", "claimedAt", "startedAt", "finishedAt"] as const;
const outputFields = ["validation_output", "apply_output", "health_output"] as const;
const outputKeys = ["validationOutput", "applyOutput", "healthOutput"] as const;
const optionalKeys = ["actorUserId", "sourcePrimaryId", "sourceNodeId", "sourceRevisionId",
  "snapshotContentHash", "snapshotVersion", "replicationVersion", "leadershipGeneration",
  "claimedAt", "startedAt", "finishedAt", ...outputKeys, "errorMessage"];
const allKeys = ["id", "revisionId", "target", "status", "source", "correlationId", "createdAt", ...optionalKeys];
let cases = 0;
function jobs() { return db.query("SELECT * FROM main.apply_jobs ORDER BY id").all(); }
function check(name: string, body: () => void) {
  body(); cases++; console.log(`PASS ${name}`);
}
function test(name: string, body: () => void) {
  check(name, () => {
    const before = jobs();
    db.exec("BEGIN");
    try { body(); assert.equal(db.inTransaction, true); }
    finally { if (db.inTransaction) db.exec("ROLLBACK"); }
    assert.deepEqual(jobs(), before);
  });
}
function insert() {
  db.query(`INSERT INTO main.apply_jobs(id,revision_id,target,status,correlation_id,created_at)
    VALUES(?,?,'combined','queued','free text NOT UUID',?)`).run(id(1), originalRevision, stamp(0n));
}
function update(name: string, value: string | bigint | null) {
  // Fixed fixture column names only; public lookup still binds the caller ID.
  db.query(`UPDATE main.apply_jobs SET ${name}=? WHERE id=?`).run(value, id(1));
}
function read(key = id(1)) {
  const before = jobs();
  const count = db.query("SELECT total_changes() AS n").get();
  const record = getSQLiteJobRecord(db, key);
  assert.deepEqual(jobs(), before);
  assert.deepEqual(db.query("SELECT total_changes() AS n").get(), count);
  if (record) assert.deepEqual(Object.keys(record).sort(), [...allKeys].sort());
  return record;
}
function driver(value: unknown, errors: { prepare?: Error; get?: Error; finalize?: Error } = {}) {
  let prepared = 0, fetched = 0, finalized = 0;
  const fake = {
    prepare(sql: string) {
      prepared++;
      assert.equal(sql, "SELECT * FROM main.apply_jobs WHERE id = ?");
      if (errors.prepare) throw errors.prepare;
      return {
        get(key: string) {
          fetched++;
          assert.equal(key, "literal ' ? ID");
          if (errors.get) throw errors.get;
          return value;
        },
        finalize() { finalized++; if (errors.finalize) throw errors.finalize; },
      };
    },
    query() { throw new Error("must reuse native uncached reader"); },
  } as unknown as Database;
  return {
    run: () => getSQLiteJobRecord(fake, "literal ' ? ID"),
    done: (counts = [1, 1, 1]) => assert.deepEqual([prepared, fetched, finalized], counts),
  };
}
try {
  const original = getSQLiteJobRow(db, originalID)!;
  const originalBytes = jobs();
  check("committed-Go-enqueue-found-missing", () => {
    const record = read(originalID)!;
    assert.equal(record.id, originalID);
    assert.equal(record.revisionId, originalRevision);
    assert.equal(record.correlationId, originalCorrelation);
    assert.equal(record.status, "queued");
    assert.equal(record.target, "combined");
    assert.equal(record.source, "ordinary");
    assert.equal(record.createdAt.getTime(), 123000);
    assert.equal(record.createdAt.toISOString(), "1970-01-01T00:02:03.000Z");
    for (const key of optionalKeys) {
      assert.equal(Object.hasOwn(record, key), true);
      assert.equal(record[key as keyof typeof record], undefined);
    }
    assert.equal(read("missing"), undefined);
    assert.equal(db.inTransaction, false);
  });
  for (const status of ["queued", "validating", "applying", "applied", "failed", "rolled-back"]) {
    test(`status-${status}`, () => { insert(); update("status", status); assert.equal(read()!.status, status); });
  }
  for (const target of ["coredns", "nginx", "combined", "certificate"]) {
    test(`target-${target}`, () => { insert(); update("target", target); assert.equal(read()!.target, target); });
  }
  for (const source of ["ordinary", "import", "sync"]) {
    test(`source-${source}`, () => { insert(); update("source", source); assert.equal(read()!.source, source); });
  }
  test("all-fields-populated-exact-public-shape", () => {
    insert();
    db.query(`INSERT INTO users(id,username,password_hash,role,created_at,updated_at)
      VALUES(?,'job-record-fixture','opaque','owner','0','2')`).run(id(9));
    for (const name of ["actor_user_id", "source_primary_id", "source_node_id", "source_revision_id"]) update(name, id(9));
    update("snapshot_content_hash", "雪🦊"); update("error_message", "");
    update("snapshot_version", -2147483648n); update("replication_version", 2147483647n);
    update("leadership_generation", 9007199254740993n);
    update("claimed_at", stamp(-1n, 999999999));
    update("started_at", stamp(0n, 1000000)); update("finished_at", stamp(86400n));
    update("validation_output", ' {"ok":false,"n":0,"s":""} ');
    update("apply_output", '[0,false,"",null]'); update("health_output", '"healthy"');
    assert.deepEqual(read(), {
      id: id(1), revisionId: originalRevision, actorUserId: id(9), target: "combined",
      status: "queued", source: "ordinary", sourcePrimaryId: id(9), sourceNodeId: id(9),
      sourceRevisionId: id(9), snapshotContentHash: "雪🦊", snapshotVersion: -2147483648,
      replicationVersion: 2147483647, leadershipGeneration: 9007199254740992,
      correlationId: "free text NOT UUID", createdAt: new Date(0), claimedAt: new Date(-1),
      startedAt: new Date(1), finishedAt: new Date(86400000),
      validationOutput: { ok: false, n: 0, s: "" }, applyOutput: [0, false, "", null],
      healthOutput: "healthy", errorMessage: "",
    });
  });
  test("SQL-NULL-own-undefined-keys", () => {
    insert(); const record = read()!;
    for (const key of optionalKeys) {
      assert.ok(Object.hasOwn(record, key)); assert.equal(record[key as keyof typeof record], undefined);
    }
  });
  test("nullable-text-empty-and-verbatim-not-truthiness", () => {
    insert();
    for (const [column, key] of [["snapshot_content_hash", "snapshotContentHash"], ["error_message", "errorMessage"]] as const) {
      for (const value of ["", "  untouched 雪🦊\n ", null]) {
        update(column, value);
        assert.equal(read()![key], value ?? undefined);
        assert.ok(Object.hasOwn(read()!, key));
      }
    }
  });
  const jsonGoldens: [string | null, unknown][] = [
    [null, undefined], ["null", undefined], ["0", 0], ["false", false], ['""', ""],
    ["42", 42], ["true", true], ['"scalar"', "scalar"], ["[]", []], ["{}", {}],
    [' [null,0,false,"",{"snow":"雪🦊"}] ', [null, 0, false, "", { snow: "雪🦊" }]],
    [' \n{"big":922337203685477580812345} \t', { big: 9.223372036854776e23 }],
    ["1e400", Infinity],
  ];
  for (let i = 0; i < outputFields.length; i++) {
    for (const [raw, expected] of jsonGoldens) {
      test(`JSON-${outputFields[i]}-${raw}`, () => {
        insert(); update(outputFields[i], raw);
        assert.deepEqual(read()![outputKeys[i]], expected);
        assert.equal(getSQLiteJobRow(db, id(1))![outputFields[i]], raw);
      });
    }
  }
  const integerGoldens: [bigint | null, number | undefined][] = [
    [null, undefined], [0n, 0], [-2147483648n, -2147483648], [2147483647n, 2147483647],
  ];
  for (const [column, key] of [["snapshot_version", "snapshotVersion"], ["replication_version", "replicationVersion"]] as const) {
    for (const [raw, expected] of integerGoldens) {
      test(`int32-${column}-${raw}`, () => { insert(); update(column, raw); assert.equal(read()![key], expected); });
    }
  }
  for (const [raw, expected] of [...integerGoldens,
    [-9223372036854775808n, -9223372036854775808], [9223372036854775807n, 9223372036854775808],
    [9007199254740993n, 9007199254740992], [-9007199254740993n, -9007199254740992],
  ] as [bigint | null, number | undefined][]) {
    test(`int64-${raw}`, () => {
      insert(); update("leadership_generation", raw);
      assert.equal(read()!.leadershipGeneration, expected);
      assert.equal(getSQLiteJobRow(db, id(1))!.leadership_generation, raw);
    });
  }
  // Independently known epoch/ISO goldens. Normalized negative seconds require
  // flooring their positive fractional milliseconds, not truncating total nanos.
  const clocks: [string, number, string | undefined][] = [
    [stamp(0n), 0, "1970-01-01T00:00:00.000Z"],
    [stamp(0n, 1), 0, "1970-01-01T00:00:00.000Z"],
    [stamp(0n, 999999), 0, "1970-01-01T00:00:00.000Z"],
    [stamp(0n, 1000000), 1, "1970-01-01T00:00:00.001Z"],
    [stamp(-1n, 999999999), -1, "1969-12-31T23:59:59.999Z"],
    [stamp(-1n, 999000000), -1, "1969-12-31T23:59:59.999Z"],
    [stamp(-1n, 998999999), -2, "1969-12-31T23:59:59.998Z"],
    [stamp(-1n, 1), -1000, "1969-12-31T23:59:59.000Z"],
    [stamp(1n, 999999999), 1999, "1970-01-01T00:00:01.999Z"],
    [stamp(2n), 2000, "1970-01-01T00:00:02.000Z"],
    [stamp(86399n, 999999999), 86399999, "1970-01-01T23:59:59.999Z"],
    [stamp(86400n), 86400000, "1970-01-02T00:00:00.000Z"],
    [stamp(-86401n, 999999999), -86400001, "1969-12-30T23:59:59.999Z"],
    [stamp(-86400n), -86400000, "1969-12-31T00:00:00.000Z"],
    [stamp(-62167219200n), -62167219200000, "0000-01-01T00:00:00.000Z"],
    [stamp(253402300800n), 253402300800000, "+010000-01-01T00:00:00.000Z"],
    [stamp(8640000000000n), 8640000000000000, "+275760-09-13T00:00:00.000Z"],
    [stamp(8640000000000n, 999999), 8640000000000000, "+275760-09-13T00:00:00.000Z"],
    [stamp(8639999999999n, 999999999), 8639999999999999, "+275760-09-12T23:59:59.999Z"],
    [stamp(8640000000000n, 1000000), NaN, undefined],
    [stamp(-8640000000000n), -8640000000000000, "-271821-04-20T00:00:00.000Z"],
    [stamp(-8640000000000n, 1000000), -8639999999999999, "-271821-04-20T00:00:00.001Z"],
    [stamp(-8640000000001n, 999999999), NaN, undefined],
    [stamp(-9223372036854775808n, 999999999), NaN, undefined],
    [stamp(9223372036854775807n), NaN, undefined],
    ["0", NaN, undefined], ["2", NaN, undefined],
  ];
  for (let i = 0; i < clockFields.length; i++) {
    for (const [raw, expected, iso] of clocks) {
      test(`clock-${clockFields[i]}-${raw}`, () => {
        insert(); update(clockFields[i], raw);
        const date = read()![clockKeys[i]]!;
        assert.ok(date instanceof Date); assert.equal(date.getTime(), expected);
        if (iso) assert.equal(date.toISOString(), iso);
        else assert.throws(() => date.toISOString(), RangeError);
        assert.equal(getSQLiteJobRow(db, id(1))![clockFields[i]], raw);
      });
    }
  }
  test("literal-ID-not-PG-normalized", () => {
    insert(); assert.equal(read()!.id, id(1));
    for (const key of ["", "missing", id(1).toUpperCase(), `{${id(1)}}`, id(1).replaceAll("-", ""),
      ` ${id(1)}`, `${id(1)} `, "' OR 1=1 --", `${id(1)}\0`, "?", "雪"]) assert.equal(read(key), undefined);
  });
  test("main-qualified-TEMP-shadow", () => {
    db.exec("CREATE TEMP TABLE apply_jobs(id TEXT)");
    try { assert.equal(read(originalID)!.id, originalID); }
    finally { db.exec("DROP TABLE temp.apply_jobs"); }
  });
  test("detached-mutable-Date-and-JSON-fresh-reads", () => {
    insert(); update("validation_output", '{"list":[{"n":1}]}'); update("apply_output", "[1]");
    update("claimed_at", stamp(1n));
    const bytes = jobs(); const first = read()!;
    first.createdAt.setTime(123); first.claimedAt!.setTime(456);
    (first.validationOutput as { list: { n: number }[] }).list[0].n = 99;
    (first.applyOutput as number[]).push(2); first.status = "failed";
    const next = read()!;
    assert.equal(next.createdAt.getTime(), 0); assert.equal(next.claimedAt!.getTime(), 1000);
    assert.deepEqual(next.validationOutput, { list: [{ n: 1 }] }); assert.deepEqual(next.applyOutput, [1]);
    assert.equal(next.status, "queued"); assert.notStrictEqual(first, next);
    assert.notStrictEqual(first.createdAt, next.createdAt); assert.notStrictEqual(first.validationOutput, next.validationOutput);
    assert.deepEqual(jobs(), bytes);
  });
  check("caller-pending-transaction-rollback-other-connection", () => {
    const other = openSQLite(path); const before = jobs(); db.exec("BEGIN IMMEDIATE");
    try {
      insert(); assert.equal(read()!.id, id(1)); assert.equal(getSQLiteJobRecord(other, id(1)), undefined);
      assert.equal(db.inTransaction, true);
    } finally { if (db.inTransaction) db.exec("ROLLBACK"); other.close(); }
    assert.deepEqual(jobs(), before); assert.equal(read(), undefined);
  });
  check("own-cursor-finalized-caller-cache-preserved", () => {
    // Bun 1.3.14 stops returning identical db.query() objects once a connection
    // has cached about 20 statements (1.4.2 keeps them), so this identity probe
    // uses a connection that holds only the statement under test.
    const cacheDb = openSQLite(path);
    const cached = cacheDb.query("SELECT * FROM main.apply_jobs WHERE id = ?"); const owned: Statement[] = [];
    const observed = new Proxy(cacheDb, {
      get(target, property) {
        if (property === "prepare") return (sql: string) => {
          const statement = target.prepare(sql); owned.push(statement); return statement;
        };
        const value = Reflect.get(target, property);
        return typeof value === "function" ? value.bind(target) : value;
      },
    });
    assert.equal(getSQLiteJobRecord(observed, originalID)!.id, originalID);
    assert.equal(getSQLiteJobRecord(observed, "missing"), undefined);
    assert.equal(owned.length, 2); for (const statement of owned) assert.throws(() => statement.get(originalID));
    assert.deepEqual(cached.get(originalID), original);
    assert.strictEqual(cacheDb.query("SELECT * FROM main.apply_jobs WHERE id = ?"), cached);
    cacheDb.close();
  });
  const invalid: [string, unknown][] = [
    ["JSON", { ...original, validation_output: "{" }],
    ["timestamp-tag", { ...original, created_at: "3" }],
    ["timestamp-nanos", { ...original, claimed_at: "100000000000000000000.1000000000" }],
    ["timestamp-seconds", { ...original, finished_at: "118446744073709551616.000000000" }],
    ["type", { ...original, created_at: 0 }], ["required-null", { ...original, created_at: null }],
    ["numeric-driver", { ...original, leadership_generation: 1 }],
    ["int32-high", { ...original, snapshot_version: 2147483648n }],
    ["int32-low", { ...original, replication_version: -2147483649n }],
    ["domain", { ...original, status: "cancelled" }], ["undefined-row", undefined],
  ];
  for (const [name, value] of invalid) {
    check(`inherited-invalid-${name}`, () => { const probe = driver(value); assert.throws(probe.run); probe.done(); });
  }
  check("missing-only-native-null", () => { const probe = driver(null); assert.equal(probe.run(), undefined); probe.done(); });
  for (const phase of ["prepare", "get", "finalize"] as const) {
    check(`inherited-error-identity-${phase}`, () => {
      const error = new Error(`injected ${phase}`); const probe = driver(original, { [phase]: error });
      assert.throws(probe.run, (caught) => caught === error); probe.done(phase === "prepare" ? [1, 0, 0] : [1, 1, 1]);
    });
  }
  check("finalize-errors-on-missing-and-invalid-not-absence", () => {
    for (const value of [null, { ...original, health_output: "{" }]) {
      const error = new Error("finalizer"); const probe = driver(value, { finalize: error });
      assert.throws(probe.run, (caught) => caught === error); probe.done();
    }
  });
  check("runtime-non-string-rejected-before-prepare", () => {
    const fake = { prepare() { assert.fail("non-string must fail before prepare"); } } as unknown as Database;
    assert.throws(() => getSQLiteJobRecord(fake, 1 as unknown as string), TypeError);
  });
  test("real-query-error-and-reuse", () => {
    db.exec("ALTER TABLE main.apply_jobs RENAME TO job_record_hidden");
    assert.throws(() => getSQLiteJobRecord(db, originalID));
    db.exec("ALTER TABLE main.job_record_hidden RENAME TO apply_jobs"); assert.equal(read(originalID)!.id, originalID);
  });
  test("real-validation-error-and-reuse", () => {
    insert(); db.exec("PRAGMA ignore_check_constraints = ON");
    try {
      for (const [column, value, pattern] of [["status", "invalid", /domain/], ["validation_output", "{", /JSON/],
        ["created_at", "bad-tag", /timestamp/]] as const) {
        const previous = getSQLiteJobRow(db, id(1))![column]; update(column, value);
        const bytes = jobs(); assert.throws(() => getSQLiteJobRecord(db, id(1)), pattern); assert.deepEqual(jobs(), bytes);
        update(column, previous); assert.equal(read()!.id, id(1));
      }
    } finally { db.exec("PRAGMA ignore_check_constraints = OFF"); }
  });
  check("closed-errors-and-reopened-committed-native-bytes", () => {
    const closed = openSQLite(path); closed.close();
    assert.throws(() => getSQLiteJobRecord(closed, originalID)); assert.throws(() => getSQLiteJobRecord(closed, "missing"));
    const reopened = openSQLite(path);
    try {
      assert.deepEqual(getSQLiteJobRow(reopened, originalID), original);
      assert.equal(getSQLiteJobRecord(reopened, originalID)!.createdAt.getTime(), 123000);
    } finally { reopened.close(); }
  });
  check("committed-projection-reopen-preserves-lossless-native-values", () => {
    // Commit non-default values on the real migrated Go job, then restore its
    // original bytes in a second commit. No deployed DB or synthetic schema.
    const replacement = {
      ...original, created_at: stamp(-1n, 999999999), claimed_at: "0", started_at: "2",
      finished_at: stamp(9223372036854775807n, 999999999),
      snapshot_version: -2147483648n, replication_version: 2147483647n,
      leadership_generation: 9223372036854775807n,
      validation_output: ' \n{"items":[0,false,"",null]} \t', apply_output: "null", health_output: "false",
    };
    function replace(row: typeof replacement | typeof original) {
      db.exec("BEGIN IMMEDIATE");
      try {
        for (const [column, value] of Object.entries(row)) {
          if (column !== "id") db.query(`UPDATE main.apply_jobs SET ${column}=? WHERE id=?`).run(value, originalID);
        }
        db.exec("COMMIT");
      } finally { if (db.inTransaction) db.exec("ROLLBACK"); }
    }
    try {
      replace(replacement);
      const reopened = openSQLite(path);
      try {
        assert.deepEqual(getSQLiteJobRow(reopened, originalID), replacement);
        const first = getSQLiteJobRecord(reopened, originalID)!;
        assert.equal(first.createdAt.toISOString(), "1969-12-31T23:59:59.999Z");
        for (const key of ["claimedAt", "startedAt", "finishedAt"] as const) assert.ok(Number.isNaN(first[key]!.getTime()));
        assert.equal(first.snapshotVersion, -2147483648); assert.equal(first.replicationVersion, 2147483647);
        assert.equal(first.leadershipGeneration, 9223372036854775808);
        assert.deepEqual(first.validationOutput, { items: [0, false, "", null] });
        assert.equal(first.applyOutput, undefined); assert.equal(first.healthOutput, false);
        first.createdAt.setTime(0); (first.validationOutput as { items: unknown[] }).items.push("mutated");
        assert.deepEqual(getSQLiteJobRow(reopened, originalID), replacement);
        assert.equal(getSQLiteJobRecord(reopened, originalID)!.createdAt.getTime(), -1);
        assert.deepEqual(getSQLiteJobRecord(reopened, originalID)!.validationOutput, { items: [0, false, "", null] });
      } finally { reopened.close(); }
    } finally { replace(original); }
  });
  assert.deepEqual(jobs(), originalBytes); assert.equal(db.inTransaction, false);
  console.log(`TOTAL ${cases} Bun public record cases`);
} finally { if (db.inTransaction) db.exec("ROLLBACK"); db.close(); }
