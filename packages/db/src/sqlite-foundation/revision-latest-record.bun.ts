import assert from "node:assert/strict";
import { Database, type Statement } from "bun:sqlite";
import { openSQLite } from "./open.js";
import { encodeTimestamp } from "./timestamp.js";
import { getSQLiteRevisionRecord } from "./revision-record.js";
import { latestSQLiteRevisionRecord, type SQLiteLatestRevisionRecord } from "./revision-latest-record.js";
import type { RevisionRecord, RevisionStore } from "../ports.js";

type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2) ? true : false
    : false;
type Expect<T extends true> = T;
type CanonicalRecord = Expect<Identical<SQLiteLatestRevisionRecord, RevisionRecord>>;
type CanonicalReturn = Expect<Identical<ReturnType<typeof latestSQLiteRevisionRecord>, ReturnType<RevisionStore["latest"]>>>;
type OnlyConnection = Expect<Identical<Parameters<typeof latestSQLiteRevisionRecord>, [db: Database]>>;

const [path, originalID] = process.argv.slice(2);
const db = openSQLite(path);
const id = (n: number) => `bbbbbbbb-bbbb-bbbb-bbbb-${String(n).padStart(12, "0")}`;
const stamp = (seconds: bigint, nanos = 0) => encodeTimestamp({ kind: "finite", seconds, nanos })!;
const columns = ["id", "revision_number", "checksum", "snapshot", "actor_user_id", "created_at", "applied_at",
  "source", "source_primary_id", "source_node_id", "source_revision_id", "snapshot_content_hash",
  "snapshot_version", "replication_version", "leadership_generation"];
const sql = `SELECT ${columns.join(", ")} FROM main.config_revisions ORDER BY revision_number DESC LIMIT 1`;
// Public fields are independent of the SQL column list and checked explicitly.
const nullable = ["actorUserId", "sourcePrimaryId", "sourceNodeId", "sourceRevisionId", "snapshotContentHash",
  "snapshotVersion", "replicationVersion", "leadershipGeneration", "appliedAt"] as const;
const publicKeys = ["id", "revisionNumber", "checksum", "snapshot", "source", "createdAt", ...nullable];
let cases = 0;
function rows(connection = db) { return connection.query("SELECT * FROM main.config_revisions ORDER BY id").all(); }
async function check(name: string, body: () => void | Promise<void>) {
  await body(); cases++; console.log(`PASS ${name}`);
}
async function test(name: string, body: () => void | Promise<void>) {
  await check(name, async () => {
    const before = rows(); db.exec("BEGIN");
    try { await body(); assert.equal(db.inTransaction, true); }
    finally { if (db.inTransaction) db.exec("ROLLBACK"); }
    assert.deepEqual(rows(), before);
  });
}
function insert(number = 2n, n = 1, created = stamp(0n)) {
  db.query(`INSERT INTO main.config_revisions(id,revision_number,checksum,snapshot,created_at)
    VALUES(?,? ,?,'null',?)`).run(id(n), number, `checksum-${n}`, created);
}
function update(column: string, value: string | bigint | null, key = id(1)) {
  // Only fixture-controlled column names, never public inputs.
  db.query(`UPDATE main.config_revisions SET ${column}=? WHERE id=?`).run(value, key);
}
async function read(connection = db) {
  const before = rows(connection), changes = connection.query("SELECT total_changes() AS n").get();
  const inTransaction = connection.inTransaction;
  const promise = latestSQLiteRevisionRecord(connection);
  assert.ok(promise instanceof Promise);
  const record = await promise;
  assert.deepEqual(rows(connection), before);
  assert.deepEqual(connection.query("SELECT total_changes() AS n").get(), changes);
  assert.equal(connection.inTransaction, inTransaction);
  if (record) assert.deepEqual(Object.keys(record).sort(), [...publicKeys].sort());
  return record;
}
function driver(raw: unknown, errors: { prepare?: Error; get?: Error; finalize?: Error } = {}) {
  const events: string[] = [];
  const fake = {
    prepare(query: string) {
      events.push("prepare"); assert.equal(query, sql);
      if (errors.prepare) throw errors.prepare;
      return {
        get(...args: unknown[]) {
          events.push("get"); assert.deepEqual(args, []);
          if (errors.get) throw errors.get;
          return raw;
        },
        finalize() { events.push("finalize"); if (errors.finalize) throw errors.finalize; },
      };
    },
    query() { assert.fail("must not use caller prepare cache"); },
    exec() { assert.fail("must not own a transaction or write"); },
  } as unknown as Database;
  return { run: () => latestSQLiteRevisionRecord(fake), events };
}
// Observations wrap this connection's own statement, never global prototypes.
function observed(connection: Database, onRow: (raw: unknown) => unknown, onFinalize: (statement: Statement) => void) {
  return new Proxy(connection, { get(target, property) {
    if (property === "prepare") return (query: string) => {
      assert.equal(query, sql);
      const statement = target.prepare(query);
      return {
        get() { return onRow(statement.get()); },
        finalize() { statement.finalize(); onFinalize(statement); },
      };
    };
    const value = Reflect.get(target, property);
    return typeof value === "function" ? value.bind(target) : value;
  } });
}
try {
  const original = db.query<Record<string, unknown>, [string]>("SELECT * FROM main.config_revisions WHERE id=?").get(originalID)!;
  const originalBytes = rows();
  await check("Go-committed-reused-revision-independent-oracle", async () => {
    const record = (await read())!;
    assert.equal(record.id, originalID); assert.equal(record.revisionNumber, 1);
    assert.equal(record.source, "ordinary"); assert.equal(record.checksum, original.checksum);
    assert.deepEqual(record.snapshot, JSON.parse(original.snapshot as string));
    assert.equal(record.createdAt.toISOString(), "1970-01-01T00:02:03.000Z");
    for (const key of nullable) { assert.ok(Object.hasOwn(record, key)); assert.equal(record[key], undefined); }
    assert.deepEqual(record, getSQLiteRevisionRecord(db, originalID));
  });
  await test("real-empty-table-native-NULL-only", async () => {
    // Pending deletion is rolled back before deferred foreign keys are checked.
    db.exec("PRAGMA defer_foreign_keys = ON; DELETE FROM main.config_revisions");
    assert.equal(await read(), undefined);
  });
  for (const source of ["ordinary", "import", "sync"] as const) {
    await test(`source-no-hidden-filter-${source}`, async () => {
      insert(); update("source", source); assert.equal((await read())!.source, source);
    });
  }
  await test("all-15-public-fields-independent-expectation-and-get-regression", async () => {
    insert();
    db.query(`INSERT INTO users(id,username,password_hash,role,created_at,updated_at)
      VALUES(?,'latest-fixture','opaque','owner','0','2')`).run(id(9));
    for (const column of ["actor_user_id", "source_primary_id", "source_node_id", "source_revision_id"]) update(column, id(9));
    update("source", "sync"); update("snapshot_content_hash", " 雪🦊\n ");
    update("snapshot_version", -2147483648n); update("replication_version", 2147483647n);
    update("leadership_generation", 9007199254740993n); update("applied_at", stamp(-1n, 999999999));
    update("snapshot", ' {"items":[false,"",null],"id":"not UUID"} ');
    const expected = {
      id: id(1), revisionNumber: 2, checksum: "checksum-1", snapshot: { items: [false, "", null], id: "not UUID" },
      actorUserId: id(9), source: "sync", sourcePrimaryId: id(9), sourceNodeId: id(9), sourceRevisionId: id(9),
      snapshotContentHash: " 雪🦊\n ", snapshotVersion: -2147483648, replicationVersion: 2147483647,
      leadershipGeneration: 9007199254740992, createdAt: new Date(0), appliedAt: new Date(-1),
    };
    assert.deepEqual(await read(), expected); assert.deepEqual(getSQLiteRevisionRecord(db, id(1)), expected);
  });
  await test("native-revision-order-not-timestamps-ID-insertion-or-source", async () => {
    insert(7n, 8, "0"); update("source", "sync", id(8)); update("applied_at", "0", id(8));
    insert(3n, 9, "2"); update("source", "import", id(9)); update("applied_at", "2", id(9));
    insert(5n, 7, stamp(9223372036854775807n));
    assert.equal((await read())!.id, id(8));
  });
  for (const value of [-2147483648n, -1n, 0n, 2147483647n]) {
    await test(`revision-int32-order-${value}`, async () => {
      update("revision_number", -2147483648n, originalID);
      if (value === -2147483648n) assert.equal((await read())!.revisionNumber, Number(value));
      else { insert(value); assert.equal((await read())!.revisionNumber, Number(value)); }
    });
  }
  await test("unique-native-revision-index-enforces-ties", async () => {
    insert(); assert.throws(() => insert(2n, 2), /UNIQUE constraint failed: config_revisions.revision_number/);
    assert.equal((await read())!.id, id(1));
  });
  // Out-of-domain int64 revisions are possible only in deliberately corrupted
  // fixtures. Observe native selection BEFORE int32 validation rejects them.
  for (const [low, high] of [[9007199254740992n, 9007199254740993n],
    [-9007199254740993n, -9007199254740992n], [-9223372036854775808n, 9223372036854775807n]]) {
    await test(`native-int64-selection-before-public-validation-${high}`, async () => {
      db.exec("PRAGMA ignore_check_constraints = ON");
      try {
        update("revision_number", low, originalID); insert(high);
        let selected: unknown;
        const connection = observed(db, (raw) => { selected = raw; return raw; }, () => {});
        await assert.rejects(latestSQLiteRevisionRecord(connection), /Invalid INTEGER revision_number/);
        assert.equal((selected as Record<string, unknown>).revision_number, high);
        assert.equal((selected as Record<string, unknown>).id, id(1));
        if (high === 9007199254740993n || high < 0n) assert.equal(Number(low), Number(high));
        // Reverse physical insertion order while preserving the same numbers.
        db.query("DELETE FROM main.config_revisions WHERE id=?").run(id(1));
        update("revision_number", high, originalID); insert(low);
        await assert.rejects(latestSQLiteRevisionRecord(connection), /Invalid INTEGER revision_number/);
        assert.equal((selected as Record<string, unknown>).revision_number, high);
        assert.equal((selected as Record<string, unknown>).id, originalID);
      } finally { db.exec("PRAGMA ignore_check_constraints = OFF"); }
    });
  }
  for (const [raw, expected] of [["null", null], ["0", 0], ["false", false], ['""', ""], ["42", 42],
    ["true", true], ['"scalar"', "scalar"], ["[]", []], ["{}", {}],
    [' [null,0,false,"",{"snow":"雪🦊"}] ', [null, 0, false, "", { snow: "雪🦊" }]],
    [' \n{"big":922337203685477580812345} \t', { big: 9.223372036854776e23 }], ["1e400", Infinity]] as [string, unknown][]) {
    await test(`generic-JSON-${raw}`, async () => {
      insert(); update("snapshot", raw); assert.deepEqual((await read())!.snapshot, expected);
      assert.equal(db.query<{ snapshot: string }, [string]>("SELECT snapshot FROM config_revisions WHERE id=?").get(id(1))!.snapshot, raw);
    });
  }
  await test("nullable-TEXT-falsy-values-not-truthiness", async () => {
    insert();
    for (const raw of [null, "", " untouched 雪🦊\n "]) {
      update("snapshot_content_hash", raw); assert.equal((await read())!.snapshotContentHash, raw ?? undefined);
    }
    update("checksum", ""); assert.equal((await read())!.checksum, "");
  });
  for (const [column, key] of [["snapshot_version", "snapshotVersion"], ["replication_version", "replicationVersion"]] as const) {
    for (const raw of [null, -2147483648n, 0n, 2147483647n]) {
      await test(`int32-${column}-${raw}`, async () => {
        insert(); update(column, raw); assert.equal((await read())![key], raw === null ? undefined : Number(raw));
      });
    }
  }
  for (const [raw, expected] of [[null, undefined], [0n, 0], [-9223372036854775808n, -9223372036854775808],
    [9223372036854775807n, 9223372036854775808], [9007199254740993n, 9007199254740992],
    [-9007199254740993n, -9007199254740992]] as [bigint | null, number | undefined][]) {
    await test(`leadership-int64-public-rounding-${raw}`, async () => {
      insert(); update("leadership_generation", raw); assert.equal((await read())!.leadershipGeneration, expected);
    });
  }
  const clocks: [string, number][] = [
    [stamp(0n), 0], [stamp(0n, 1), 0], [stamp(0n, 999999), 0], [stamp(0n, 1000000), 1],
    [stamp(-1n, 999999999), -1], [stamp(-1n, 998999999), -2], [stamp(-1n, 1), -1000],
    [stamp(1n, 999999999), 1999], [stamp(8640000000000n), 8640000000000000],
    [stamp(8640000000000n, 999999), 8640000000000000], [stamp(8639999999999n, 999999999), 8639999999999999],
    [stamp(8640000000000n, 1000000), NaN], [stamp(-8640000000000n), -8640000000000000],
    [stamp(-8640000000000n, 1000000), -8639999999999999], [stamp(-8640000000001n, 999999999), NaN],
    [stamp(-9223372036854775808n, 999999999), NaN], [stamp(9223372036854775807n), NaN], ["0", NaN], ["2", NaN],
  ];
  for (const [column, key] of [["created_at", "createdAt"], ["applied_at", "appliedAt"]] as const) {
    for (const [raw, expected] of clocks) {
      await test(`clock-${column}-${raw}`, async () => {
        insert(); update(column, raw); const result = (await read())![key]!;
        assert.ok(result instanceof Date); assert.equal(result.getTime(), expected);
        if (Number.isNaN(expected)) assert.throws(() => result.toISOString(), RangeError);
        assert.equal(db.query<Record<string, unknown>, [string]>("SELECT * FROM config_revisions WHERE id=?").get(id(1))![column], raw);
      });
    }
  }
  await test("fresh-mutable-JSON-Date-and-records", async () => {
    insert(); update("snapshot", '{"items":[{"n":1}]}'); update("applied_at", stamp(1n));
    const bytes = rows(), first = (await read())!;
    first.createdAt.setTime(12); first.appliedAt!.setTime(34); first.source = "import";
    (first.snapshot as unknown as { items: { n: number }[] }).items[0].n = 99;
    const next = (await read())!;
    assert.equal(next.createdAt.getTime(), 0); assert.equal(next.appliedAt!.getTime(), 1000);
    assert.equal(next.source, "ordinary"); assert.deepEqual(next.snapshot, { items: [{ n: 1 }] });
    assert.notStrictEqual(first, next); assert.notStrictEqual(first.snapshot, next.snapshot);
    assert.notStrictEqual(first.createdAt, next.createdAt); assert.notStrictEqual(first.appliedAt, next.appliedAt);
    assert.deepEqual(rows(), bytes);
  });
  await test("main-qualified-TEMP-shadow", async () => {
    db.exec("CREATE TEMP TABLE config_revisions(id TEXT)");
    try { assert.equal((await read())!.id, originalID); } finally { db.exec("DROP TABLE temp.config_revisions"); }
  });
  await test("caller-savepoints-pending-insert-update-delete-and-external-invisibility", async () => {
    const other = openSQLite(path);
    try {
      db.exec("SAVEPOINT latest_fixture"); insert();
      assert.equal((await read())!.id, id(1)); assert.equal((await read(other))!.id, originalID);
      update("revision_number", 3n, originalID);
      assert.equal((await read())!.id, originalID); assert.equal((await read(other))!.revisionNumber, 1);
      db.exec("PRAGMA defer_foreign_keys = ON; SAVEPOINT latest_delete");
      db.query("DELETE FROM main.config_revisions WHERE id=?").run(originalID);
      assert.equal((await read())!.id, id(1)); assert.equal((await read(other))!.id, originalID);
      db.exec("ROLLBACK TO latest_delete; RELEASE latest_delete");
      assert.equal((await read())!.revisionNumber, 3);
      db.exec("DELETE FROM main.config_revisions WHERE revision_number=2");
      assert.equal((await read())!.revisionNumber, 3);
      db.exec("SAVEPOINT latest_nested"); insert(4n, 2);
      assert.equal((await read())!.id, id(2)); db.exec("ROLLBACK TO latest_nested; RELEASE latest_nested");
      assert.equal((await read())!.revisionNumber, 3);
      db.exec("ROLLBACK TO latest_fixture; RELEASE latest_fixture");
      assert.deepEqual(rows(), originalBytes); assert.equal((await read())!.revisionNumber, 1);
    } finally { other.close(); }
  });
  await check("uncached-owned-cursor-finalized-before-real-projection-and-publication", async () => {
    const cached = db.query(sql); const cursors: Statement[] = []; let finalized = false;
    const connection = observed(db, (raw) => new Proxy(raw as object, {
      ownKeys(target) {
        assert.equal(finalized, true, "column validation before finalize");
        return Reflect.ownKeys(target);
      },
      get(target, property) {
        assert.equal(finalized, true, `projection accessed ${String(property)} before finalize`);
        return Reflect.get(target, property);
      },
    }), (statement) => {
      cursors.push(statement); assert.throws(() => statement.get()); finalized = true;
    });
    assert.equal((await latestSQLiteRevisionRecord(connection))!.id, originalID);
    finalized = false;
    assert.equal((await latestSQLiteRevisionRecord(connection))!.id, originalID);
    assert.equal(cursors.length, 2); assert.notStrictEqual(cursors[0], cursors[1]);
    assert.deepEqual(cached.get(), original); assert.strictEqual(db.query(sql), cached);
  });
  const invalid: [string, unknown][] = [["undefined-row", undefined], ["array", []], ["scalar", 1], ["extra", { ...original, extra: 1 }]];
  for (const column of columns) {
    const missing = { ...original }; delete missing[column]; invalid.push([`missing-${column}`, missing]);
    invalid.push([`undefined-${column}`, { ...original, [column]: undefined }]);
  }
  for (const column of ["id", "checksum", "snapshot", "actor_user_id", "created_at", "applied_at", "source",
    "source_primary_id", "source_node_id", "source_revision_id", "snapshot_content_hash"]) {
    for (const value of [0, 1n, false, {}, "bad\0text"]) invalid.push([`TEXT-${column}-${String(value)}`, { ...original, [column]: value }]);
  }
  for (const column of ["id", "checksum", "snapshot", "created_at", "source", "revision_number"]) {
    invalid.push([`required-null-${column}`, { ...original, [column]: null }]);
  }
  for (const column of ["id", "actor_user_id", "source_primary_id", "source_node_id", "source_revision_id"]) {
    for (const value of ["", "not UUID", id(1).toUpperCase(), `{${id(1)}}`, id(1).replaceAll("-", ""), `${id(1)}\n`]) {
      invalid.push([`UUID-${column}-${value}`, { ...original, [column]: value }]);
    }
  }
  for (const column of ["revision_number", "snapshot_version", "replication_version", "leadership_generation"]) {
    for (const value of [0, 1.5, NaN, "1", true, 9223372036854775808n, -9223372036854775809n]) {
      invalid.push([`INTEGER-${column}-${String(value)}`, { ...original, [column]: value }]);
    }
    if (column !== "leadership_generation") for (const value of [2147483648n, -2147483649n]) {
      invalid.push([`int32-${column}-${value}`, { ...original, [column]: value }]);
    }
  }
  for (const value of ["", "ordinary ", "invalid"]) invalid.push([`source-${value}`, { ...original, source: value }]);
  for (const value of ["{", "undefined", "", "[1,]"]) invalid.push([`JSON-${value}`, { ...original, snapshot: value }]);
  for (const column of ["created_at", "applied_at"]) {
    for (const value of ["3", "1", "100000000000000000000.1000000000", "118446744073709551616.000000000", "100000000000000000000.00000000x"]) {
      invalid.push([`tag-${column}-${value}`, { ...original, [column]: value }]);
    }
  }
  for (const [name, value] of invalid) await check(`invalid-selected-${name}`, async () => {
    const probe = driver(value); await assert.rejects(probe.run());
    assert.deepEqual(probe.events, ["prepare", "get", "finalize"]);
  });
  await check("empty-native-NULL-finalized", async () => {
    const probe = driver(null); assert.equal(await probe.run(), undefined);
    assert.deepEqual(probe.events, ["prepare", "get", "finalize"]);
  });
  for (const phase of ["prepare", "get", "finalize"] as const) {
    await check(`Promise-rejection-preserves-${phase}-identity`, async () => {
      const sentinel = new Error(phase), probe = driver(original, { [phase]: sentinel });
      let promise: Promise<RevisionRecord | undefined> | undefined;
      assert.doesNotThrow(() => { promise = probe.run(); }); assert.ok(promise instanceof Promise);
      await assert.rejects(promise!, (error) => error === sentinel);
      assert.deepEqual(probe.events, phase === "prepare" ? ["prepare"] : ["prepare", "get", "finalize"]);
    });
  }
  await check("finalizer-error-takes-precedence-on-empty-malformed-and-valid", async () => {
    for (const raw of [null, { ...original, snapshot: "{" }, original]) {
      const sentinel = new Error("finalize"), probe = driver(raw, { finalize: sentinel });
      await assert.rejects(probe.run(), (error) => error === sentinel);
      assert.deepEqual(probe.events, ["prepare", "get", "finalize"]);
    }
  });
  await check("fetch-and-finalize-failure-finalizer-precedence", async () => {
    const fetch = new Error("fetch"), finalize = new Error("finalize");
    const probe = driver(original, { get: fetch, finalize });
    await assert.rejects(probe.run(), (error) => error === finalize);
    assert.deepEqual(probe.events, ["prepare", "get", "finalize"]);
  });
  await check("projection-sentinel-after-actual-finalize-no-result-or-cache-damage", async () => {
    const sentinel = new Error("projection"), cached = db.query(sql); let finalized = false;
    const connection = observed(db, (raw) => new Proxy(raw as object, { get() {
      assert.equal(finalized, true); throw sentinel;
    } }), (statement) => { assert.throws(() => statement.get()); finalized = true; });
    await assert.rejects(latestSQLiteRevisionRecord(connection), (error) => error === sentinel);
    assert.deepEqual(cached.get(), original); assert.deepEqual(rows(), originalBytes);
  });
  await check("finalize-sentinel-after-actual-native-finalization", async () => {
    const sentinel = new Error("native-finalized-then-fault");
    const connection = observed(db, (raw) => raw, (statement) => {
      assert.throws(() => statement.get()); throw sentinel;
    });
    await assert.rejects(latestSQLiteRevisionRecord(connection), (error) => error === sentinel);
    assert.equal((await read())!.id, originalID);
  });
  await test("malformed-unselected-older-row-irrelevant-selected-rejects", async () => {
    insert(); db.exec("PRAGMA ignore_check_constraints = ON");
    try {
      update("snapshot", "{", originalID);
      assert.equal((await read())!.id, id(1));
      await assert.rejects(Promise.resolve().then(() => getSQLiteRevisionRecord(db, originalID)));
      update("snapshot", "{", id(1)); const before = rows();
      await assert.rejects(latestSQLiteRevisionRecord(db)); assert.deepEqual(rows(), before);
    } finally { db.exec("PRAGMA ignore_check_constraints = OFF"); }
  });
  await test("real-selected-corruption-error-and-recovery", async () => {
    insert(); db.exec("PRAGMA ignore_check_constraints = ON");
    try {
      for (const [column, value] of [["source", "invalid"], ["snapshot", "{"], ["created_at", "bad-tag"],
        ["snapshot_version", 2147483648n]] as [string, string | bigint][]) {
        const previous = db.query<Record<string, string | bigint | null>, [string]>("SELECT * FROM config_revisions WHERE id=?").get(id(1))![column];
        update(column, value); const before = rows();
        await assert.rejects(latestSQLiteRevisionRecord(db)); assert.deepEqual(rows(), before);
        update(column, previous); assert.equal((await read())!.id, id(1));
      }
    } finally { db.exec("PRAGMA ignore_check_constraints = OFF"); }
  });
  await test("real-prepare-error-not-absence-and-reuse", async () => {
    db.exec("ALTER TABLE main.config_revisions RENAME TO latest_hidden");
    await assert.rejects(latestSQLiteRevisionRecord(db));
    db.exec("ALTER TABLE main.latest_hidden RENAME TO config_revisions");
    assert.equal((await read())!.id, originalID);
  });
  await check("closed-connection-Promise-rejects", async () => {
    const closed = openSQLite(path); closed.close();
    let promise: Promise<RevisionRecord | undefined> | undefined;
    assert.doesNotThrow(() => { promise = latestSQLiteRevisionRecord(closed); });
    await assert.rejects(promise!);
  });
  await check("committed-reopen-native-preservation-and-unfiltered-latest", async () => {
    db.exec("BEGIN IMMEDIATE");
    try {
      insert(2n, 1, stamp(-1n, 999999999)); update("source", "sync");
      update("snapshot", ' \n{"items":[0,false,"",null]} \t'); update("applied_at", "2");
      update("leadership_generation", 9223372036854775807n); db.exec("COMMIT");
      const before = rows(), reopened = openSQLite(path);
      try {
        const first = (await read(reopened))!;
        assert.equal(first.id, id(1)); assert.equal(first.source, "sync"); assert.equal(first.createdAt.getTime(), -1);
        assert.ok(Number.isNaN(first.appliedAt!.getTime())); assert.equal(first.leadershipGeneration, 9223372036854775808);
        assert.deepEqual(first.snapshot, { items: [0, false, "", null] });
        first.createdAt.setTime(0); (first.snapshot as unknown as { items: unknown[] }).items.push("mutated");
        assert.equal((await read(reopened))!.createdAt.getTime(), -1);
        assert.deepEqual((await read(reopened))!.snapshot, { items: [0, false, "", null] });
        assert.deepEqual(rows(reopened), before);
      } finally { reopened.close(); }
    } finally {
      if (db.inTransaction) db.exec("ROLLBACK");
      db.query("DELETE FROM main.config_revisions WHERE id=?").run(id(1));
    }
  });
  assert.deepEqual(rows(), originalBytes); assert.equal(db.inTransaction, false);
  console.log(`COMPLETE Bun latest revision cases: ${cases}`);
} finally { if (db.inTransaction) db.exec("ROLLBACK"); db.close(); }
