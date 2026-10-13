import { strict as assert } from "node:assert";
import { dirname, join } from "node:path";
import { openSQLite } from "./open.js";

const path = process.argv[2];
assert.ok(path, "absolute database path is required");
const db = openSQLite(path);
try {
  assert.equal(db.query<{ journal_mode: string }, []>("PRAGMA journal_mode").get()?.journal_mode, "wal");
  assert.equal(db.query<{ foreign_keys: bigint }, []>("PRAGMA foreign_keys").get()?.foreign_keys, 1n);
  assert.equal(db.query<{ timeout: bigint }, []>("PRAGMA busy_timeout").get()?.timeout, 5000n);
  const values = [9007199254740993n, 9223372036854775807n, -9223372036854775808n];
  const snapshot = new TextEncoder().encode('{"format":1,"payload":"exact"}\n');
  const blob = new Uint8Array([...snapshot, 0, 255, 128]);
  const label = "shared café 🛰";
  const select = db.query<{ value: bigint; snapshot: Uint8Array; label: string; optional: null }, [bigint]>(
    "SELECT value, snapshot, label, optional FROM sample WHERE origin = 'go' AND value = ?",
  );
  for (const value of values) {
    const row = select.get(value);
    assert.ok(row);
    assert.equal(row.value, value);
    assert.deepEqual(row.snapshot, blob);
    assert.equal(row.label, label);
    assert.equal(row.optional, null);
    db.query("INSERT INTO sample VALUES (?, ?, ?, ?, ?)").run("bun", value, blob, label, null);
  }
  assert.throws(() => db.query("INSERT INTO child VALUES (?, ?)").run("missing", 1n));
  assert.throws(() => db.query("SELECT $value").get({}));
  assert.throws(() => db.query("SELECT ?").get(9223372036854775808n));
  assert.throws(() => db.query("SELECT ?").get(-9223372036854775809n));
  assert.throws(() => db.transaction(() => {
    db.query("INSERT INTO sample VALUES (?, ?, ?, ?, ?)").run("rolled-back", 1n, blob, label, null);
    throw new Error("rollback");
  })(), /rollback/);
} finally {
  db.close();
}

for (const invalid of ["", "bad\0path", dirname(path), join(dirname(path), "missing", "db.sqlite")]) {
  assert.throws(() => openSQLite(invalid));
}
