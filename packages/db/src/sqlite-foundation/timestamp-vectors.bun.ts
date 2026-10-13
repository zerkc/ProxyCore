import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import { isAbsolute } from "node:path";
import { openSQLite } from "./open.js";
import { decodeTimestamp, encodeTimestamp, type Timestamp } from "./timestamp.js";

type Vector = { kind: Timestamp["kind"]; seconds?: string; nanos?: number; key: string | null };
const [fixturePath, dbPath] = process.argv.slice(2);
assert.ok(fixturePath && dbPath && isAbsolute(fixturePath) && isAbsolute(dbPath));
const data = JSON.parse(readFileSync(fixturePath, "utf8")) as {
  vectors: Vector[];
  invalidKeys: string[];
  invalidSeconds: string[];
  invalidNanos: number[];
};

function fixtureSeconds(raw: string): bigint {
  assert.equal(typeof raw, "string", "fixture seconds must not be numbers");
  const seconds = BigInt(raw);
  assert.equal(seconds.toString(), raw, "fixture seconds must be canonical decimal strings");
  assert.ok(seconds >= -(1n << 63n) && seconds < (1n << 63n));
  return seconds;
}

const keys = data.vectors.map((v) => {
  const value: Timestamp = v.kind === "finite"
    ? { kind: "finite", seconds: fixtureSeconds(v.seconds!), nanos: v.nanos! }
    : { kind: v.kind };
  const key = encodeTimestamp(value);
  assert.equal(key, v.key);
  assert.deepEqual(decodeTimestamp(v.key), value);
  return key;
});
for (const key of data.invalidKeys) assert.throws(() => decodeTimestamp(key));
for (const seconds of data.invalidSeconds) assert.throws(() => fixtureSeconds(seconds));
for (const nanos of [...data.invalidNanos, 0.5, NaN, Infinity]) {
  assert.throws(() => encodeTimestamp({ kind: "finite", seconds: 0n, nanos }));
}
for (const seconds of [-(1n << 63n) - 1n, 1n << 63n]) {
  assert.throws(() => encodeTimestamp({ kind: "finite", seconds, nanos: 0 }));
}
assert.throws(() => encodeTimestamp({ kind: "finite", seconds: 0, nanos: 0 } as unknown as Timestamp));
assert.throws(() => encodeTimestamp({ kind: "null", seconds: 0n } as unknown as Timestamp));
assert.throws(() => decodeTimestamp(undefined as unknown as string));

const db = openSQLite(dbPath);
try {
  const insert = db.query("INSERT INTO timestamps VALUES ('bun', ?)");
  db.transaction(() => {
    for (const key of [...keys].reverse()) insert.run(key);
  })();
  // NULL is counted independently, not assigned an eventual PostgreSQL ordering.
  for (const origin of ["go", "bun"]) {
    const nulls = db.query<{ count: bigint }, [string]>(
      "SELECT count(*) AS count FROM timestamps WHERE origin = ? AND key IS NULL",
    ).get(origin);
    assert.equal(nulls?.count, 1n);
    const rows = db.query<{ key: string }, [string]>(
      "SELECT key FROM timestamps WHERE origin = ? AND key IS NOT NULL ORDER BY key COLLATE BINARY",
    ).all(origin);
    assert.deepEqual(rows.map((row) => row.key), keys.slice(1));
    for (const [i, key] of keys.slice(1).entries()) {
      const bound = db.query<{ count: bigint }, [string, string | null]>(
        "SELECT count(*) AS count FROM timestamps WHERE origin = ? AND key <= ? COLLATE BINARY",
      ).get(origin, key);
      assert.equal(bound?.count, BigInt(i + 1));
    }
  }
} finally {
  db.close();
}
