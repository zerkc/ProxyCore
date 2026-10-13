import assert from "node:assert/strict";
import { openSQLite } from "./open.js";
import { SQLiteWriter } from "./write.js";
import { encodeTimestamp } from "./timestamp.js";

const db = openSQLite(process.argv[2]!);
const other = openSQLite(`${process.argv[2]}.other`);
try {
  let calls = 0;
  const writer = new SQLiteWriter(db, () => {
    calls++;
    return { kind: "finite", seconds: -1n, nanos: 999999999 };
  });
  const fixture = encodeTimestamp({ kind: "finite", seconds: -1n, nanos: 999999999 });
  assert.equal(db.query<{ stamp: string }, []>("SELECT stamp FROM writes WHERE origin='go'").get()?.stamp, fixture);
  const ids = new Set<string>();
  writer.write((v) => {
    assert(Object.isFrozen(v));
    assert.throws(() => writer.write(() => assert.fail("nested callback")), /nested/i);
    assert.throws(() => new SQLiteWriter(db).write(() => assert.fail("active callback")), /nested/i);
    for (let i = 0; i < 2; i++) {
      const id = v.newUUID();
      assert.match(id, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
      ids.add(id);
      v.db.query("INSERT INTO writes VALUES ('bun', ?, ?)").run(v.timestamp, id);
    }
  });
  assert.equal(calls, 1);
  assert.equal(ids.size, 2);
  writer.write(() => {});
  assert.equal(calls, 2);
  let second = 0;
  const changing = new SQLiteWriter(db, () => ({ kind: "finite", seconds: BigInt(++second), nanos: 0 }));
  const first = changing.write((v) => v.timestamp);
  assert.notEqual(changing.write((v) => v.timestamp), first);
  const count = () => db.query<{ n: bigint }, []>("SELECT count(*) AS n FROM writes").get()!.n;
  const before = count();
  assert.throws(() => writer.write((v) => {
    v.db.query("INSERT INTO writes VALUES ('bad', ?, 'id')").run(v.timestamp);
    throw new Error("callback failure");
  }), /callback failure/);
  assert.equal(count(), before);
  let invoked = false;
  assert.throws(() => writer.write(async () => { invoked = true; }), /async/i);
  assert.equal(invoked, false);
  assert.throws(() => writer.write((v) => {
    v.db.query("INSERT INTO writes VALUES ('bad', ?, 'id')").run(v.timestamp);
    return { then() {} };
  }), /async/i);
  assert.equal(count(), before);
  assert.throws(() => writer.write((v) => {
    v.db.query("INSERT INTO writes VALUES ('bad', ?, 'id')").run(v.timestamp);
    return Promise.resolve("unsupported");
  }), /async/i);
  assert.equal(count(), before);
  assert.throws(() => new SQLiteWriter(db, () => ({ kind: "finite", seconds: 0n, nanos: -1 })).write(() => assert.fail("invalid clock callback")));
  db.transaction(() => {
    assert.throws(() => writer.write(() => assert.fail("external transaction callback")), /nested/i);
  }).immediate();
  other.exec("CREATE TABLE isolated(stamp TEXT)");
  const isolated = new SQLiteWriter(other, () => ({ kind: "finite", seconds: 42n, nanos: 5 }));
  writer.write((v) => isolated.write((o) => {
    assert.notEqual(v.timestamp, o.timestamp);
    o.db.query("INSERT INTO isolated VALUES (?)").run(o.timestamp);
  }));
  assert.equal(other.query<{ n: bigint }, []>("SELECT count(*) AS n FROM isolated").get()!.n, 1n);
  writer.write(() => {});
  assert.equal(db.inTransaction, false);
} finally {
  other.close();
  db.close();
}
