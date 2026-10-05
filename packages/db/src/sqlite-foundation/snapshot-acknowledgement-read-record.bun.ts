import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import { getSQLiteSnapshotAcknowledgementRecord } from "./snapshot-acknowledgement-read-record.js";
import type { SQLiteSnapshotAcknowledgementRecord } from "./snapshot-acknowledgement-read-record.js";
import type { SnapshotAcknowledgement, ContinuityTransactionPort } from "../ports.js";

const read: (db: Database, nodeId: string, contentHash: string) =>
  ReturnType<ContinuityTransactionPort["getSnapshotAcknowledgement"]> = getSQLiteSnapshotAcknowledgementRecord;
type Equal<A, B> = (<T>() => T extends A ? 1 : 2) extends
  (<T>() => T extends B ? 1 : 2) ? true : false;
const linked: Equal<SQLiteSnapshotAcknowledgementRecord, SnapshotAcknowledgement> = true;
const promised: Equal<ReturnType<typeof read>, Promise<SnapshotAcknowledgement | undefined>> = true;

const path = process.argv[2];
function open() {
  const connection = new Database(path, { strict: true, safeIntegers: true });
  connection.exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=0");
  return connection;
}
const db = open();
const node = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
const revision = "cccccccc-cccc-cccc-cccc-cccccccccccc";
const missing = "ffffffff-ffff-ffff-ffff-ffffffffffff";
const hash = "  hash 雪 🦊  ";
const columns = ["node_id", "content_hash", "snapshot_version", "replication_version",
  "revision_id", "leadership_generation", "applied_at", "received_at"];
const fields = ["nodeId", "contentHash", "snapshotVersion", "replicationVersion",
  "revisionId", "leadershipGeneration", "appliedAt", "receivedAt"];
const expected: SnapshotAcknowledgement = {
  nodeId: node, contentHash: hash, snapshotVersion: -2147483648,
  replicationVersion: 2147483647, revisionId: revision,
  leadershipGeneration: Number(9007199254740993n),
  appliedAt: new Date(-1), receivedAt: new Date(1),
};
function rawRow(): Record<string, unknown> {
  return {
    node_id: node, content_hash: hash, snapshot_version: -2147483648n,
    replication_version: 2147483647n, revision_id: revision,
    leadership_generation: 9007199254740993n,
    applied_at: "109223372036854775807.999999999",
    received_at: "109223372036854775808.001999999",
  };
}
let checks = 0, scenarios = 0;
function equal(actual: unknown, wanted: unknown) { assert.deepEqual(actual, wanted); checks++; }
function ok(value: unknown) { assert.ok(value); checks++; }
async function rejects(body: () => Promise<unknown>, wanted?: Error | RegExp) {
  await assert.rejects(body, error => {
    if (wanted instanceof Error) assert.equal(error, wanted);
    else if (wanted) assert.match(String(error), wanted);
    return true;
  });
  checks++;
}
async function test(name: string, body: () => Promise<void>) {
  await body();
  scenarios++;
  console.log(`PASS ${name}`);
}
function values(connection = db) {
  const statement = connection.prepare(`SELECT ${columns.join(", ")} FROM main.node_snapshot_acks ORDER BY node_id, content_hash`);
  try { return statement.values(); } finally { statement.finalize(); }
}
const initial = values();
function run(connection: Database, sql: string, ...bindings: any[]) {
  const statement = connection.prepare(sql);
  try { statement.run(...bindings); } finally { statement.finalize(); }
}

type Owned = { gets: number; finalizes: number; args: unknown[]; closed: boolean };
type Faults = { prepare?: Error; get?: Error; finalize?: Error };
// No default: an explicit undefined malformed row must never be masked.
function facade(value: unknown, faults: Faults = {}, native = false) {
  const owned: Owned[] = [], sql: string[] = [];
  const connection = {
    prepare(query: string) {
      sql.push(query);
      if (faults.prepare) throw faults.prepare;
      const statement = native ? db.prepare(query) : undefined;
      const observation: Owned = { gets: 0, finalizes: 0, args: [], closed: false };
      owned.push(observation);
      return {
        get(...args: any[]) {
          observation.gets++;
          observation.args = args;
          if (faults.get) throw faults.get;
          return statement ? statement.get(...args) : value;
        },
        finalize() {
          observation.finalizes++;
          statement?.finalize();
          observation.closed = true;
          if (faults.finalize) throw faults.finalize;
        },
      };
    },
    query() { assert.fail("no caller cache access"); },
    exec() { assert.fail("no writes or owned transaction SQL"); },
    close() { assert.fail("caller owns connection"); },
    transaction() { assert.fail("caller owns transaction"); },
    get inTransaction() { return assert.fail("no transaction guard"); },
  } as unknown as Database;
  return { connection, owned, sql };
}
function finished(seam: ReturnType<typeof facade>, nodeId = node, contentHash = hash) {
  equal(seam.owned.length, 1);
  equal(seam.owned[0], { gets: 1, finalizes: 1, args: [nodeId, contentHash], closed: true });
}
async function project(row: unknown) {
  const seam = facade(row);
  const result = await read(seam.connection, node, hash);
  finished(seam);
  return result;
}
async function invalid(row: unknown, wanted?: RegExp) {
  const seam = facade(row);
  await rejects(() => read(seam.connection, node, hash), wanted);
  finished(seam);
}
function timestamp(seconds: bigint, nanos = 0) {
  return `1${(seconds + (1n << 63n)).toString().padStart(20, "0")}.${nanos.toString().padStart(9, "0")}`;
}
async function change(key: string, value: unknown, body: () => Promise<void>) {
  ok(columns.includes(key));
  db.exec("SAVEPOINT reader_case");
  try {
    run(db, `UPDATE main.node_snapshot_acks SET ${key}=? WHERE node_id=? AND content_hash=?`, value, node, hash);
    await body();
  } finally { db.exec("ROLLBACK TO reader_case; RELEASE reader_case"); }
  equal(values(), initial);
}
function nativeReject(sql: string, ...bindings: any[]) {
  db.exec("SAVEPOINT native_constraint");
  try {
    assert.throws(() => run(db, sql, ...bindings));
    checks++;
  } finally { db.exec("ROLLBACK TO native_constraint; RELEASE native_constraint"); }
  equal(values(), initial);
}

try {
  await test("all eight canonical required fields, linked type and Promise", async () => {
    equal(linked, true);
    equal(promised, true);
    equal(initial.length, 2);
    const record = await read(db, node, hash);
    equal(record, expected);
    equal(Object.keys(record!).sort(), [...fields].sort());
    const again = await read(db, node, hash);
    equal(again, expected);
    ok(again !== record);
    ok(again!.appliedAt !== record!.appliedAt);
    ok(again!.receivedAt !== record!.receivedAt);
    equal(values(), initial);
  });
  await test("explicit MAIN SELECT with independently bound literal keys", async () => {
    const seam = facade(undefined, {}, true);
    equal(await read(seam.connection, node, hash), expected);
    equal(seam.sql, [`SELECT ${columns.join(", ")} FROM main.node_snapshot_acks WHERE node_id = ? AND content_hash = ? LIMIT 1`]);
    finished(seam);
    const lookups: [string, string][] = [
      [missing, hash], [node, "missing"], [revision, hash], [hash, node],
      [node.toUpperCase(), hash], [`{${node}}`, hash], [node.replaceAll("-", ""), hash],
      [` ${node}`, hash], [`${node} `, hash], [`${node}\n`, hash], [`${node}\0`, hash],
      ["", hash], ["not-a-uuid", hash], [node, hash.toUpperCase()],
      [node, hash.trim()], [node, "  hash 雪 🦊 "], [node, `${hash}\0`],
      ["' OR 1=1 --", hash], [node, "' OR 1=1 --"],
      [node, `${hash}' UNION SELECT * FROM node_snapshot_acks --`],
    ];
    for (const [nodeId, contentHash] of lookups) {
      equal(await read(db, nodeId, contentHash), undefined);
      const absent = facade(undefined, {}, true);
      equal(await read(absent.connection, nodeId, contentHash), undefined);
      finished(absent, nodeId, contentHash);
      equal(absent.sql, seam.sql);
    }
    equal(await read(db, node, ""), {
      nodeId: node, contentHash: "", snapshotVersion: 0, replicationVersion: 0,
      revisionId: revision, leadershipGeneration: 0, appliedAt: new Date(NaN), receivedAt: new Date(NaN),
    });
    const bad = facade(rawRow());
    for (const value of [null, undefined, 1, {}, new String(node)]) {
      await rejects(() => read(bad.connection, value as string, hash), /must be strings/);
      await rejects(() => read(bad.connection, node, value as string), /must be strings/);
    }
    equal(bad.sql, []);
    equal(values(), initial);
  });
  await test("hash empty, whitespace, case and Unicode exact preservation", async () => {
    // Empty hash is separately seeded because the composite PK already contains it.
    for (const value of ["HASH", "hash", "雪", "雪 🦊", "é", "e\u0301", "  text\n  ", "' OR 1=1 --"]) {
      equal((await project({ ...rawRow(), content_hash: value }))!.contentHash, value);
      await change("content_hash", value, async () => {
        equal(await read(db, node, hash), undefined);
        equal((await read(db, node, value))!.contentHash, value);
        equal(await read(db, missing, value), undefined);
      });
    }
    equal((await project({ ...rawRow(), content_hash: "" }))!.contentHash, "");
  });
  await test("every required NULL, missing, undefined, wrong type and row shape rejects", async () => {
    for (const value of [undefined, false, 0, "", [], {}]) await invalid(value);
    await invalid({ ...rawRow(), extra: true }, /columns/);
    for (const key of columns) {
      const removed = rawRow();
      delete removed[key];
      await invalid(removed, /columns/);
      for (const value of [null, undefined, false, {}, [], new Uint8Array()]) {
        await invalid({ ...rawRow(), [key]: value });
      }
      nativeReject(`UPDATE main.node_snapshot_acks SET ${key}=NULL WHERE node_id=? AND content_hash=?`, node, hash);
    }
    for (const key of ["node_id", "revision_id"]) {
      for (const value of [node.toUpperCase(), `{${node}}`, node.replaceAll("-", ""),
        node.slice(0, -1), `${node}x`, `${node}\n`, "g".repeat(36), "", 1, 1n, `${node}\0`]) {
        await invalid({ ...rawRow(), [key]: value }, /UUID|TEXT/);
      }
    }
    for (const key of ["content_hash", "node_id", "revision_id", "applied_at", "received_at"]) {
      for (const value of [1, 1n, "bad\0text"]) await invalid({ ...rawRow(), [key]: value }, /TEXT/);
    }
  });
  await test("both int32 boundaries and signed int64 Number rounding", async () => {
    for (const key of ["snapshot_version", "replication_version"]) {
      const property = fields[columns.indexOf(key)] as keyof SnapshotAcknowledgement;
      for (const value of [-2147483648n, -1n, 0n, 1n, 2147483647n]) {
        equal((await project({ ...rawRow(), [key]: value }))![property], Number(value));
        await change(key, value, async () => equal((await read(db, node, hash))![property], Number(value)));
      }
      for (const value of [-2147483649n, 2147483648n, 0, 1.5, NaN, Infinity, "1", true, {}]) {
        await invalid({ ...rawRow(), [key]: value }, /INTEGER/);
      }
    }
    for (const value of [-(1n << 63n), -9007199254740993n, -1n, 0n,
      9007199254740993n, (1n << 63n) - 1n]) {
      equal((await project({ ...rawRow(), leadership_generation: value }))!.leadershipGeneration, Number(value));
      await change("leadership_generation", value, async () => {
        equal((await read(db, node, hash))!.leadershipGeneration, Number(value));
      });
    }
    for (const value of [-(1n << 63n) - 1n, 1n << 63n, 0, 1.5, NaN, Infinity, "1", false]) {
      await invalid({ ...rawRow(), leadership_generation: value }, /INTEGER/);
    }
  });
  await test("both public Dates: nanos, pre-epoch, TimeClip, infinity and malformed", async () => {
    const cases: [string, number][] = [
      [timestamp(-1n, 999999999), -1], [timestamp(-1n, 1), -1000],
      [timestamp(0n, 999999), 0], [timestamp(0n, 1000000), 1],
      [timestamp(0n, 999999999), 999],
      [timestamp(-8640000000000n), -8640000000000000],
      [timestamp(8640000000000n), 8640000000000000],
      [timestamp(-8640000000001n, 999999999), NaN],
      [timestamp(8640000000000n, 1000000), NaN],
      [timestamp(-(1n << 63n)), NaN], [timestamp((1n << 63n) - 1n), NaN],
      ["0", NaN], ["2", NaN],
    ];
    for (const key of ["applied_at", "received_at"]) {
      const property = key === "applied_at" ? "appliedAt" : "receivedAt";
      for (const [value, milliseconds] of cases) {
        equal((await project({ ...rawRow(), [key]: value }))![property].getTime(), milliseconds);
        await change(key, value, async () => {
          equal((await read(db, node, hash))![property].getTime(), milliseconds);
        });
      }
      for (const value of ["", "1", "3", "2025-01-01", " 0", "2\n", "0\0", 0, 1n,
        timestamp(0n).replace(".", ","), "118446744073709551616.000000000",
        `${timestamp(0n)}\n`, {}, new Date(0)]) {
        await invalid({ ...rawRow(), [key]: value });
      }
    }
  });
  await test("prepare/get/finalize identity, per-statement counts and finalizer priority", async () => {
    const prepare = new Error("prepare sentinel"), get = new Error("get sentinel");
    const finalize = new Error("finalize sentinel");
    const beforePrepare = facade(rawRow(), { prepare });
    await rejects(() => read(beforePrepare.connection, node, hash), prepare);
    equal(beforePrepare.owned, []);
    equal(beforePrepare.sql.length, 1);
    for (const value of [null, rawRow(), undefined]) {
      for (const faults of [{ get }, { finalize }, { get, finalize }]) {
        const seam = facade(value, faults);
        await rejects(() => read(seam.connection, node, hash), faults.finalize ?? faults.get);
        finished(seam);
      }
    }
    equal(await project(null), undefined);
    await invalid(undefined);
    equal(values(), initial);
  });
  await test("finalized before absence, projection, errors or publication; no clocks", async () => {
    const projection = new Error("projection sentinel");
    const row = rawRow(), seam = facade(row);
    Object.defineProperty(row, "node_id", {
      enumerable: true, get() { ok(seam.owned[0].closed); throw projection; },
    });
    await rejects(() => read(seam.connection, node, hash), projection);
    finished(seam);
    const valid = rawRow(), projected = facade(valid);
    for (const key of columns) {
      const value = valid[key];
      Object.defineProperty(valid, key, {
        enumerable: true, get() { ok(projected.owned[0].closed); return value; },
      });
    }
    equal(await read(projected.connection, node, hash), expected);
    finished(projected);
    const absent = facade(null);
    equal(await read(absent.connection, node, hash), undefined);
    finished(absent);
    const OriginalDate = Date;
    globalThis.Date = new Proxy(OriginalDate, {
      construct(target, args) {
        equal(args.length, 1);
        return Reflect.construct(target, args, target);
      },
      get(target, property, receiver) {
        if (property === "now") return () => assert.fail("no clock reads");
        return Reflect.get(target, property, receiver);
      },
    });
    try { equal(await read(db, node, hash), expected); }
    finally { globalThis.Date = OriginalDate; }
  });
  await test("native PK, FK, STRICT types and CHECKs reject without weakening DDL", async () => {
    nativeReject(`INSERT INTO main.node_snapshot_acks SELECT * FROM main.node_snapshot_acks WHERE content_hash=?`, hash);
    for (const key of ["node_id", "revision_id"]) {
      nativeReject(`UPDATE main.node_snapshot_acks SET ${key}=? WHERE content_hash=?`, missing, hash);
    }
    for (const [key, value] of [
      ["node_id", "bad"], ["revision_id", revision.toUpperCase()], ["content_hash", "bad\0text"],
      ["snapshot_version", 2147483648], ["replication_version", -2147483649],
      ["snapshot_version", 1.5], ["replication_version", "not-integer"],
      ["leadership_generation", "not-integer"], ["leadership_generation", 1.5],
      ["applied_at", "malformed"], ["received_at", "malformed"],
      ["applied_at", "118446744073709551616.000000000"], ["received_at", "0\0"],
      ["content_hash", new Uint8Array([1])],
    ]) {
      nativeReject(`UPDATE main.node_snapshot_acks SET ${key}=? WHERE content_hash=?`, value, hash);
      await invalid({ ...rawRow(), [key as string]: value });
    }
    equal(await read(db, node, hash), expected);
  });
  await test("caller cache, pending transaction and savepoint rollback preserved", async () => {
    const cached = db.query("SELECT snapshot_version FROM main.node_snapshot_acks WHERE node_id=? AND content_hash=?");
    db.exec("BEGIN");
    try {
      run(db, "UPDATE main.node_snapshot_acks SET snapshot_version=7 WHERE content_hash=?", hash);
      equal((await read(db, node, hash))!.snapshotVersion, 7);
      equal(db.inTransaction, true);
      db.exec("SAVEPOINT caller");
      run(db, "UPDATE main.node_snapshot_acks SET replication_version=8 WHERE content_hash=?", hash);
      equal((await read(db, node, hash))!.replicationVersion, 8);
      equal(await read(db, missing, hash), undefined);
      equal(db.inTransaction, true);
      equal(db.query("SELECT snapshot_version FROM main.node_snapshot_acks WHERE node_id=? AND content_hash=?"), cached);
      equal(cached.get(node, hash), { snapshot_version: 7n });
      db.exec("ROLLBACK TO caller; RELEASE caller");
      equal((await read(db, node, hash))!.replicationVersion, expected.replicationVersion);
      equal((await read(db, node, hash))!.snapshotVersion, 7);
    } finally { db.exec("ROLLBACK"); }
    equal(values(), initial);
    equal(cached.get(node, hash), { snapshot_version: -2147483648n });
    cached.finalize();
  });
  await test("real native get error preserves caller transaction and savepoint", async () => {
    db.exec("BEGIN; SAVEPOINT native_error");
    try {
      run(db, "UPDATE main.node_snapshot_acks SET snapshot_version=9 WHERE content_hash=?", hash);
      let finalizes = 0;
      const connection = {
        prepare(query: string) {
          const statement = db.prepare(query);
          return {
            get() { return statement.get(node, hash, hash); }, // Native excess-binding error.
            finalize() { finalizes++; statement.finalize(); },
          };
        },
      } as unknown as Database;
      await rejects(() => read(connection, node, hash));
      equal(finalizes, 1);
      equal(db.inTransaction, true);
      equal((await read(db, node, hash))!.snapshotVersion, 9);
      db.exec("ROLLBACK TO native_error; RELEASE native_error");
      equal(await read(db, node, hash), expected);
    } finally { db.exec("ROLLBACK"); }
    equal(values(), initial);
  });
  await test("TEMP shadow cannot intercept MAIN", async () => {
    db.exec("CREATE TEMP TABLE node_snapshot_acks(node_id TEXT,content_hash TEXT); INSERT INTO temp.node_snapshot_acks VALUES('shadow','shadow')");
    try {
      equal(await read(db, node, hash), expected);
      equal(await read(db, "shadow", "shadow"), undefined);
    } finally { db.exec("DROP TABLE temp.node_snapshot_acks"); }
    equal(values(), initial);
  });
  await test("external commit visible, no result cache, and reopened connection", async () => {
    const other = open();
    try {
      other.exec("BEGIN");
      run(other, "UPDATE main.node_snapshot_acks SET snapshot_version=11 WHERE content_hash=?", hash);
      equal(await read(db, node, hash), expected);
      other.exec("COMMIT");
      equal((await read(db, node, hash))!.snapshotVersion, 11);
      const reopened = open();
      try { equal((await read(reopened, node, hash))!.snapshotVersion, 11); }
      finally { reopened.close(); }
    } finally {
      if (other.inTransaction) other.exec("ROLLBACK");
      run(other, "UPDATE main.node_snapshot_acks SET snapshot_version=? WHERE content_hash=?", expected.snapshotVersion, hash);
      other.close();
    }
    equal(values(), initial);
  });
  equal(values(), initial);
} finally { db.close(); }
const reopened = open();
try {
  equal(await read(reopened, node, hash), expected);
  equal(values(reopened), initial);
} finally { reopened.close(); }
console.log(`snapshot-acknowledgement-read checks=${checks} scenarios=${scenarios}`);
