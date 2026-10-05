import assert from "node:assert/strict";
import { Database, type Statement } from "bun:sqlite";
import { openSQLite } from "./open.js";
import { encodeTimestamp } from "./timestamp.js";
import { getSQLiteJobRow } from "./job-read.js";

const [path, originalID, originalRevision, originalCorrelation] = process.argv.slice(2);
const db = openSQLite(path);
const id = (n: number) => `aaaaaaaa-aaaa-aaaa-aaaa-${String(n).padStart(12, "0")}`;
const stamp = (seconds: bigint, nanos = 0) => encodeTimestamp({ kind: "finite", seconds, nanos })!;
const raw = ' \n{"big":922337203685477580812345,"snow":"雪🦊"} \t';
const required = ["id", "revision_id", "correlation_id", "created_at", "target", "status", "source"];
const nullable = ["actor_user_id", "source_primary_id", "source_node_id", "source_revision_id",
  "snapshot_content_hash", "validation_output", "apply_output", "health_output", "error_message",
  "claimed_at", "started_at", "finished_at"];
const integerNames = ["snapshot_version", "replication_version", "leadership_generation"];
let cases = 0;
function jobs() { return db.query("SELECT * FROM main.apply_jobs ORDER BY id").all(); }
function check(name: string, body: () => void) {
  body();
  cases++;
  console.log(`PASS ${name}`);
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
    VALUES(?,?,'combined','queued','free text NOT a UUID','0')`).run(id(1), originalRevision);
}
function update(name: string, value: string | bigint | null) {
  // Column names come only from this fixture's fixed arrays, never reader input.
  db.query(`UPDATE main.apply_jobs SET ${name}=? WHERE id=?`).run(value, id(1));
}
function readUnchanged(key = id(1)) {
  const before = jobs();
  const count = db.query("SELECT total_changes() AS n").get();
  const row = getSQLiteJobRow(db, key);
  assert.deepEqual(jobs(), before);
  assert.deepEqual(db.query("SELECT total_changes() AS n").get(), count);
  if (row) {
    assert.deepEqual(row, db.query("SELECT * FROM main.apply_jobs WHERE id=?").get(key));
    assert.ok(Object.isFrozen(row));
  }
  return row;
}
// A private fake driver boundary can inject impossible row shapes without
// weakening the production schema. It also observes exact prepare/get/finalize.
function boundary(value: unknown, getError?: Error) {
  let prepares = 0, gets = 0, finalized = 0;
  const fake = {
    prepare(sql: string) {
      prepares++;
      assert.equal(sql, "SELECT * FROM main.apply_jobs WHERE id = ?");
      return {
        get(key: string) {
          gets++;
          assert.equal(key, "literal ' ? ID");
          if (getError) throw getError;
          return value;
        },
        finalize() { finalized++; },
      };
    },
    query() { throw new Error("reader must not use cached query or reference lookup"); },
  } as unknown as Database;
  return {
    run: () => getSQLiteJobRow(fake, "literal ' ? ID"),
    done: () => assert.deepEqual([prepares, gets, finalized], [1, 1, 1]),
  };
}
try {
  const original = db.query("SELECT * FROM main.apply_jobs WHERE id=?").get(originalID)!;
  check("Go enqueue found/absent", () => {
    const row = readUnchanged(originalID)!;
    assert.equal(row.revision_id, originalRevision);
    assert.equal(row.correlation_id, originalCorrelation);
    assert.equal(row.status, "queued");
    assert.equal(getSQLiteJobRow(db, "absent"), undefined);
    assert.equal(db.inTransaction, false);
  });
  for (const status of ["queued", "validating", "applying", "applied", "failed", "rolled-back"]) {
    test(`status-${status}`, () => {
      insert(); update("status", status);
      assert.equal(readUnchanged()!.status, status);
    });
  }
  for (const target of ["coredns", "nginx", "combined", "certificate"]) {
    test(`target-${target}`, () => {
      insert(); update("target", target);
      assert.equal(readUnchanged()!.target, target);
    });
  }
  for (const source of ["ordinary", "import", "sync"]) {
    test(`source-${source}`, () => {
      insert(); update("source", source);
      assert.equal(readUnchanged()!.source, source);
    });
  }
  test("all-nullable-SQL-NULL", () => {
    insert();
    for (const name of [...nullable, ...integerNames]) update(name, null);
    const row = readUnchanged()!;
    for (const name of [...nullable, ...integerNames]) assert.equal(row[name as keyof typeof row], null);
  });
  test("all-columns-populated-detached", () => {
    insert();
    db.query(`INSERT INTO users(id,username,password_hash,role,created_at,updated_at)
      VALUES(?,'job-read-fixture','opaque','owner','0','2')`).run(id(9));
    for (const name of nullable) {
      update(name, name.endsWith("_id") ? id(9) : name.endsWith("_at") ? stamp(-1n, 999999999)
        : name.endsWith("_output") ? raw : "");
    }
    for (const name of integerNames) update(name, 1n);
    const row = readUnchanged()!;
    assert.equal(row.validation_output, raw);
    assert.equal(row.correlation_id, "free text NOT a UUID");
    assert.throws(() => { (row as { status: string }).status = "failed"; }, TypeError);
    update("status", "failed");
    assert.equal(row.status, "queued");
    assert.equal(readUnchanged()!.status, "failed");
  });
  for (const name of ["validation_output", "apply_output", "health_output"]) {
    for (const text of [null, "null", "42", "true", '"scalar"', "[]", raw]) {
      test(`JSON-${name}-${text}`, () => {
        insert(); update(name, text);
        assert.equal(readUnchanged()![name as "validation_output"], text);
      });
    }
  }
  for (const name of integerNames) {
    const bounds = name === "leadership_generation"
      ? [-(1n << 63n), (1n << 63n) - 1n, 9007199254740993n]
      : [-(1n << 31n), (1n << 31n) - 1n];
    for (const value of [...bounds, 0n, null]) {
      test(`integer-${name}-${value}`, () => {
        insert(); update(name, value);
        assert.equal(readUnchanged()![name as "snapshot_version"], value);
      });
    }
  }
  for (const name of ["created_at", "claimed_at", "started_at", "finished_at"]) {
    for (const text of ["0", "2", stamp(-9223372036854775808n, 1), stamp(9223372036854775807n, 999999999), stamp(-1n, 123456789)]) {
      test(`timestamp-${name}-${text}`, () => {
        insert(); update(name, text);
        assert.equal(readUnchanged()![name as "created_at"], text);
      });
    }
  }
  test("literal-ID-bound-not-normalized", () => {
    insert(); assert.equal(readUnchanged()!.id, id(1));
    for (const key of ["", "absent", id(1).toUpperCase(), `{${id(1)}}`, id(1).replaceAll("-", ""),
      ` ${id(1)}`, `${id(1)} `, "' OR 1=1 --", `${id(1)}\0`, "?", "雪"]) {
      assert.equal(readUnchanged(key), undefined);
    }
  });
  test("main-qualified-despite-TEMP-shadow", () => {
    db.exec("CREATE TEMP TABLE apply_jobs(id TEXT)");
    try { assert.deepEqual(readUnchanged(originalID), original); }
    finally { db.exec("DROP TABLE temp.apply_jobs"); }
  });
  check("caller-pending-transaction-and-rollback", () => {
    const other = openSQLite(path);
    const before = jobs();
    db.exec("BEGIN IMMEDIATE");
    try {
      insert();
      assert.equal(readUnchanged()!.id, id(1));
      assert.equal(getSQLiteJobRow(other, id(1)), undefined);
      assert.equal(db.inTransaction, true);
    } finally {
      if (db.inTransaction) db.exec("ROLLBACK");
      other.close();
    }
    assert.deepEqual(jobs(), before);
    assert.equal(getSQLiteJobRow(db, id(1)), undefined);
  });
  check("own-statement-finalized-caller-cache-preserved", () => {
    const cached = db.query("SELECT * FROM main.apply_jobs WHERE id = ?");
    const owned: Statement[] = [];
    const observed = new Proxy(db, {
      get(target, property) {
        if (property === "prepare") return (sql: string) => {
          const statement = target.prepare(sql);
          owned.push(statement);
          return statement;
        };
        const value = Reflect.get(target, property);
        return typeof value === "function" ? value.bind(target) : value;
      },
    });
    assert.deepEqual(getSQLiteJobRow(observed, originalID), original);
    assert.equal(getSQLiteJobRow(observed, "missing"), undefined);
    assert.equal(owned.length, 2);
    for (const statement of owned) assert.throws(() => statement.get(originalID));
    assert.deepEqual(cached.get(originalID), original);
    assert.strictEqual(db.query("SELECT * FROM main.apply_jobs WHERE id = ?"), cached);
  });
  test("numeric-driver-projection-rejected", () => {
    insert(); update("leadership_generation", 9223372036854775807n);
    const projected = new Proxy(db, {
      get(target, property) {
        if (property === "prepare") return (sql: string) => {
          const statement = target.prepare(sql);
          // Real Bun supports this statement option; installed declarations
          // omit it. Exercise the native numeric projection, not a fake row.
          (statement as unknown as { safeIntegers(enabled: boolean): void }).safeIntegers(false);
          return statement;
        };
        const value = Reflect.get(target, property);
        return typeof value === "function" ? value.bind(target) : value;
      },
    });
    assert.throws(() => getSQLiteJobRow(projected, id(1)), /integer/);
  });
  // Every column must be own/present, even if null is a valid value.
  for (const name of [...required, ...nullable, ...integerNames]) {
    check(`missing-column-${name}`, () => {
      const row = { ...original } as Record<string, unknown>;
      delete row[name];
      const probe = boundary(row); assert.throws(probe.run, /columns/); probe.done();
    });
  }
  const invalid: unknown[] = [undefined, false, 42, "row", [], { ...original, extra: null }];
  for (const name of [...required, ...nullable]) {
    for (const value of [undefined, 42, {}, "bad\0text", ...(required.includes(name) ? [null] : [])]) {
      invalid.push({ ...original, [name]: value });
    }
  }
  for (const name of ["id", "revision_id", "actor_user_id", "source_primary_id", "source_node_id", "source_revision_id"]) {
    for (const value of ["not-a-UUID", id(1).toUpperCase(), `{${id(1)}}`]) invalid.push({ ...original, [name]: value });
  }
  for (const [name, value] of [["status", "cancelled"], ["status", "QUEUED"], ["target", "unknown"], ["source", "IMPORT"]]) {
    invalid.push({ ...original, [name]: value });
  }
  for (const name of ["created_at", "claimed_at", "started_at", "finished_at"]) {
    for (const value of ["", "2026-01-01", "3", "118446744073709551616.000000000", "100000000000000000000.00000000x"]) {
      invalid.push({ ...original, [name]: value });
    }
  }
  for (const name of ["validation_output", "apply_output", "health_output"]) {
    for (const value of ["", "{", "undefined", "NaN"]) invalid.push({ ...original, [name]: value });
  }
  for (const name of integerNames) {
    const bits = name === "leadership_generation" ? 63n : 31n;
    for (const value of [0, Number.MAX_SAFE_INTEGER + 1, "1", undefined, false, -(1n << bits) - 1n, 1n << bits]) {
      invalid.push({ ...original, [name]: value });
    }
  }
  for (const value of invalid) {
    check(`invalid-row-${cases}`, () => {
      const probe = boundary(value); assert.throws(probe.run); probe.done();
    });
  }
  check("fake-missing-is-only-null-and-valid-is-detached", () => {
    const missing = boundary(null); assert.equal(missing.run(), undefined); missing.done();
    const source: Record<string, unknown> = { ...original, actor_user_id: id(99), revision_id: id(98) };
    const valid = boundary(source);
    const row = valid.run()!;
    valid.done();
    assert.notStrictEqual(row, source);
    assert.ok(Object.isFrozen(row));
    source.status = "failed";
    assert.equal(row.status, "queued");
  });
  check("query-errors-propagate-finalize", () => {
    const error = new Error("injected get failure");
    const probe = boundary(null, error);
    assert.throws(probe.run, (caught) => caught === error); probe.done();
    const prepareError = new Error("injected prepare failure");
    const failing = { prepare() { throw prepareError; } } as unknown as Database;
    assert.throws(() => getSQLiteJobRow(failing, "x"), (caught) => caught === prepareError);
  });
  test("real-query-error-then-reuse", () => {
    db.exec("ALTER TABLE main.apply_jobs RENAME TO job_read_fixture_hidden");
    assert.throws(() => getSQLiteJobRow(db, originalID));
    db.exec("ALTER TABLE main.job_read_fixture_hidden RENAME TO apply_jobs");
    assert.deepEqual(readUnchanged(originalID), original);
  });
  test("real-validation-error-then-reuse", () => {
    insert();
    db.exec("PRAGMA ignore_check_constraints = ON");
    try {
      update("status", "invalid");
      const before = jobs();
      assert.throws(() => getSQLiteJobRow(db, id(1)), /domain/);
      assert.deepEqual(jobs(), before);
      update("status", "queued");
      assert.equal(readUnchanged()!.status, "queued");
    } finally { db.exec("PRAGMA ignore_check_constraints = OFF"); }
  });
  check("closed-DB-error-and-reopen", () => {
    const closed = openSQLite(path);
    closed.close();
    assert.throws(() => getSQLiteJobRow(closed, originalID));
    assert.throws(() => getSQLiteJobRow(closed, "missing"));
    const reopened = openSQLite(path);
    try { assert.deepEqual(getSQLiteJobRow(reopened, originalID), original); }
    finally { reopened.close(); }
  });
  assert.deepEqual(jobs(), [original]);
  assert.equal(db.inTransaction, false);
  console.log(`TOTAL ${cases} Bun reader cases`);
} finally {
  if (db.inTransaction) db.exec("ROLLBACK");
  db.close();
}
