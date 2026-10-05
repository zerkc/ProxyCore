import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import { getSQLiteAppliedSnapshotRecord } from "./applied-snapshot-read-record.js";
import type { SQLiteAppliedSnapshotRecord } from "./applied-snapshot-read-record.js";
import type { AppliedSnapshotRecord, ContinuityTransactionPort } from "../ports.js";

const read: (db: Database, id: string) => ReturnType<ContinuityTransactionPort["getAppliedSnapshot"]>
  = getSQLiteAppliedSnapshotRecord;
type Equal<A, B> = (<T>() => T extends A ? 1 : 2) extends
  (<T>() => T extends B ? 1 : 2) ? true : false;
const linked: Equal<SQLiteAppliedSnapshotRecord, AppliedSnapshotRecord> = true;
const promised: Equal<ReturnType<typeof read>, Promise<AppliedSnapshotRecord | undefined>> = true;

const path = process.argv[2];
function open() {
  const connection = new Database(path, { strict: true, safeIntegers: true });
  connection.exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=0");
  return connection;
}
const db = open();
const id = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const node = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
const revision = "cccccccc-cccc-cccc-cccc-cccccccccccc";
const job = "dddddddd-dddd-dddd-dddd-dddddddddddd";
const missing = "ffffffff-ffff-ffff-ffff-ffffffffffff";
const columns = ["id", "source_primary_id", "leadership_generation", "snapshot_version",
  "replication_version", "content_hash", "snapshot_body", "revision_id", "status",
  "apply_job_id", "failure_code", "applied_at", "discarded_at"];
const fields = ["id", "sourcePrimaryId", "leadershipGeneration", "snapshotVersion",
  "replicationVersion", "contentHash", "snapshotBody", "revisionId", "status",
  "applyJobId", "failureCode", "appliedAt", "discardedAt"];
const uuidColumns = ["id", "source_primary_id", "revision_id", "apply_job_id"];
const nullable = ["snapshot_body", "revision_id", "apply_job_id", "failure_code", "discarded_at"];
const statuses = ["pending", "applied", "rejected", "rolled-back", "archived"] as const;
const expected: AppliedSnapshotRecord = {
  id, sourcePrimaryId: node, leadershipGeneration: Number(9007199254740993n),
  snapshotVersion: -2147483648, replicationVersion: 2147483647,
  contentHash: "  hash 雪 🦊  ", snapshotBody: new Uint8Array([0, 255, 128, 65]),
  revisionId: revision, status: "pending", applyJobId: job, failureCode: "  result 雪  ",
  appliedAt: new Date(-1), discardedAt: new Date(1),
};
function rawRow(): Record<string, unknown> {
  return {
    id, source_primary_id: node, leadership_generation: 9007199254740993n,
    snapshot_version: -2147483648n, replication_version: 2147483647n,
    content_hash: "  hash 雪 🦊  ", snapshot_body: new Uint8Array([0, 255, 128, 65]),
    revision_id: revision, status: "pending", apply_job_id: job, failure_code: "  result 雪  ",
    applied_at: "109223372036854775807.999000000",
    discarded_at: "109223372036854775808.001000000",
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
  const statement = connection.prepare(`SELECT ${columns.join(", ")} FROM main.applied_snapshots ORDER BY id`);
  try { return statement.values(); } finally { statement.finalize(); }
}
const initial = values();
function run(connection: Database, sql: string, ...bindings: any[]) {
  const statement = connection.prepare(sql);
  try { statement.run(...bindings); } finally { statement.finalize(); }
}

type Owned = { gets: number; finalizes: number; args: unknown[]; closed: boolean };
type Faults = { prepare?: Error; get?: Error; finalize?: Error };
// No default row: explicitly undefined must remain malformed, not become valid.
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
    query() { assert.fail("no caller cached statements"); },
    exec() { assert.fail("no write or owned transaction SQL"); },
    close() { assert.fail("caller owns connection"); },
    transaction() { assert.fail("caller owns transaction"); },
    get inTransaction() { return assert.fail("no transaction guard"); },
  } as unknown as Database;
  return { connection, owned, sql };
}
function finished(seam: ReturnType<typeof facade>, lookup = id) {
  equal(seam.owned.length, 1);
  equal(seam.owned[0], { gets: 1, finalizes: 1, args: [lookup], closed: true });
}
async function project(row: unknown) {
  const seam = facade(row);
  const result = await read(seam.connection, id);
  finished(seam);
  return result;
}
async function invalid(row: unknown, wanted?: RegExp) {
  const seam = facade(row);
  await rejects(() => read(seam.connection, id), wanted);
  finished(seam);
}
function timestamp(seconds: bigint, nanos = 0) {
  return `1${(seconds + (1n << 63n)).toString().padStart(20, "0")}.${nanos.toString().padStart(9, "0")}`;
}
async function change(key: string, value: unknown, body: () => Promise<void>) {
  ok(columns.includes(key));
  db.exec("SAVEPOINT reader_case");
  try {
    const statement = db.prepare(`UPDATE main.applied_snapshots SET ${key} = ? WHERE id = ?`);
    try { statement.run(value as any, id); } finally { statement.finalize(); }
    await body();
  } finally {
    db.exec("ROLLBACK TO reader_case; RELEASE reader_case");
  }
  equal(values(), initial);
}

try {
  await test("all thirteen canonical fields, Promise and type alias", async () => {
    equal(linked, true);
    equal(promised, true);
    equal(initial.length, 3);
    const record = await read(db, id);
    equal(record, expected);
    equal(Object.keys(record!).sort(), [...fields].sort());
    const again = await read(db, id);
    equal(again, expected);
    ok(again !== record);
    ok(again!.appliedAt !== record!.appliedAt);
    ok(again!.discardedAt !== record!.discardedAt);
    ok(again!.snapshotBody !== record!.snapshotBody);
    record!.snapshotBody![0] = 42;
    equal((await read(db, id))!.snapshotBody, expected.snapshotBody);
    equal(values(), initial);
  });
  await test("Go-seeded empty and NULL bodies retain canonical optional own keys", async () => {
    for (const [lookup, contentHash, snapshotBody] of [
      ["eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", "empty-body", new Uint8Array()],
      ["11111111-1111-1111-1111-111111111111", "null-body", undefined],
    ] as const) {
      const record = await read(db, lookup);
      equal(record, {
        id: lookup, sourcePrimaryId: node, leadershipGeneration: 0, snapshotVersion: 0,
        replicationVersion: 0, contentHash, snapshotBody, revisionId: undefined,
        status: "applied", applyJobId: undefined, failureCode: undefined,
        appliedAt: new Date(-1), discardedAt: undefined,
      });
      equal(Object.keys(record!).sort(), [...fields].sort());
      if (snapshotBody) ok(record!.snapshotBody !== snapshotBody);
    }
    equal(values(), initial);
  });
  await test("explicit MAIN SELECT and one bound literal id", async () => {
    const seam = facade(undefined, {}, true);
    equal(await read(seam.connection, id), expected);
    equal(seam.sql, [`SELECT ${columns.join(", ")} FROM main.applied_snapshots WHERE id = ? LIMIT 1`]);
    finished(seam);
    const lookups = [missing, node, revision, job, "", "not-a-uuid", id.toUpperCase(),
      `{${id}}`, id.replaceAll("-", ""), ` ${id}`, `${id} `, `${id}\n`, `${id}\0`,
      "' OR 1=1 --", `${id}' UNION SELECT * FROM applied_snapshots --`];
    for (const lookup of lookups) {
      equal(await read(db, lookup), undefined);
      const absent = facade(undefined, {}, true);
      equal(await read(absent.connection, lookup), undefined);
      finished(absent, lookup);
      equal(absent.sql, seam.sql);
    }
    const bad = facade(rawRow());
    for (const lookup of [null, undefined, 1, {}, new String(id)]) {
      await rejects(() => read(bad.connection, lookup as string), /must be a string/);
    }
    equal(bad.sql, []);
    equal(values(), initial);
  });
  await test("all nullable keys present undefined, native SQL NULL", async () => {
    for (const key of nullable) {
      const index = columns.indexOf(key);
      const row = { ...rawRow(), [key]: null };
      const wanted = { ...expected, [fields[index]]: undefined };
      equal(await project(row), wanted);
      await change(key, null, async () => {
        const record = await read(db, id);
        equal(record, wanted);
        ok(Object.hasOwn(record!, fields[index]));
      });
    }
    const row = rawRow();
    for (const key of nullable) row[key] = null;
    const record = await project(row);
    for (const key of nullable) {
      const property = fields[columns.indexOf(key)];
      ok(Object.hasOwn(record!, property));
      equal(record![property as keyof AppliedSnapshotRecord], undefined);
    }
  });
  await test("NULL, empty and binary BLOB copy independence", async () => {
    for (const bytes of [new Uint8Array(), new Uint8Array([0, 255, 128, 65]),
      new Uint8Array([255, 254, 192, 128, 0])]) {
      const backing = new Uint8Array(bytes.length + 2);
      backing.set(bytes, 1);
      const source = backing.subarray(1, bytes.length + 1);
      const row = { ...rawRow(), snapshot_body: source };
      const first = (await project(row))!.snapshotBody!;
      const second = (await project(row))!.snapshotBody!;
      equal(first, bytes);
      ok(first !== source && first !== second);
      ok(first.buffer !== source.buffer && first.buffer !== second.buffer);
      if (first.length) {
        first[0] ^= 255;
        equal(source, bytes);
        equal(second, bytes);
        source[0] ^= 127;
        equal(second, bytes);
      }
      await change("snapshot_body", bytes, async () => {
        const record = await read(db, id);
        equal(record!.snapshotBody, bytes);
        ok(record!.snapshotBody instanceof Uint8Array);
        ok(record!.snapshotBody !== bytes);
      });
    }
    for (const value of [undefined, "", "00FF", [], {}, new ArrayBuffer(0), new Int8Array(0), 0]) {
      await invalid({ ...rawRow(), snapshot_body: value }, /BLOB/);
    }
  });
  await test("all statuses and exact TEXT preservation", async () => {
    for (const status of statuses) {
      equal((await project({ ...rawRow(), status }))!.status, status);
      await change("status", status, async () => equal((await read(db, id))!.status, status));
    }
    for (const key of ["content_hash", "failure_code"]) {
      for (const value of ["", "  whitespace 雪 🦊 \n  ", "null", "{not JSON}"]) {
        const property = fields[columns.indexOf(key)] as keyof AppliedSnapshotRecord;
        equal((await project({ ...rawRow(), [key]: value }))![property], value);
        await change(key, value, async () => equal((await read(db, id))![property], value));
      }
    }
    for (const status of ["", "APPLIED", "unknown", "applied\n", "applied\0"]) {
      await invalid({ ...rawRow(), status }, /status|TEXT/);
    }
  });
  await test("stored UUID, TEXT and row shape validation", async () => {
    for (const value of [undefined, false, 0, "", [], {}]) await invalid(value);
    const extra = { ...rawRow(), unexpected: true };
    await invalid(extra, /columns/);
    for (const key of columns) {
      const removed = rawRow();
      delete removed[key];
      await invalid(removed, /columns/);
      await invalid({ ...rawRow(), [key]: undefined });
      if (!nullable.includes(key)) await invalid({ ...rawRow(), [key]: null });
    }
    for (const key of uuidColumns) {
      for (const value of [node.toUpperCase(), `{${node}}`, node.replaceAll("-", ""),
        node.slice(0, -1), `${node}x`, `${node}\n`, "g".repeat(36), "", 1, {}, `${node}\0`]) {
        await invalid({ ...rawRow(), [key]: value }, /UUID|TEXT/);
      }
    }
    for (const key of ["content_hash", "failure_code", "status"]) {
      for (const value of [1, 1n, false, {}, [], new Uint8Array(), "bad\0text"]) {
        await invalid({ ...rawRow(), [key]: value }, /TEXT/);
      }
    }
  });
  await test("native int32 bounds and int64 rounded Number projection", async () => {
    for (const key of ["snapshot_version", "replication_version"]) {
      const property = fields[columns.indexOf(key)] as keyof AppliedSnapshotRecord;
      for (const value of [-2147483648n, -1n, 0n, 1n, 2147483647n]) {
        equal((await project({ ...rawRow(), [key]: value }))![property], Number(value));
        await change(key, value, async () => equal((await read(db, id))![property], Number(value)));
      }
      for (const value of [-2147483649n, 2147483648n, 0, 1.5, NaN, Infinity, "1", true, {}]) {
        await invalid({ ...rawRow(), [key]: value }, /INTEGER/);
      }
    }
    for (const value of [-(1n << 63n), -9007199254740993n, -1n, 0n,
      9007199254740993n, (1n << 63n) - 1n]) {
      equal((await project({ ...rawRow(), leadership_generation: value }))!.leadershipGeneration, Number(value));
      await change("leadership_generation", value, async () => {
        equal((await read(db, id))!.leadershipGeneration, Number(value));
      });
    }
    for (const value of [-(1n << 63n) - 1n, 1n << 63n, 0, 1.5, NaN, Infinity, "1", false]) {
      await invalid({ ...rawRow(), leadership_generation: value }, /INTEGER/);
    }
  });
  await test("public millisecond timestamps, infinities and Date boundaries", async () => {
    const cases: [string, number][] = [
      [timestamp(-1n, 999999999), -1], [timestamp(0n, 999999), 0],
      [timestamp(0n, 1000000), 1], [timestamp(0n, 999999999), 999],
      [timestamp(-8640000000000n), -8640000000000000],
      [timestamp(8640000000000n), 8640000000000000],
      [timestamp(-8640000000001n, 999999999), NaN],
      [timestamp(8640000000000n, 1000000), NaN],
      [timestamp(-(1n << 63n)), NaN], [timestamp((1n << 63n) - 1n), NaN],
      ["0", NaN], ["2", NaN],
    ];
    for (const key of ["applied_at", "discarded_at"]) {
      const property = key === "applied_at" ? "appliedAt" : "discardedAt";
      for (const [value, milliseconds] of cases) {
        equal((await project({ ...rawRow(), [key]: value }))![property]!.getTime(), milliseconds);
        await change(key, value, async () => {
          equal((await read(db, id))![property]!.getTime(), milliseconds);
        });
      }
      for (const value of ["", "1", "3", "2025-01-01", " 0", "2\n", "0\0", 0, 1n,
        timestamp(0n).replace(".", ","), "118446744073709551616.000000000",
        `${timestamp(0n)}\n`, {}, new Date(0)]) {
        await invalid({ ...rawRow(), [key]: value });
      }
    }
  });
  await test("owned statement fault identity, counts and finalizer priority", async () => {
    const prepare = new Error("prepare sentinel"), get = new Error("get sentinel");
    const finalize = new Error("finalize sentinel");
    const beforePrepare = facade(rawRow(), { prepare });
    await rejects(() => read(beforePrepare.connection, id), prepare);
    equal(beforePrepare.owned, []);
    equal(beforePrepare.sql.length, 1);
    for (const value of [null, rawRow(), undefined]) {
      for (const faults of [{ get }, { finalize }, { get, finalize }]) {
        const seam = facade(value, faults);
        await rejects(() => read(seam.connection, id), faults.finalize ?? faults.get);
        finished(seam);
      }
    }
    equal(await project(null), undefined);
    for (const value of [undefined, { ...rawRow(), status: "broken" }]) await invalid(value);
    equal(values(), initial);
  });
  await test("finalization precedes projection, absence and publication", async () => {
    const projection = new Error("projection sentinel");
    const row = rawRow();
    const seam = facade(row);
    Object.defineProperty(row, "status", {
      enumerable: true, get() { ok(seam.owned[0].closed); throw projection; },
    });
    await rejects(() => read(seam.connection, id), projection);
    finished(seam);
    const valid = rawRow(), projected = facade(valid);
    for (const key of columns) {
      const value = valid[key];
      Object.defineProperty(valid, key, {
        enumerable: true, get() { ok(projected.owned[0].closed); return value; },
      });
    }
    equal(await read(projected.connection, id), expected);
    finished(projected);
    const absent = facade(null);
    equal(await read(absent.connection, id), undefined);
    finished(absent);
  });
  await test("caller cached statement, pending transaction and savepoint rollback", async () => {
    const cached = db.query("SELECT content_hash FROM main.applied_snapshots WHERE id = ?");
    db.exec("BEGIN");
    try {
      run(db, "UPDATE main.applied_snapshots SET failure_code='pending caller' WHERE id=?", id);
      equal((await read(db, id))!.failureCode, "pending caller");
      equal(db.inTransaction, true);
      db.exec("SAVEPOINT caller");
      run(db, "UPDATE main.applied_snapshots SET content_hash='caller savepoint' WHERE id=?", id);
      equal((await read(db, id))!.contentHash, "caller savepoint");
      equal(await read(db, missing), undefined);
      equal(db.inTransaction, true);
      equal(db.query("SELECT content_hash FROM main.applied_snapshots WHERE id = ?"), cached);
      equal(cached.get(id), { content_hash: "caller savepoint" });
      db.exec("ROLLBACK TO caller; RELEASE caller");
      equal((await read(db, id))!.contentHash, expected.contentHash);
      equal((await read(db, id))!.failureCode, "pending caller");
    } finally { db.exec("ROLLBACK"); }
    equal(values(), initial);
    equal(cached.get(id), { content_hash: expected.contentHash });
    cached.finalize();
  });
  await test("real native query error preserves caller savepoint", async () => {
    db.exec("BEGIN; SAVEPOINT native_error");
    try {
      run(db, "UPDATE main.applied_snapshots SET failure_code='native pending' WHERE id=?", id);
      let finalizes = 0;
      const connection = {
        prepare(query: string) {
          const statement = db.prepare(query);
          return {
            get() { return statement.get(id, id); }, // Strict native excess binding error.
            finalize() { finalizes++; statement.finalize(); },
          };
        },
      } as unknown as Database;
      await rejects(() => read(connection, id));
      equal(finalizes, 1);
      equal(db.inTransaction, true);
      equal((await read(db, id))!.failureCode, "native pending");
      db.exec("ROLLBACK TO native_error; RELEASE native_error");
      equal(await read(db, id), expected);
    } finally { db.exec("ROLLBACK"); }
    equal(values(), initial);
  });
  await test("TEMP shadow cannot intercept MAIN and migrated malformed CHECK remains", async () => {
    db.exec("CREATE TEMP TABLE applied_snapshots(id TEXT); INSERT INTO temp.applied_snapshots VALUES('shadow')");
    try {
      equal(await read(db, id), expected);
      equal(await read(db, "shadow"), undefined);
    } finally { db.exec("DROP TABLE temp.applied_snapshots"); }
    for (const [key, value] of [["status", "broken"], ["applied_at", "malformed"],
      ["discarded_at", "malformed"], ["snapshot_version", 2147483648]]) {
      db.exec("SAVEPOINT native_constraint");
      try {
        const statement = db.prepare(`UPDATE main.applied_snapshots SET ${key}=? WHERE id=?`);
        try { assert.throws(() => statement.run(value as any, id)); checks++; }
        finally { statement.finalize(); }
        equal(await read(db, id), expected);
      } finally { db.exec("ROLLBACK TO native_constraint; RELEASE native_constraint"); }
    }
    equal(values(), initial);
  });
  await test("external commit visible, never cached, and reopened connection", async () => {
    const other = open();
    try {
      other.exec("BEGIN");
      run(other, "UPDATE main.applied_snapshots SET failure_code='external committed' WHERE id=?", id);
      equal(await read(db, id), expected);
      other.exec("COMMIT");
      equal((await read(db, id))!.failureCode, "external committed");
      const reopened = open();
      try { equal((await read(reopened, id))!.failureCode, "external committed"); }
      finally { reopened.close(); }
    } finally {
      if (other.inTransaction) other.exec("ROLLBACK");
      run(other, "UPDATE main.applied_snapshots SET failure_code=? WHERE id=?", expected.failureCode!, id);
      other.close();
    }
    equal(values(), initial);
  });
  equal(values(), initial);
} finally {
  db.close();
}
const reopened = open();
try {
  equal(await read(reopened, id), expected);
  equal(values(reopened), initial);
} finally { reopened.close(); }
console.log(`applied-snapshot-read checks=${checks} scenarios=${scenarios}`);
