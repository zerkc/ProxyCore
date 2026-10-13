import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import { saveSQLiteSnapshotAcknowledgementRecord } from "./snapshot-acknowledgement-save-record.js";
import type { SQLiteSnapshotAcknowledgementSaveRecord } from "./snapshot-acknowledgement-save-record.js";
import { getSQLiteSnapshotAcknowledgementRecord } from "./snapshot-acknowledgement-read-record.js";
import { encodeTimestamp } from "./timestamp.js";
import type { SnapshotAcknowledgement, ContinuityTransactionPort } from "../ports.js";

type Equal<A, B> = (<T>() => T extends A ? 1 : 2) extends
  (<T>() => T extends B ? 1 : 2) ? true : false;
const alias: Equal<SQLiteSnapshotAcknowledgementSaveRecord, SnapshotAcknowledgement> = true;
const input: Equal<Parameters<typeof saveSQLiteSnapshotAcknowledgementRecord>[1], SnapshotAcknowledgement> = true;
const output: Equal<ReturnType<typeof saveSQLiteSnapshotAcknowledgementRecord>,
  ReturnType<ContinuityTransactionPort["recordSnapshotAcknowledgement"]>> = true;
const path = process.argv[2];
function open() {
  const connection = new Database(path, { strict: true, safeIntegers: true });
  connection.exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=0");
  return connection;
}
const db = open();
const node = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
const revision = "cccccccc-cccc-cccc-cccc-cccccccccccc";
const otherId = "ffffffff-ffff-ffff-ffff-ffffffffffff";
const hash = "  hash 雪 🦊  ";
const fields = ["nodeId", "contentHash", "snapshotVersion", "replicationVersion",
  "revisionId", "leadershipGeneration", "appliedAt", "receivedAt"] as const;
const columns = ["node_id", "content_hash", "snapshot_version", "replication_version",
  "revision_id", "leadership_generation", "applied_at", "received_at"];
function base(): SnapshotAcknowledgement {
  return { nodeId: node, contentHash: hash, snapshotVersion: -2147483648,
    replicationVersion: 2147483647, revisionId: revision,
    leadershipGeneration: 9007199254740992, appliedAt: new Date(-1), receivedAt: new Date(1) };
}
function stamp(ms: number) {
  const millis = BigInt(ms), remainder = ((millis % 1000n) + 1000n) % 1000n;
  return encodeTimestamp({ kind: "finite", seconds: (millis - remainder) / 1000n,
    nanos: Number(remainder) * 1_000_000 });
}
const save = (ack: object, connection = db) =>
  saveSQLiteSnapshotAcknowledgementRecord(connection, ack as SnapshotAcknowledgement);
const read = (connection = db, nodeId = node, contentHash = hash) =>
  getSQLiteSnapshotAcknowledgementRecord(connection, nodeId, contentHash);
let checks = 0, scenarios = 0;
function equal(actual: unknown, wanted: unknown) { assert.deepEqual(actual, wanted); checks++; }
function ok(value: unknown) { assert.ok(value); checks++; }
async function rejects(body: () => Promise<unknown>, wanted?: Error | RegExp | typeof RangeError) {
  await assert.rejects(body, error => {
    if (wanted instanceof Error) assert.equal(error, wanted);
    else if (wanted === RangeError) assert.ok(error instanceof RangeError);
    else if (wanted instanceof RegExp) assert.match(String(error), wanted);
    return true;
  });
  checks++;
}
function run(connection: Database, sql: string, ...bindings: any[]) {
  const statement = connection.prepare(sql);
  try { statement.run(...bindings); } finally { statement.finalize(); }
}
function values(connection = db) {
  const statement = connection.prepare(`SELECT ${columns.join(", ")} FROM main.node_snapshot_acks ORDER BY node_id, content_hash`);
  try { return statement.values(); } finally { statement.finalize(); }
}
function row(connection = db, nodeId = node, contentHash = hash) {
  const statement = connection.prepare<Record<string, unknown>, [string, string]>(`SELECT ${columns.join(", ")} FROM main.node_snapshot_acks WHERE node_id=? AND content_hash=?`);
  try { return statement.get(nodeId, contentHash); } finally { statement.finalize(); }
}
async function unchanged(ack: object, wanted?: Error | RegExp | typeof RangeError, connection = db) {
  const before = values();
  await rejects(() => save(ack, connection), wanted);
  equal(values(), before);
}
async function test(name: string, body: () => Promise<void>) {
  db.exec("DELETE FROM main.node_snapshot_acks");
  await body();
  scenarios++;
  console.log(`PASS ${name}`);
}

type Faults = { prepare?: Error; run?: Error; finalize?: Error };
// Local SQL-build/fault seam: no cache, connection ownership, clock or transaction API.
function facade(faults: Faults = {}, native = true) {
  let prepares = 0, runs = 0, finalizes = 0;
  let sql = "", bindings: unknown[] = [];
  const connection = {
    prepare(text: string) {
      prepares++; sql = text;
      ok(text.startsWith("INSERT INTO main.node_snapshot_acks ("));
      ok(!/RETURNING|BEGIN|COMMIT|SELECT/i.test(text));
      if (faults.prepare) throw faults.prepare;
      const statement = native ? db.prepare(text) : undefined;
      return {
        run(...args: any[]) {
          runs++; bindings = args;
          if (faults.run) throw faults.run;
          return statement?.run(...args);
        },
        finalize() {
          finalizes++; statement?.finalize();
          if (faults.finalize) throw faults.finalize;
        },
      };
    },
    query() { assert.fail("no caller cache access"); },
    exec() { assert.fail("no owned SQL or transaction"); },
    close() { assert.fail("caller owns connection"); },
    transaction() { assert.fail("caller owns transaction"); },
    get inTransaction() { return assert.fail("no transaction guard"); },
  } as unknown as Database;
  return { connection, counts: () => [prepares, runs, finalizes], sql: () => sql, bindings: () => bindings };
}
const expectedBindings = (ack: SnapshotAcknowledgement) => [ack.nodeId, ack.contentHash,
  ack.snapshotVersion, ack.replicationVersion, ack.revisionId, BigInt(ack.leadershipGeneration),
  stamp(ack.appliedAt.getTime()), stamp(ack.receivedAt.getTime())];
function sqlFor(insert: string[], update: string[]) {
  return `INSERT INTO main.node_snapshot_acks (${insert.join(", ")}) VALUES (${insert.map(() => "?").join(", ")})`
    + " ON CONFLICT(node_id, content_hash) DO UPDATE SET "
    + update.map(column => `${column} = excluded.${column}`).join(", ");
}
function seedRevision() {
  run(db, `INSERT INTO main.config_revisions(id,revision_number,checksum,snapshot,actor_user_id,created_at)
    VALUES(?,2,'fixture-only-second-revision','{}',NULL,'0')`, otherId);
}
function seedNode() {
  run(db, `INSERT INTO main.node_credentials(id,node_id,credential_hash,hash_version,created_at)
    VALUES(?,?,'fixture-only-dummy-hash','fixture-only-dummy-version','0')`, otherId, otherId);
  run(db, `INSERT INTO main.enrolled_nodes(node_id,installation_id,primary_id,credential_id,enrolled_at,created_by_attempt_id)
    VALUES(?,?,?,?, '0',?)`, otherId, otherId, otherId, otherId, otherId);
}

try {
  await test("eight-field bound MAIN INSERT, canonical alias/input/void and reader", async () => {
    equal([alias, input, output], [true, true, true]);
    const seam = facade(), ack = base();
    const promise = save(ack, seam.connection);
    ok(promise instanceof Promise); equal(await promise, undefined);
    equal(seam.counts(), [1, 1, 1]);
    equal(seam.sql(), sqlFor(columns, columns.slice(2)));
    equal(seam.bindings(), expectedBindings(ack));
    equal(values(), [expectedBindings(ack).map((value, index) => index === 2 || index === 3 ? BigInt(value as number) : value)]);
    equal(await read(), ack);
  });
  await test("same composite key changes exactly six nonkeys", async () => {
    await save(base());
    db.exec("SAVEPOINT second_revision");
    try {
      seedRevision();
      const updated = { ...base(), snapshotVersion: 17, replicationVersion: -23,
        revisionId: otherId, leadershipGeneration: -9007199254740992,
        appliedAt: new Date(123), receivedAt: new Date(-1001) };
      const seam = facade(); equal(await save(updated, seam.connection), undefined);
      equal(await read(), updated); equal(values().length, 1);
      equal(seam.sql(), sqlFor(columns, columns.slice(2)));
      equal(seam.bindings(), expectedBindings(updated)); equal(seam.counts(), [1, 1, 1]);
    } finally { db.exec("ROLLBACK TO second_revision; RELEASE second_revision"); }
    equal(await read(), base());
  });
  await test("distinct nodes and literal hashes are independent composite keys", async () => {
    db.exec("SAVEPOINT second_node");
    try {
      seedNode();
      const hashes = [hash, "", "HASH", "hash", "雪", "雪 🦊", "é", "e\u0301", "  text\n  ", "' OR 1=1 --"];
      for (const nodeId of [node, otherId]) {
        for (const contentHash of hashes) {
          const ack = { ...base(), nodeId, contentHash };
          const seam = facade(); await save(ack, seam.connection);
          equal(await read(db, nodeId, contentHash), ack);
          equal(seam.bindings().slice(0, 2), [nodeId, contentHash]);
          equal(seam.sql(), sqlFor(columns, columns.slice(2)));
        }
      }
      equal(values().length, hashes.length * 2);
      await save({ ...base(), contentHash: "", snapshotVersion: 7 });
      equal((await read(db, node, ""))!.snapshotVersion, 7);
      equal((await read(db, otherId, ""))!.snapshotVersion, base().snapshotVersion);
      equal(await read(), base());
    } finally { db.exec("ROLLBACK TO second_node; RELEASE second_node"); }
    equal(values(), []);
  });
  for (let index = 0; index < fields.length; index++) {
    const field = fields[index], column = columns[index];
    await test(`${field} undefined INSERT omission/SET filtering, required native rejection new/existing`, async () => {
      const ack = { ...base(), [field]: undefined }, seam = facade({}, false);
      equal(await save(ack, seam.connection), undefined); // SQL build only, NOT native durability.
      equal(seam.sql(), sqlFor(columns.filter(key => key !== column), columns.slice(2).filter(key => key !== column)));
      equal(seam.bindings(), expectedBindings(base()).filter((_, i) => i !== index));
      equal(seam.counts(), [1, 1, 1]);
      for (const existing of [false, true]) {
        if (existing) await save(base());
        const native = facade();
        await unchanged(ack, /NOT NULL/, native.connection);
        equal(native.counts(), [1, 1, 1]);
      }
    });
    await test(`${field} explicit NULL encoder bypass/native constraint new/existing`, async () => {
      const ack = { ...base(), [field]: null }, seam = facade({}, false);
      await save(ack, seam.connection);
      equal(seam.bindings(), expectedBindings(base()).map((value, i) => i === index ? null : value));
      equal(seam.sql(), sqlFor(columns, columns.slice(2)));
      for (const existing of [false, true]) {
        if (existing) await save(base());
        const native = facade();
        await unchanged(ack, /NOT NULL/, native.connection);
        equal(native.counts(), [1, 1, 1]);
      }
    });
  }
  await test("all six undefined SET rejects before every INSERT encoder/prepare/clock", async () => {
    const malformed = new Date(NaN), seam = facade();
    for (const keys of [{ nodeId: node, contentHash: hash }, {},
      { nodeId: malformed, contentHash: malformed },
      { nodeId: { toISOString() { assert.fail("no INSERT date encoding"); } }, contentHash: null }]) {
      const ack: Record<string, unknown> = { ...keys };
      for (const field of fields.slice(2)) ack[field] = undefined;
      Object.defineProperty(ack, "extra", { get() { assert.fail("unknown getter ignored"); } });
      await unchanged(ack, /No values to set/, seam.connection);
    }
    equal(seam.counts(), [0, 0, 0]);
    const closed = open(); closed.close();
    await rejects(() => save({ nodeId: malformed, contentHash: hash }, closed), /No values to set/);
    const noUpdates = { ...base(), appliedAt: malformed };
    // An invalid supplied Date is a defined SET and must reach its ISO encoder.
    await unchanged(noUpdates, RangeError, seam.connection);
    equal(seam.counts(), [0, 0, 0]);
  });
  await test("unknown fields ignored, no default Date construction or clock", async () => {
    const ack = base();
    Object.defineProperty(ack, "unknown", { get() { assert.fail("unknown getter"); } });
    Object.defineProperty(ack, "uuid", { get() { assert.fail("no UUID generation"); } });
    const OriginalDate = Date;
    globalThis.Date = new Proxy(OriginalDate, {
      construct(target, args) { equal(args.length, 1); return Reflect.construct(target, args, target); },
      get(target, property, receiver) {
        if (property === "now") return () => assert.fail("no clock reads");
        return Reflect.get(target, property, receiver);
      },
    });
    try {
      equal(await save(ack), undefined);
      await unchanged({ ...ack, receivedAt: undefined }, /NOT NULL/);
      await rejects(() => save({ nodeId: new Date(NaN), contentHash: hash }), /No values to set/);
    } finally { globalThis.Date = OriginalDate; }
    equal(await read(), base());
  });
  for (const field of ["snapshotVersion", "replicationVersion"] as const) {
    await test(`${field} signed int32 boundaries and invalid types preSQL`, async () => {
      const column = columns[fields.indexOf(field)];
      for (const value of [-2147483648, -1, -0, 0, 1, 2147483647]) {
        await save({ ...base(), [field]: value });
        equal(row()![column], BigInt(value));
        equal((await read())![field], value === 0 ? 0 : value);
      }
      for (const value of [-2147483649, 2147483648, NaN, Infinity, -Infinity, 1.5, "1", 1n, false, {}]) {
        const seam = facade(); await unchanged({ ...base(), [field]: value }, /int32/, seam.connection);
        equal(seam.counts(), [0, 0, 0]);
      }
    });
  }
  await test("leadership represented unsafe integers and signed int64 boundaries", async () => {
    for (const value of [-(2 ** 63), -(2 ** 63) + 1024, -9007199254740992, -1, -0, 0,
      9007199254740993, 9007199254740994, (2 ** 63) - 1024]) {
      await save({ ...base(), leadershipGeneration: value });
      equal(row()!.leadership_generation, BigInt(value));
      equal((await read())!.leadershipGeneration, value === 0 ? 0 : value);
    }
    for (const value of [Number((1n << 63n) - 1n), 2 ** 63, -(2 ** 63) - 2048,
      NaN, Infinity, -Infinity, 0.5, "1", 1n, false, {}]) {
      const seam = facade(); await unchanged({ ...base(), leadershipGeneration: value }, /int64/, seam.connection);
      equal(seam.counts(), [0, 0, 0]);
    }
  });
  for (const field of ["appliedAt", "receivedAt"] as const) {
    await test(`${field} finite Date/negative milliseconds/TimeClip and invalid preSQL`, async () => {
      const column = columns[fields.indexOf(field)];
      for (const ms of [-8640000000000000, -1001, -1000, -1, 0, 1, 999, 1000, 8640000000000000]) {
        await save({ ...base(), [field]: new Date(ms) });
        equal(row()![column], stamp(ms)); equal((await read())![field].getTime(), ms);
      }
      for (const value of [new Date(NaN), new Date(-8640000000000001), new Date(8640000000000001)]) {
        const seam = facade(); await unchanged({ ...base(), [field]: value }, RangeError, seam.connection);
        equal(seam.counts(), [0, 0, 0]);
      }
      for (const value of ["not Date", 0, {}, false]) {
        const seam = facade(); await unchanged({ ...base(), [field]: value }, undefined, seam.connection);
        equal(seam.counts(), [0, 0, 0]);
      }
      const error = new Error("ISO sentinel"), seam = facade();
      await unchanged({ ...base(), [field]: { toISOString() { throw error; } } }, error, seam.connection);
      equal(seam.counts(), [0, 0, 0]);
    });
  }
  await test("both ISO encoders ordered before prepare and nanoseconds become milliseconds", async () => {
    const order: string[] = [], seam = facade();
    const ack = { ...base(), appliedAt: { toISOString() { order.push("applied"); return "1969-12-31T23:59:59.999999999Z"; } },
      receivedAt: { toISOString() { order.push("received"); return "1970-01-01T00:00:00.001999999Z"; } } };
    await save(ack, seam.connection); equal(order, ["applied", "received"]);
    equal(seam.bindings().slice(6), [stamp(-1), stamp(1)]);
    equal(await read(), base());
  });
  await test("native UUID/NUL/TEXT/FK rejection with no partial replacement", async () => {
    await save(base());
    for (const field of ["nodeId", "revisionId"] as const) {
      for (const value of [node.toUpperCase(), "bad", `{${node}}`, node.replaceAll("-", ""), `${node}\0`]) {
        await unchanged({ ...base(), [field]: value });
      }
      await unchanged({ ...base(), [field]: otherId }, /FOREIGN KEY/);
    }
    for (const field of ["nodeId", "contentHash", "revisionId"] as const) {
      await unchanged({ ...base(), [field]: "bad\0text" });
      await unchanged({ ...base(), [field]: new Uint8Array([1]) });
    }
    equal(await read(), base());
  });
  await test("owned prepare/run/finalize identity, exact counts and finalizer priority", async () => {
    const prepare = new Error("prepare"), run = new Error("run"), finalize = new Error("finalize");
    for (const faults of [{ prepare }, { run }, { finalize }, { run, finalize }]) {
      const seam = facade(faults, false);
      await unchanged(base(), faults.finalize ?? faults.run ?? faults.prepare, seam.connection);
      equal(seam.counts(), faults.prepare ? [1, 0, 0] : [1, 1, 1]);
    }
    await save(base());
    const native = facade({ finalize });
    await unchanged({ ...base(), revisionId: otherId }, finalize, native.connection);
    equal(native.counts(), [1, 1, 1]);
  });
  await test("post-autocommit finalizer rejects durable INSERT and UPDATE", async () => {
    for (const existing of [false, true]) {
      if (existing) await save(base());
      const error = new Error("after native finalize"), seam = facade({ finalize: error });
      const ack = { ...base(), snapshotVersion: existing ? 42 : 41 };
      await rejects(() => save(ack, seam.connection), error);
      equal(seam.counts(), [1, 1, 1]); equal(db.inTransaction, false);
      equal(await read(), ack);
      const reopened = open(); try { equal(await read(reopened), ack); } finally { reopened.close(); }
    }
  });
  await test("MAIN ignores TEMP shadow, cached statement and connection survive", async () => {
    db.exec("CREATE TEMP TABLE node_snapshot_acks(node_id TEXT,content_hash TEXT); INSERT INTO temp.node_snapshot_acks VALUES('shadow','shadow')");
    const cached = db.query("SELECT snapshot_version FROM main.node_snapshot_acks WHERE node_id=? AND content_hash=?");
    try {
      equal(cached.get(node, hash), null); await save(base());
      equal(cached.get(node, hash), { snapshot_version: -2147483648n });
      await save({ ...base(), snapshotVersion: 3 });
      equal(db.query("SELECT snapshot_version FROM main.node_snapshot_acks WHERE node_id=? AND content_hash=?"), cached);
      equal(cached.get(node, hash), { snapshot_version: 3n });
      equal(db.query("SELECT * FROM temp.node_snapshot_acks").all(), [{ node_id: "shadow", content_hash: "shadow" }]);
    } finally { cached.finalize(); db.exec("DROP TABLE temp.node_snapshot_acks"); }
    equal((await read())!.snapshotVersion, 3);
  });
  await test("caller pending INSERT/UPDATE, savepoint/external invisibility and rollback", async () => {
    await save(base()); const before = values(), other = open();
    db.exec("BEGIN IMMEDIATE; SAVEPOINT caller");
    try {
      equal(await save({ ...base(), snapshotVersion: 7 }), undefined);
      await save({ ...base(), contentHash: "pending" });
      equal(db.inTransaction, true); equal((await read())!.snapshotVersion, 7);
      equal(values(other), before); equal(await read(other, node, "pending"), undefined);
      db.exec("ROLLBACK TO caller; RELEASE caller"); equal(values(), before);
      await save({ ...base(), replicationVersion: 8 }); equal(values(other), before);
    } finally { if (db.inTransaction) db.exec("ROLLBACK"); other.close(); }
    equal(values(), before);
    const reopened = open(); try { equal(values(reopened), before); } finally { reopened.close(); }
  });
  await test("caller COMMIT alone publishes pending rows and survives reopen", async () => {
    const other = open(); db.exec("BEGIN IMMEDIATE");
    try {
      equal(await save(base()), undefined); equal(db.inTransaction, true);
      equal(values(other), []); db.exec("COMMIT"); equal(await read(other), base());
    } finally { if (db.inTransaction) db.exec("ROLLBACK"); other.close(); }
    const reopened = open(); try { equal(await read(reopened), base()); } finally { reopened.close(); }
  });

  // Fixture-only deferred FK exercises real SQLite COMMIT failures, not mocks.
  db.exec("CREATE TABLE ack_save_parent(id TEXT PRIMARY KEY); CREATE TABLE ack_save_guard(id TEXT REFERENCES ack_save_parent(id) DEFERRABLE INITIALLY DEFERRED)");
  function trigger(kind: "INSERT" | "UPDATE") {
    db.exec(`CREATE TEMP TRIGGER ack_save_commit AFTER ${kind} ON main.node_snapshot_acks BEGIN INSERT INTO ack_save_guard VALUES('missing'); END`);
  }
  for (const kind of ["INSERT", "UPDATE"] as const) {
    await test(`${kind} real implicit COMMIT failure rolls back and recovers`, async () => {
      if (kind === "UPDATE") await save(base());
      const before = values(), seam = facade(); trigger(kind);
      try {
        await unchanged({ ...base(), snapshotVersion: 11 }, /FOREIGN KEY/, seam.connection);
        equal(seam.counts(), [1, 1, 1]); equal(db.inTransaction, false);
        equal(db.query("SELECT * FROM ack_save_guard").all(), []);
        const reopened = open(); try { equal(values(reopened), before); } finally { reopened.close(); }
      } finally { db.exec("DROP TRIGGER temp.ack_save_commit"); }
      await save({ ...base(), snapshotVersion: 12 }); equal((await read())!.snapshotVersion, 12);
    });
  }
  await test("real deferred caller COMMIT failure stays caller-owned until rollback", async () => {
    await save(base()); const before = values(), other = open(); trigger("UPDATE");
    db.exec("BEGIN IMMEDIATE; SAVEPOINT caller_failure");
    try {
      equal(await save({ ...base(), snapshotVersion: 13 }), undefined);
      equal(db.inTransaction, true); equal(values(other), before);
      assert.throws(() => db.exec("COMMIT"), /FOREIGN KEY/); checks++;
      equal(db.inTransaction, true); equal((await read())!.snapshotVersion, 13);
      db.exec("ROLLBACK TO caller_failure; RELEASE caller_failure"); equal(values(), before);
      db.exec("ROLLBACK"); equal(values(), before);
      equal(db.query("SELECT * FROM ack_save_guard").all(), []);
    } finally {
      if (db.inTransaction) db.exec("ROLLBACK");
      db.exec("DROP TRIGGER temp.ack_save_commit"); other.close();
    }
    await save({ ...base(), snapshotVersion: 14 }); equal((await read())!.snapshotVersion, 14);
  });
  await test("BUSY rejects atomically; release permits caller write and durable reopen", async () => {
    await save(base()); const before = values(), other = open();
    db.exec("PRAGMA busy_timeout=150"); other.exec("BEGIN IMMEDIATE");
    try {
      const start = performance.now(), seam = facade();
      await unchanged({ ...base(), snapshotVersion: 21, replicationVersion: 22,
        leadershipGeneration: 23, appliedAt: new Date(24), receivedAt: new Date(25) }, /busy|locked/i, seam.connection);
      equal(seam.counts(), [1, 1, 1]); equal(values(other), before);
      const elapsed = performance.now() - start; ok(elapsed >= 100 && elapsed < 2000);
      console.log(`BUSY ${elapsed.toFixed(2)}ms`);
    } finally { other.exec("ROLLBACK"); db.exec("PRAGMA busy_timeout=0"); }
    try {
      const ack = { ...base(), snapshotVersion: 31, replicationVersion: 32,
        leadershipGeneration: 33, appliedAt: new Date(34), receivedAt: new Date(35) };
      other.exec("BEGIN IMMEDIATE"); equal(await save(ack, other), undefined);
      equal(values(), before); equal(await read(other), ack);
      other.exec("COMMIT"); equal(await read(), ack);
      const reopened = open(); try { equal(await read(reopened), ack); } finally { reopened.close(); }
    } finally { if (other.inTransaction) other.exec("ROLLBACK"); other.close(); }
  });
  await test("T46 reader retains native infinity, rounded int64 and required projection", async () => {
    await save(base());
    for (const infinity of ["0", "2"]) {
      run(db, "UPDATE main.node_snapshot_acks SET applied_at=?,received_at=?,leadership_generation=?", infinity, infinity, 9007199254740993n);
      const ack = (await read())!;
      ok(Number.isNaN(ack.appliedAt.getTime())); ok(Number.isNaN(ack.receivedAt.getTime()));
      equal(ack.leadershipGeneration, Number(9007199254740993n));
      equal(Object.keys(ack).sort(), [...fields].sort());
      await unchanged({ ...base(), appliedAt: ack.appliedAt }, RangeError);
    }
  });
  await test("final durable eight-field Go oracle and reopened reader", async () => {
    equal(await save(base()), undefined); equal(await read(), base());
    const reopened = open(); try { equal(await read(reopened), base()); equal(values(reopened), values()); }
    finally { reopened.close(); }
  });
} finally { if (db.inTransaction) db.exec("ROLLBACK"); db.close(); }
console.log(`snapshot-acknowledgement-save checks=${checks} scenarios=${scenarios}`);
