import assert from "node:assert/strict";
import { Database, type Statement } from "bun:sqlite";
import type { JobRecord, JobStore } from "../ports.js";
import { openSQLite } from "./open.js";
import { encodeTimestamp } from "./timestamp.js";
import { listSQLiteJobRecords } from "./job-list-record.js";

type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2)
      ? true : false
    : false;
type Expect<T extends true> = T;
type CanonicalParameters = Expect<Identical<Parameters<typeof listSQLiteJobRecords>, [db: Database]>>;
type CanonicalReturn = Expect<Identical<ReturnType<typeof listSQLiteJobRecords>, ReturnType<JobStore["list"]>>>;

const [path, originalID] = process.argv.slice(2);
const db = openSQLite(path);
const id = (n: number) => `bbbbbbbb-bbbb-bbbb-bbbb-${String(n).padStart(12, "0")}`;
const stamp = (seconds: bigint, nanos = 0) => encodeTimestamp({ kind: "finite", seconds, nanos })!;
const outputs = ["validation_output", "apply_output", "health_output"] as const;
const outputKeys = ["validationOutput", "applyOutput", "healthOutput"] as const;
const clocks = ["created_at", "claimed_at", "started_at", "finished_at"] as const;
const clockKeys = ["createdAt", "claimedAt", "startedAt", "finishedAt"] as const;
const optional = ["actorUserId", "sourcePrimaryId", "sourceNodeId", "sourceRevisionId",
  "snapshotContentHash", "snapshotVersion", "replicationVersion", "leadershipGeneration",
  "claimedAt", "startedAt", "finishedAt", ...outputKeys, "errorMessage"];
const keys = ["id", "revisionId", "target", "status", "source", "correlationId", "createdAt", ...optional];
// Independent explicit SQL golden: no implementation constants or reader reuse.
const sql = "SELECT id, revision_id, correlation_id, created_at, target, status, source, "
  + "actor_user_id, source_primary_id, source_node_id, source_revision_id, snapshot_content_hash, "
  + "validation_output, apply_output, health_output, error_message, claimed_at, started_at, finished_at, "
  + "snapshot_version, replication_version, leadership_generation FROM main.apply_jobs ORDER BY created_at DESC";
let cases = 0;
function rows(connection = db) {
  const statement = connection.prepare<Record<string, unknown>, []>("SELECT * FROM main.apply_jobs ORDER BY id");
  try { return statement.all(); }
  finally { statement.finalize(); }
}
const originalRows = rows();
const original = originalRows[0];
const revision = original.revision_id as string;
function insert(n = 1, created = stamp(0n), target = "combined", status = "queued", source = "ordinary") {
  db.query(`INSERT INTO main.apply_jobs(id,revision_id,target,status,source,correlation_id,created_at)
    VALUES(?,?,?,?,?,'free text NOT UUID',?)`).run(id(n), revision, target, status, source, created);
}
function update(column: string, value: string | bigint | null, n = 1) {
  db.query(`UPDATE main.apply_jobs SET ${column}=? WHERE id=?`).run(value, id(n));
}
async function list(connection = db) {
  const promise = listSQLiteJobRecords(connection);
  assert(promise instanceof Promise);
  const result = await promise;
  for (const record of result) assert.deepEqual(Object.keys(record).sort(), [...keys].sort());
  return result;
}
async function check(name: string, body: () => void | Promise<void>) {
  await body(); cases++; console.log(`PASS ${name}`);
}
async function test(name: string, body: () => void | Promise<void>, seed = true) {
  await check(name, async () => {
    const before = rows();
    const revisions = db.query("SELECT * FROM main.config_revisions ORDER BY id").all();
    db.exec("BEGIN");
    try {
      db.exec("DELETE FROM main.apply_jobs");
      if (seed) insert();
      await body();
      assert.equal(db.inTransaction, true, "list does not finish the caller transaction");
    } finally { if (db.inTransaction) db.exec("ROLLBACK"); }
    assert.deepEqual(rows(), before);
    assert.deepEqual(db.query("SELECT * FROM main.config_revisions ORDER BY id").all(), revisions);
  });
}
async function unchanged(connection = db) {
  const before = rows();
  const changes = db.query("SELECT total_changes() AS n").get();
  const result = await list(connection);
  assert.deepEqual(rows(), before);
  assert.deepEqual(db.query("SELECT total_changes() AS n").get(), changes);
  return result;
}
// Local facade around a real statement. No global monkeypatches, ID reads,
// transaction methods or cached query method are available to the implementation.
function facade(options: { transform?: (values: Record<string, unknown>[]) => unknown[];
  prepareError?: Error; allError?: Error; finalizeError?: Error } = {}) {
  const state = { prepares: 0, all: 0, completions: 0, finalizations: 0 };
  const owned: Statement[] = [];
  const connection = {
    prepare(actual: string) {
      state.prepares++;
      assert.equal(actual, sql);
      if (options.prepareError) throw options.prepareError;
      const statement = db.prepare<Record<string, unknown>, []>(actual);
      owned.push(statement);
      return {
        all(...bindings: unknown[]) {
          state.all++;
          assert.equal(bindings.length, 0);
          if (options.allError) throw options.allError;
          const values = statement.all();
          state.completions++;
          return options.transform ? options.transform(values) : values;
        },
        finalize() {
          state.finalizations++;
          statement.finalize();
          if (options.finalizeError) throw options.finalizeError;
        },
      };
    },
  } as unknown as Database;
  return { connection, state, owned };
}
try {
  await check("Go-enqueue-independent-projection-golden-native-bytes", async () => {
    const records = await unchanged();
    assert.deepEqual(records, [{
      id: originalID, revisionId: revision, actorUserId: undefined, target: "combined",
      status: "queued", source: "ordinary", sourcePrimaryId: undefined, sourceNodeId: undefined,
      sourceRevisionId: undefined, snapshotContentHash: undefined, snapshotVersion: undefined,
      replicationVersion: undefined, leadershipGeneration: undefined, correlationId: original.correlation_id,
      createdAt: new Date(123000), claimedAt: undefined, startedAt: undefined, finishedAt: undefined,
      validationOutput: undefined, applyOutput: undefined, healthOutput: undefined, errorMessage: undefined,
    }]);
    assert.equal(records[0].createdAt.toISOString(), "1970-01-01T00:02:03.000Z");
  });
  await test("empty-is-empty-array-with-one-completed-finalized-query", async () => {
    const seam = facade();
    assert.deepEqual(await unchanged(seam.connection), []);
    assert.deepEqual(seam.state, { prepares: 1, all: 1, completions: 1, finalizations: 1 });
    assert.throws(() => seam.owned[0].all());
  }, false);
  await test("all-target-status-source-combinations-no-filters", async () => {
    const expected: [string, string, string, string][] = [];
    let n = 0;
    for (const target of ["coredns", "nginx", "combined", "certificate"]) {
      for (const status of ["queued", "validating", "applying", "applied", "failed", "rolled-back"]) {
        for (const source of ["ordinary", "import", "sync"]) {
          n++; insert(n, stamp(BigInt(n)), target, status, source);
          expected.unshift([id(n), target, status, source]);
        }
      }
    }
    assert.deepEqual((await unchanged()).map((r) => [r.id, r.target, r.status, r.source]), expected);
  }, false);
  await test("unbounded-complete-list-over-common-1000-cap", async () => {
    for (let n = 1; n <= 1101; n++) insert(n, stamp(BigInt(n)));
    const result = await unchanged();
    assert.equal(result.length, 1101);
    assert.deepEqual(result.map((r) => r.id), Array.from({ length: 1101 }, (_, i) => id(1101 - i)));
  }, false);
  await test("extra-JS-arguments-unused-not-inspected", async () => {
    const poison = new Proxy({}, { get() { assert.fail("extra argument inspected"); }, ownKeys() { assert.fail("extra argument enumerated"); } });
    const extra = listSQLiteJobRecords as unknown as (db: Database, ...ignored: unknown[]) => Promise<JobRecord[]>;
    assert.equal((await extra(db, poison, 0, 1, "failed"))[0].id, id(1));
  });
  // Explicit chronological sequence, NOT sorting with the production codec or
  // comparing lossy Dates. Distinct nanos share a millisecond; ties are sets.
  const ordering: [string, number][] = [
    ["2", NaN], [stamp(9223372036854775807n, 999999999), NaN],
    [stamp(8640000000000n, 1000000), NaN], [stamp(8640000000000n, 999999), 8640000000000000],
    [stamp(8640000000000n), 8640000000000000], [stamp(1n), 1000],
    [stamp(0n, 999999999), 999], [stamp(0n, 1000000), 1],
    [stamp(0n, 999999), 0], [stamp(0n, 1), 0], [stamp(0n), 0],
    [stamp(-1n, 999999999), -1], [stamp(-1n, 999000000), -1],
    [stamp(-1n, 998999999), -2], [stamp(-1n, 1), -1000], [stamp(-1n), -1000],
    [stamp(-8640000000000n), -8640000000000000], [stamp(-8640000000001n, 999999999), NaN],
    [stamp(-9223372036854775808n), NaN], ["0", NaN],
  ];
  await test("native-DESC-before-lossy-Date-nanos-infinities-negative-Date-boundaries", async () => {
    // Reverse insertion/ID order rules out an accidental ID-desc test oracle.
    for (let i = ordering.length - 1; i >= 0; i--) insert(i + 1, ordering[i][0]);
    const result = await unchanged();
    assert.deepEqual(result.map((r) => r.id), ordering.map((_, i) => id(i + 1)));
    for (let i = 0; i < result.length; i++) assert.equal(result[i].createdAt.getTime(), ordering[i][1]);
  }, false);
  await test("timestamp-ties-unordered-no-ID-tiebreak-in-SQL", async () => {
    insert(3); insert(2); insert(4, stamp(1n));
    const seam = facade();
    const result = await unchanged(seam.connection);
    assert.equal(result[0].id, id(4));
    assert.deepEqual(new Set(result.slice(1).map((r) => r.id)), new Set([id(1), id(2), id(3)]));
  });
  await test("main-qualified-TEMP-shadow-excluded", async () => {
    db.exec("CREATE TEMP TABLE apply_jobs(id TEXT); INSERT INTO temp.apply_jobs VALUES('shadow')");
    try { assert.deepEqual((await unchanged()).map((r) => r.id), [id(1)]); }
    finally { db.exec("DROP TABLE temp.apply_jobs"); }
  });
  await test("22-own-keys-SQL-NULL-undefined", async () => {
    const record = (await unchanged())[0];
    assert.equal(Object.keys(record).length, 22);
    for (const key of optional) {
      assert(Object.hasOwn(record, key));
      assert.equal(record[key as keyof JobRecord], undefined);
    }
  });
  await test("all-populated-independent-public-golden", async () => {
    db.query(`INSERT INTO main.users(id,username,password_hash,role,created_at,updated_at)
      VALUES(?,'job-list-fixture','opaque','owner','0','2')`).run(id(9));
    for (const column of ["actor_user_id", "source_primary_id", "source_node_id", "source_revision_id"]) update(column, id(9));
    update("snapshot_content_hash", "雪🦊"); update("error_message", "");
    update("snapshot_version", -2147483648n); update("replication_version", 2147483647n);
    update("leadership_generation", 9007199254740993n);
    update("claimed_at", stamp(-1n, 999999999)); update("started_at", stamp(0n, 1000000)); update("finished_at", stamp(86400n));
    update("validation_output", ' {"ok":false,"n":0,"s":""} ');
    update("apply_output", '[0,false,"",null]'); update("health_output", '"healthy"');
    assert.deepEqual(await unchanged(), [{
      id: id(1), revisionId: revision, actorUserId: id(9), target: "combined", status: "queued",
      source: "ordinary", sourcePrimaryId: id(9), sourceNodeId: id(9), sourceRevisionId: id(9),
      snapshotContentHash: "雪🦊", snapshotVersion: -2147483648, replicationVersion: 2147483647,
      leadershipGeneration: 9007199254740992, correlationId: "free text NOT UUID",
      createdAt: new Date(0), claimedAt: new Date(-1), startedAt: new Date(1), finishedAt: new Date(86400000),
      validationOutput: { ok: false, n: 0, s: "" }, applyOutput: [0, false, "", null],
      healthOutput: "healthy", errorMessage: "",
    }]);
  });
  for (let i = 0; i < outputs.length; i++) {
    for (const [raw, expected] of [[null, undefined], ["null", undefined], ["0", 0], ["false", false],
      ['""', ""], [' [null,0,false,"",{"snow":"雪🦊"}] ', [null, 0, false, "", { snow: "雪🦊" }]],
      [' {"big":922337203685477580812345} ', { big: 9.223372036854776e23 }], ["1e400", Infinity]] as [string | null, unknown][]) {
      await test(`JSON-${outputs[i]}-${raw}`, async () => {
        update(outputs[i], raw);
        assert.deepEqual((await unchanged())[0][outputKeys[i]], expected);
        assert.equal(rows()[0][outputs[i]], raw);
      });
    }
  }
  for (const [column, key] of [["snapshot_content_hash", "snapshotContentHash"], ["error_message", "errorMessage"]] as const) {
    for (const raw of [null, "", "  untouched 雪🦊\n "]) {
      await test(`nullable-text-${column}-${raw}`, async () => {
        update(column, raw); assert.equal((await unchanged())[0][key], raw ?? undefined);
      });
    }
  }
  for (const [column, key] of [["snapshot_version", "snapshotVersion"], ["replication_version", "replicationVersion"],
    ["leadership_generation", "leadershipGeneration"]] as const) {
    const goldens: [bigint | null, number | undefined][] = [[null, undefined], [0n, 0], [-2147483648n, -2147483648], [2147483647n, 2147483647]];
    if (column === "leadership_generation") goldens.push(
      [-9223372036854775808n, -9223372036854775808], [9223372036854775807n, 9223372036854775808],
      [9007199254740993n, 9007199254740992], [-9007199254740993n, -9007199254740992]);
    for (const [raw, expected] of goldens) {
      await test(`integer-${column}-${raw}`, async () => {
        update(column, raw); assert.equal((await unchanged())[0][key], expected);
        assert.equal(rows()[0][column], raw);
      });
    }
  }
  const dateGoldens: [string, number, string | undefined][] = [
    [stamp(0n, 999999), 0, "1970-01-01T00:00:00.000Z"],
    [stamp(0n, 1000000), 1, "1970-01-01T00:00:00.001Z"],
    [stamp(-1n, 999999999), -1, "1969-12-31T23:59:59.999Z"],
    [stamp(-1n, 998999999), -2, "1969-12-31T23:59:59.998Z"],
    [stamp(8640000000000n), 8640000000000000, "+275760-09-13T00:00:00.000Z"],
    [stamp(-8640000000000n), -8640000000000000, "-271821-04-20T00:00:00.000Z"],
    [stamp(8640000000000n, 1000000), NaN, undefined],
    [stamp(-8640000000001n, 999999999), NaN, undefined], ["0", NaN, undefined], ["2", NaN, undefined],
  ];
  for (let i = 0; i < clocks.length; i++) {
    for (const [raw, expected, iso] of dateGoldens) {
      await test(`date-${clocks[i]}-${raw}`, async () => {
        update(clocks[i], raw);
        const value = (await unchanged())[0][clockKeys[i]]!;
        assert(value instanceof Date); assert.equal(value.getTime(), expected);
        if (iso) assert.equal(value.toISOString(), iso);
        else assert.throws(() => value.toISOString(), RangeError);
        assert.equal(rows()[0][clocks[i]], raw);
      });
    }
  }
  await test("isolated-record-Date-JSON-mutations-and-fresh-lists", async () => {
    insert(2, stamp(1n));
    for (const n of [1, 2]) {
      for (const column of outputs) update(column, '{"list":[{"n":1}]}', n);
      for (const column of clocks.slice(1)) update(column, stamp(1n), n);
    }
    const before = rows();
    const first = await unchanged();
    for (const key of clockKeys) first[0][key]!.setTime(456);
    for (const key of outputKeys) (first[0][key] as { list: { n: number }[] }).list[0].n = 99;
    first[0].status = "failed";
    for (const key of outputKeys) assert.deepEqual(first[1][key], { list: [{ n: 1 }] });
    assert.equal(first[1].createdAt.getTime(), 0);
    const next = await unchanged();
    assert.equal(next[0].status, "queued"); assert.equal(next[0].createdAt.getTime(), 1000);
    for (const key of clockKeys) assert.notStrictEqual(first[0][key], next[0][key]);
    for (const key of outputKeys) {
      assert.deepEqual(next[0][key], { list: [{ n: 1 }] }); assert.notStrictEqual(first[0][key], next[0][key]);
    }
    assert.deepEqual(rows(), before);
  });
  await test("one-query-drain-finalize-before-every-row-validation-projection", async () => {
    insert(2, stamp(1n));
    const seam = facade({ transform(values) {
      return values.map((value) => ({ ...value, get validation_output() {
        assert.equal(seam.state.completions, 1); assert.equal(seam.state.finalizations, 1);
        return value.validation_output;
      } }));
    } });
    assert.deepEqual((await unchanged(seam.connection)).map((r) => r.id), [id(2), id(1)]);
    assert.deepEqual(seam.state, { prepares: 1, all: 1, completions: 1, finalizations: 1 });
    assert.throws(() => seam.owned[0].all());
  });
  const invalid: [string, unknown][] = [
    ["null-row", null], ["array-row", []], ["missing-column", { ...original, id: undefined }],
    ["extra-column", { ...original, extra: 1 }], ["UUID-uppercase", { ...original, id: id(1).toUpperCase() }],
    ["UUID-bad-length", { ...original, source_node_id: "a" }], ["text-NUL", { ...original, correlation_id: "a\0b" }],
    ["required-null", { ...original, created_at: null }], ["nullable-undefined", { ...original, claimed_at: undefined }],
    ["domain-target", { ...original, target: "other" }], ["domain-status", { ...original, status: "cancelled" }],
    ["domain-source", { ...original, source: "other" }], ["JSON", { ...original, health_output: "{" }],
    ["timestamp", { ...original, created_at: "3" }],
    ["timestamp-seconds", { ...original, claimed_at: "118446744073709551616.000000000" }],
    ["timestamp-nanos", { ...original, finished_at: "100000000000000000000.1000000000" }],
    ["numeric-driver", { ...original, leadership_generation: 1 }],
    ["int32-high", { ...original, snapshot_version: 2147483648n }],
    ["int32-low", { ...original, replication_version: -2147483649n }],
    ["int64-high", { ...original, leadership_generation: 9223372036854775808n }],
  ];
  for (const [name, value] of invalid) {
    await check(`malformed-LATER-row-${name}-no-partial-result`, async () => {
      const seam = facade({ transform(values) { return [values[0], value]; } });
      let published: JobRecord[] | undefined;
      await assert.rejects(async () => { published = await list(seam.connection); });
      assert.equal(published, undefined);
      assert.deepEqual(seam.state, { prepares: 1, all: 1, completions: 1, finalizations: 1 });
      assert.throws(() => seam.owned[0].all());
    });
  }
  await check("later-row-access-exception-identity-after-finalize-no-publication", async () => {
    const error = new Error("returned-row projection boundary");
    const seam = facade({ transform(values) { return [values[0], { ...values[0], get validation_output() {
      assert.equal(seam.state.completions, 1); assert.equal(seam.state.finalizations, 1); throw error;
    } }]; } });
    let published: JobRecord[] | undefined;
    await assert.rejects(async () => { published = await list(seam.connection); }, (caught) => caught === error);
    assert.equal(published, undefined);
  });
  await check("local-projection-map-stage-exception-no-partial-publication", async () => {
    const error = new Error("mapping stage sentinel");
    let projected = 0;
    const seam = facade({ transform(values) {
      // Wrap only this returned array's map; the real shared projector still
      // runs. Inject a failure after one valid projection without global hooks.
      const result = [...values];
      result.map = function <U>(callback: (value: Record<string, unknown>, index: number,
        array: Record<string, unknown>[]) => U, thisArg?: unknown): U[] {
        assert.equal(seam.state.completions, 1); assert.equal(seam.state.finalizations, 1);
        callback.call(thisArg, result[0], 0, result); projected++; throw error;
      };
      return result;
    } });
    let published: JobRecord[] | undefined;
    await assert.rejects(async () => { published = await list(seam.connection); }, (caught) => caught === error);
    assert.equal(projected, 1); assert.equal(published, undefined);
  });
  for (const phase of ["prepareError", "allError", "finalizeError"] as const) {
    await check(`exception-identity-${phase}`, async () => {
      const error = new Error(phase);
      const seam = facade({ [phase]: error });
      await assert.rejects(list(seam.connection), (caught) => caught === error);
      assert.deepEqual(seam.state, phase === "prepareError"
        ? { prepares: 1, all: 0, completions: 0, finalizations: 0 }
        : { prepares: 1, all: 1, completions: phase === "allError" ? 0 : 1, finalizations: 1 });
    });
  }
  await test("finalizer-error-empty-and-invalid-before-validation", async () => {
    const error = new Error("finalizer-first");
    for (const values of [[], [null]]) {
      const seam = facade({ transform() { return values; }, finalizeError: error });
      await assert.rejects(list(seam.connection), (caught) => caught === error);
    }
  });
  await test("private-cursor-finalization-caller-cache-preserved", async () => {
    const cached = db.query(sql);
    const cachedBefore = cached.all();
    const first = facade(); const second = facade();
    await list(first.connection); await list(second.connection);
    assert.notStrictEqual(first.owned[0], second.owned[0]);
    for (const statement of [...first.owned, ...second.owned]) assert.throws(() => statement.all());
    assert.strictEqual(db.query(sql), cached); assert.deepEqual(cached.all(), cachedBefore);
  });
  await test("explicit-existing-columns-ignore-unrelated-fixture-column", async () => {
    db.exec("ALTER TABLE main.apply_jobs ADD COLUMN fixture_only TEXT");
    try {
      update("fixture_only", "opaque");
      assert.equal((await unchanged())[0].id, id(1));
      assert.equal(Object.keys((await list())[0]).length, 22);
    } finally { db.exec("ALTER TABLE main.apply_jobs DROP COLUMN fixture_only"); }
  });
  await test("real-query-error-recovery", async () => {
    db.exec("ALTER TABLE main.apply_jobs RENAME TO list_hidden");
    try { await assert.rejects(list()); }
    finally { db.exec("ALTER TABLE main.list_hidden RENAME TO apply_jobs"); }
    assert.equal((await unchanged())[0].id, id(1));
  });
  await test("real-later-validation-error-recovery-by-caller", async () => {
    insert(2, stamp(-1n));
    db.exec("PRAGMA ignore_check_constraints=ON");
    try {
      for (const [column, value, previous] of [["status", "bad", "queued"], ["validation_output", "{", null],
        ["created_at", "0bad", stamp(-1n)]] as const) {
        update(column, value, 2);
        const before = rows(); const seam = facade(); let published: JobRecord[] | undefined;
        await assert.rejects(async () => { published = await list(seam.connection); });
        assert.equal(published, undefined); assert.equal(seam.state.finalizations, 1);
        assert.deepEqual(rows(), before);
        update(column, previous, 2); assert.equal((await unchanged()).length, 2);
      }
    } finally { db.exec("PRAGMA ignore_check_constraints=OFF"); }
  });
  await check("actual-safeIntegers-disabled-rejects-not-empty-or-partial", async () => {
    const unsafe = new Database(path, { strict: true, safeIntegers: false });
    try {
      // Exercise the actual numeric driver on its own pending caller write.
      unsafe.exec("BEGIN");
      unsafe.query("UPDATE main.apply_jobs SET leadership_generation=9223372036854775807 WHERE id=?").run(originalID);
      await assert.rejects(list(unsafe), /integer/);
      assert.equal(unsafe.inTransaction, true);
    } finally { if (unsafe.inTransaction) unsafe.exec("ROLLBACK"); unsafe.close(); }
    assert.deepEqual(rows(), originalRows);
  });
  await check("pending-caller-transaction-external-invisibility-savepoints-rollback", async () => {
    const other = openSQLite(path);
    db.exec("BEGIN IMMEDIATE");
    try {
      insert(1, stamp(999n));
      assert.deepEqual((await unchanged()).map((r) => r.id), [id(1), originalID]);
      assert.deepEqual((await list(other)).map((r) => r.id), [originalID]);
      db.exec("SAVEPOINT caller_list"); insert(2, stamp(1000n));
      assert.deepEqual((await unchanged()).map((r) => r.id), [id(2), id(1), originalID]);
      db.exec("ROLLBACK TO caller_list; RELEASE caller_list");
      assert.deepEqual((await unchanged()).map((r) => r.id), [id(1), originalID]);
      assert.equal(db.inTransaction, true);
    } finally { if (db.inTransaction) db.exec("ROLLBACK"); other.close(); }
    assert.deepEqual(rows(), originalRows);
  });
  await check("closed-database-error-reopen-native-job-and-revision-bytes", async () => {
    const closed = openSQLite(path); closed.close(); await assert.rejects(list(closed));
    const reopened = openSQLite(path);
    try {
      assert.deepEqual(rows(reopened), originalRows);
      assert.deepEqual(await list(reopened), await list());
      assert.deepEqual(reopened.query("SELECT * FROM main.config_revisions ORDER BY id").all(),
        db.query("SELECT * FROM main.config_revisions ORDER BY id").all());
    } finally { reopened.close(); }
  });
  await check("committed-nondefault-list-reopen-preserves-lossless-bytes", async () => {
    const revisions = db.query("SELECT * FROM main.config_revisions ORDER BY id").all();
    db.exec("BEGIN IMMEDIATE");
    try {
      insert(1, stamp(-1n, 999999999)); insert(2, "2", "certificate", "failed", "sync");
      update("leadership_generation", 9223372036854775807n);
      update("validation_output", ' \n{"items":[0,false,"",null]} \t');
      update("apply_output", "null"); update("health_output", "false");
      update("claimed_at", "0"); update("started_at", "2");
      update("finished_at", stamp(9223372036854775807n, 999999999));
      db.exec("COMMIT");
      const native = rows(); const reopened = openSQLite(path);
      try {
        assert.deepEqual(rows(reopened), native);
        const records = await list(reopened);
        assert.deepEqual(records.map((r) => r.id), [id(2), originalID, id(1)]);
        const record = records[2];
        assert.equal(record.createdAt.toISOString(), "1969-12-31T23:59:59.999Z");
        assert.equal(record.leadershipGeneration, 9223372036854775808);
        for (const key of ["claimedAt", "startedAt", "finishedAt"] as const) assert(Number.isNaN(record[key]!.getTime()));
        assert.deepEqual(record.validationOutput, { items: [0, false, "", null] });
        assert.equal(record.applyOutput, undefined); assert.equal(record.healthOutput, false);
        record.createdAt.setTime(0); (record.validationOutput as { items: unknown[] }).items.push("mutation");
        assert.deepEqual(rows(reopened), native);
        assert.deepEqual(reopened.query("SELECT * FROM main.config_revisions ORDER BY id").all(), revisions);
      } finally { reopened.close(); }
    } finally {
      if (db.inTransaction) db.exec("ROLLBACK");
      db.exec("BEGIN IMMEDIATE");
      try {
        db.query("DELETE FROM main.apply_jobs WHERE id IN (?,?)").run(id(1), id(2));
        db.exec("COMMIT");
      } finally { if (db.inTransaction) db.exec("ROLLBACK"); }
    }
    assert.deepEqual(rows(), originalRows);
  });
  assert.deepEqual(rows(), originalRows); assert.equal(db.inTransaction, false);
  console.log(`TOTAL ${cases} Bun public list cases`);
} finally { if (db.inTransaction) db.exec("ROLLBACK"); db.close(); }
