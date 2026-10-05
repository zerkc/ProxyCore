import assert from "node:assert/strict";
import { Database, type SQLQueryBindings } from "bun:sqlite";
import { createInterface } from "node:readline";
import { openSQLite } from "./open.js";
import { enqueueIfNotActiveSQLiteJobRecord, createInternalSQLiteConditionalJobEnqueuer } from "./job-enqueue-if-not-active-record.js";
import { enqueueSQLiteJobRecord, createInternalSQLiteJobEnqueuer } from "./job-enqueue-record.js";
import { getSQLiteJobRecord, projectSQLiteJobRecord } from "./job-record.js";
import { getSQLiteJobRow } from "./job-read.js";
import type { JobEnqueueInput, JobRecord, JobStore } from "../ports.js";

type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2)
      ? true : false : false;
type Expect<T extends true> = T;
type CanonicalParameters = Expect<Identical<Parameters<typeof enqueueIfNotActiveSQLiteJobRecord>,
  [db: Database, job: JobEnqueueInput]>>;
type CanonicalReturn = Expect<Identical<ReturnType<typeof enqueueIfNotActiveSQLiteJobRecord>,
  ReturnType<JobStore["enqueueIfNotActive"]>>>;

const [mode, path, originalID, revision, extra] = process.argv.slice(2);
const db = openSQLite(path);
const targets = ["coredns", "nginx", "combined", "certificate"] as const;
const active = ["queued", "validating", "applying"] as const;
const terminal = ["applied", "failed", "rolled-back"] as const;
const keys = ["id", "revisionId", "actorUserId", "target", "status", "source", "sourcePrimaryId",
  "sourceNodeId", "sourceRevisionId", "snapshotContentHash", "snapshotVersion", "replicationVersion",
  "leadershipGeneration", "correlationId", "createdAt", "claimedAt", "startedAt", "finishedAt",
  "validationOutput", "applyOutput", "healthOutput", "errorMessage"];
const id = (n: number) => `aaaaaaaa-aaaa-aaaa-aaaa-${String(n).padStart(12, "0")}`;
const input = (): JobEnqueueInput => ({ revisionId: revision, target: "combined", correlationId: extra ?? "conditional" });
let cases = 0;
function rows() { return db.query("SELECT * FROM main.apply_jobs ORDER BY id").all(); }
async function test(name: string, body: () => void | Promise<void>) {
  const before = rows();
  try {
    await body(); assert.equal(db.inTransaction, false);
  } finally {
    if (db.inTransaction) db.exec("ROLLBACK");
    db.query("DELETE FROM main.apply_jobs WHERE id != ?").run(originalID);
  }
  assert.deepEqual(rows(), before);
  cases++; console.log(`PASS ${name}`);
}
async function enqueue(job = input(), connection = db) {
  const pending = enqueueIfNotActiveSQLiteJobRecord(connection, job);
  assert(pending instanceof Promise);
  const result = await pending;
  if (result) assert.deepEqual(Object.keys(result).sort(), [...keys].sort());
  return result;
}
async function seed(status: JobEnqueueInput["status"], target: JobEnqueueInput["target"] = "nginx") {
  return enqueueSQLiteJobRecord(db, { ...input(), status, target });
}

// Test-local facade delegates real owned transactions/statements. Only the guard
// SELECT is instrumented; no global patch, cached cursor or fake COMMIT is used.
function guardFacade(stage?: "prepare" | "step" | "finalize", effect?: () => void,
  insertStage?: "prepare" | "step" | "finalize" | "row") {
  let finalized = 0, stepped = 0, inserts = 0, insertFinalized = 0;
  const sentinel = new Error(`guard ${stage}`);
  const connection = {
    get inTransaction() { return db.inTransaction; },
    transaction: db.transaction.bind(db),
    query: db.query.bind(db),
    prepare(sql: string) {
      if (sql.startsWith("INSERT")) {
        assert.equal(finalized, 1); inserts++;
        if (insertStage === "prepare") throw sentinel;
        const statement = db.prepare<Record<string, unknown>, SQLQueryBindings[]>(sql);
        return {
          all(...bindings: SQLQueryBindings[]) {
            if (insertStage === "step") throw sentinel;
            const result = statement.all(...bindings);
            if (insertStage === "row") result[0].revision_id = "invalid";
            return result;
          },
          finalize() {
            statement.finalize(); insertFinalized++;
            if (insertStage === "finalize") throw sentinel;
          },
        };
      }
      assert(sql.includes("main.apply_jobs") && sql.includes("LIMIT 1"));
      assert(!sql.includes("target") && !sql.includes("revision_id"));
      if (stage === "prepare") throw sentinel;
      const statement = db.prepare<Record<string, unknown>, SQLQueryBindings[]>(sql);
      return {
        get(...bindings: SQLQueryBindings[]) {
          if (stage === "step") throw sentinel;
          const row = statement.get(...bindings); stepped++;
          effect?.(); return row;
        },
        finalize() {
          statement.finalize(); finalized++;
          if (stage === "finalize") throw sentinel;
        },
      };
    },
  } as unknown as Database;
  return { connection, sentinel, finalized: () => finalized, stepped: () => stepped,
    inserts: () => inserts, insertFinalized: () => insertFinalized };
}

if (mode === "child") {
  db.exec("PRAGMA busy_timeout=150");
  const reader = createInterface({ input: process.stdin });
  console.log("READY");
  for await (const command of reader) {
    if (command === "exit") break;
    if (command === "hold") { db.exec("BEGIN IMMEDIATE"); console.log("HELD"); }
    else if (command === "release") { db.exec("ROLLBACK"); console.log("RELEASED"); }
    else if (command === "ordinary" || command === "conditional") {
      const job = { ...input(), target: extra as JobEnqueueInput["target"] };
      try {
        const result = command === "ordinary" ? await enqueueSQLiteJobRecord(db, job) : await enqueue(job);
        console.log(result ? `ENQUEUED ${result.id}` : "EMPTY");
      } catch (error) {
        if (/locked|busy/i.test(String(error))) console.log("BUSY");
        else throw error;
      }
    } else throw new Error(`Unknown command ${command}`);
  }
  reader.close();
  if (db.inTransaction) db.exec("ROLLBACK");
} else if (mode === "unit") {
  db.query("UPDATE main.apply_jobs SET status='applied' WHERE id=?").run(originalID);
  await test("empty active set, default encoding and 22 own keys", async () => {
    const before = Date.now(), result = (await enqueue())!;
    assert(result); assert.equal(result.status, "queued"); assert.equal(result.source, "ordinary");
    assert.equal(result.revisionId, revision); assert.equal(result.correlationId, extra);
    assert.match(result.id, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    assert(result.createdAt.getTime() >= before && result.createdAt.getTime() <= Date.now());
    for (const key of keys.filter((key) => !["id", "revisionId", "target", "status", "source", "correlationId", "createdAt"].includes(key))) {
      assert.equal(result[key as keyof JobRecord], undefined);
    }
    assert.equal(await enqueue(), undefined);
  });
  for (const status of active) {
    for (const existingTarget of targets) {
      for (const requestedTarget of targets) {
        await test(`global guard ${status}/${existingTarget}->${requestedTarget}`, async () => {
          await seed(status, existingTarget);
          const before = rows();
          // Input revision is deliberately unrelated; neither target nor revision
          // may narrow the guard, even for an invalid future insert.
          assert.equal(await enqueue({ ...input(), target: requestedTarget, revisionId: id(99) }), undefined);
          assert.deepEqual(rows(), before);
        });
      }
    }
  }
  await test("multiple active rows do not introduce priority or require uniqueness", async () => {
    for (const status of active) await seed(status);
    assert.equal(await enqueue(), undefined);
  });
  for (const status of terminal) {
    for (const target of targets) {
      await test(`terminal ${status}/${target} permits retry`, async () => {
        await seed(status, target);
        const result = (await enqueue())!;
        assert(result); assert.equal(result.status, "queued");
        db.query("UPDATE main.apply_jobs SET status=? WHERE id=?").run(status, result.id);
        const retry = (await enqueue())!;
        assert(retry && retry.id !== result.id);
      });
    }
  }
  await test("all terminal states together permit reservation", async () => {
    for (const status of terminal) await seed(status);
    assert(await enqueue());
  });
  await test("truly empty table permits first reservation", async () => {
    const original = getSQLiteJobRow(db, originalID)!;
    db.query("DELETE FROM main.apply_jobs WHERE id=?").run(originalID);
    try { assert(await enqueue()); }
    finally {
      const columns = Object.keys(original);
      const statement = db.prepare(`INSERT INTO main.apply_jobs (${columns.join(",")}) VALUES (${columns.map(() => "?").join(",")})`);
      try { statement.run(...Object.values(original) as SQLQueryBindings[]); }
      finally { statement.finalize(); }
    }
  });
  for (const bad of [undefined, null, {}, { claimedAt: new Date(NaN) }, { validationOutput: 1n }]) {
    await test(`blocked invalid input never read/encoded ${String(bad)}`, async () => {
      await seed("queued");
      let reads = 0, hooks = 0, projections = 0;
      const poison = bad && typeof bad === "object" ? new Proxy(bad, { get() { reads++; throw new Error("input read"); } }) : bad;
      const f = guardFacade(), before = rows();
      const run = createInternalSQLiteConditionalJobEnqueuer({
        hook() { hooks++; }, project(row) { projections++; return projectSQLiteJobRecord(row); },
      });
      assert.equal(await run(f.connection, poison as JobEnqueueInput), undefined);
      assert.equal(reads, 0); assert.equal(hooks, 0); assert.equal(projections, 0);
      assert.equal(f.inserts(), 0); assert.equal(f.finalized(), 1); assert.deepEqual(rows(), before);
      // UUID generation is below the guard in the shared insertion helper;
      // the private writer's clock before this guard is explicitly accepted.
    });
  }
  for (const patch of [{ revisionId: id(99) }, { actorUserId: id(98) }, { target: "bad" },
    { status: "bad" }, { source: "bad" }, { correlationId: undefined }, { revisionId: null },
    { validationOutput: 1n }, { claimedAt: new Date(NaN) }, { snapshotVersion: NaN }]) {
    await test(`free invalid input rolls back ${Object.keys(patch)}`, async () => {
      const before = rows();
      await assert.rejects(enqueue({ ...input(), ...patch } as JobEnqueueInput));
      assert.deepEqual(rows(), before); assert(await enqueue());
    });
  }
  for (const nullish of [undefined, null]) {
    await test(`source nullish defaults/optional fields ${nullish}`, async () => {
      const job = { ...input(), status: nullish, source: nullish } as unknown as JobEnqueueInput;
      const optional = keys.filter((key) => !["id", "revisionId", "target", "status", "source", "correlationId", "createdAt"].includes(key));
      for (const key of optional) Object.assign(job, { [key]: nullish });
      const result = (await enqueue(job))!;
      assert.equal(result.status, "queued"); assert.equal(result.source, "ordinary");
      for (const key of optional) assert.equal(result[key as keyof JobRecord], undefined);
    });
  }
  await test("blocked branch does not validate or invoke async internal hook", async () => {
    await seed("validating"); let invoked = false;
    assert.equal(await createInternalSQLiteConditionalJobEnqueuer({
      hook: async () => { invoked = true; },
    })(db, undefined as unknown as JobEnqueueInput), undefined);
    assert.equal(invoked, false);
  });
  await test("source encoders, integer rounding, dates and detached JSON retained", async () => {
    const result = (await enqueue({ ...input(), status: "applied", source: "sync", sourcePrimaryId: id(1),
      sourceNodeId: id(2), sourceRevisionId: id(3), snapshotContentHash: "", errorMessage: "",
      snapshotVersion: -2147483648, replicationVersion: 2147483647,
      leadershipGeneration: 9007199254740993n as unknown as number,
      validationOutput: { nested: [1] }, applyOutput: false, healthOutput: { toJSON() { return null; } },
      claimedAt: new Date(-1), startedAt: new Date(0), finishedAt: new Date(123) }))!;
    assert.equal(result.leadershipGeneration, Number(9007199254740993n));
    assert.equal(result.snapshotVersion, -2147483648); assert.equal(result.replicationVersion, 2147483647);
    assert.equal(result.source, "sync"); assert.equal(result.status, "applied");
    assert.equal(result.sourcePrimaryId, id(1)); assert.equal(result.sourceNodeId, id(2));
    assert.equal(result.sourceRevisionId, id(3)); assert.equal(result.snapshotContentHash, "");
    assert.equal(result.errorMessage, ""); assert.equal(result.applyOutput, false); assert.equal(result.healthOutput, undefined);
    assert.equal(result.claimedAt!.getTime(), -1); assert.equal(result.startedAt!.getTime(), 0);
    assert.equal(result.finishedAt!.getTime(), 123);
    (result.validationOutput as { nested: number[] }).nested.push(2);
    assert.deepEqual(getSQLiteJobRecord(db, result.id)!.validationOutput, { nested: [1] });
    assert.equal(getSQLiteJobRow(db, result.id)!.health_output, "null");
  });
  for (const stage of ["prepare", "step", "finalize"] as const) {
    for (const blocked of [false, true]) {
      await test(`guard ${stage} error, blocked=${blocked}, cursor/cache/recovery`, async () => {
        if (blocked) await seed("applying");
        const cached = db.query("SELECT id FROM main.apply_jobs WHERE id=?");
        const f = guardFacade(stage), before = rows();
        await assert.rejects(enqueue(input(), f.connection), (error: unknown) => error === f.sentinel);
        assert.equal(f.finalized(), stage === "prepare" ? 0 : 1);
        assert.equal(f.stepped(), stage === "finalize" ? 1 : 0);
        assert.equal(f.inserts(), 0); assert.deepEqual(rows(), before);
        assert.equal((cached.get(originalID) as { id: string }).id, originalID);
        if (blocked) assert.equal(await enqueue(), undefined);
        else assert(await enqueue());
      });
    }
  }
  for (const insertStage of ["prepare", "step", "finalize", "row"] as const) {
    await test(`conditional INSERT ${insertStage} rejects/finalizes/recovers`, async () => {
      const before = rows(), f = guardFacade(undefined, undefined, insertStage);
      const cached = db.query("SELECT id FROM main.apply_jobs WHERE id=?");
      await assert.rejects(enqueue(input(), f.connection), insertStage === "row"
        ? /UUID/ : (error: unknown) => error === f.sentinel);
      assert.equal(f.finalized(), 1); assert.equal(f.inserts(), 1);
      assert.equal(f.insertFinalized(), insertStage === "prepare" ? 0 : 1);
      assert.deepEqual(rows(), before); assert.equal(db.inTransaction, false);
      assert.equal((cached.get(originalID) as { id: string }).id, originalID);
      assert(await enqueue());
    });
  }
  for (const blocked of [false, true]) {
    await test(`TEMP shadow cannot redirect guard or insertion blocked=${blocked}`, async () => {
      if (blocked) await seed("validating");
      db.exec("CREATE TEMP TABLE apply_jobs(status TEXT); INSERT INTO temp.apply_jobs VALUES('queued')");
      try {
        const result = await enqueue();
        assert.equal(result === undefined, blocked);
        assert.deepEqual(db.query("SELECT status FROM temp.apply_jobs").all(), [{ status: "queued" }]);
      } finally { db.exec("DROP TABLE temp.apply_jobs"); }
    });
  }
  await test("guard is finalized before insertion hook and projection; commit gates publication", async () => {
    db.exec("CREATE TABLE main.conditional_hook(id TEXT)");
    const f = guardFacade(); let projected = false;
    try {
      const result = await createInternalSQLiteConditionalJobEnqueuer({
        hook(connection, insertedID) {
          assert.equal(connection, f.connection); assert.equal(f.finalized(), 1);
          assert.equal(f.insertFinalized(), 1); assert(db.inTransaction && !projected);
          connection.query("INSERT INTO main.conditional_hook VALUES(?)").run(insertedID);
          const external = openSQLite(path);
          try { assert.equal(getSQLiteJobRecord(external, insertedID), undefined); }
          finally { external.close(); }
        },
        project(row) { assert(db.inTransaction); projected = true; return projectSQLiteJobRecord(row); },
      })(f.connection, input());
      assert(result && projected && !db.inTransaction);
      assert.equal((db.query("SELECT id FROM main.conditional_hook").get() as { id: string }).id, result.id);
    } finally { db.exec("DROP TABLE main.conditional_hook"); }
  });
  for (const failure of ["hook", "projection", "commit"] as const) {
    await test(`executed INSERT and local SQL, ${failure} failure fully rolls back/reopens`, async () => {
      db.exec("CREATE TABLE main.conditional_guard(id TEXT REFERENCES config_revisions(id) DEFERRABLE INITIALLY DEFERRED)");
      const before = rows(), sentinel = new Error(failure);
      let insertedID = "", projected = false, published: JobRecord | undefined;
      try {
        const run = createInternalSQLiteConditionalJobEnqueuer({
          hook(connection, value) {
            insertedID = value; assert(connection.query("SELECT id FROM main.apply_jobs WHERE id=?").get(value));
            connection.query("INSERT INTO main.conditional_guard VALUES(?)").run(failure === "commit" ? id(99) : revision);
            if (failure === "hook") throw sentinel;
          },
          project(row) {
            assert(db.inTransaction); projected = true;
            const result = projectSQLiteJobRecord(row);
            if (failure === "projection") throw sentinel;
            return result;
          },
        });
        await assert.rejects(async () => { published = await run(db, input()); },
          failure === "commit" ? /FOREIGN KEY/i : (error: unknown) => error === sentinel);
        assert(insertedID); assert.equal(projected, failure !== "hook"); assert.equal(published, undefined);
        assert.deepEqual(rows(), before); assert.equal(db.inTransaction, false);
        assert.deepEqual(db.query("SELECT * FROM main.conditional_guard").all(), []);
        const reopened = openSQLite(path);
        try { assert.equal(getSQLiteJobRow(reopened, insertedID), undefined); }
        finally { reopened.close(); }
        assert(await enqueue());
      } finally { db.exec("DROP TABLE main.conditional_guard"); }
    });
  }
  await test("blocked undefined is published only after real successful COMMIT", async () => {
    await seed("queued");
    db.exec("CREATE TABLE main.conditional_guard(id TEXT REFERENCES config_revisions(id) DEFERRABLE INITIALLY DEFERRED)");
    const before = rows(); let published = false;
    const f = guardFacade(undefined, () => db.query("INSERT INTO main.conditional_guard VALUES(?)").run(id(99)));
    try {
      await assert.rejects(async () => { await enqueue(input(), f.connection); published = true; }, /FOREIGN KEY/i);
      assert.equal(published, false); assert.equal(f.inserts(), 0); assert.equal(f.finalized(), 1);
      assert.equal(db.inTransaction, false); assert.deepEqual(rows(), before);
      assert.deepEqual(db.query("SELECT * FROM main.conditional_guard").all(), []);
      const reopened = openSQLite(path);
      try { assert.deepEqual(reopened.query("SELECT * FROM main.conditional_guard").all(), []); }
      finally { reopened.close(); }
      assert.equal(await enqueue(), undefined);
    } finally { db.exec("DROP TABLE main.conditional_guard"); }
  });
  for (const run of [createInternalSQLiteConditionalJobEnqueuer, createInternalSQLiteJobEnqueuer]) {
    await test(`shared path async/thenable guard ${run.name}`, async () => {
      let invoked = false, assimilated = false;
      const before = rows();
      await assert.rejects(run({ hook: async () => { invoked = true; } })(db, input()), /Async/);
      assert.equal(invoked, false);
      await assert.rejects(run({ hook: () => ({ then() { assimilated = true; } }) })(db, input()), /thenable/);
      assert.equal(assimilated, false);
      await assert.rejects(run({ hook: () => Promise.reject(new Error("observed hook rejection")) })(db, input()), /thenable/);
      await new Promise((resolve) => setTimeout(resolve, 0));
      assert.deepEqual(rows(), before);
      const project = (() => Promise.resolve({})) as unknown as (row: Parameters<typeof projectSQLiteJobRecord>[0]) => JobRecord;
      await assert.rejects(run({ project })(db, input()), /Async write results/);
      assert.deepEqual(rows(), before);
    });
  }
  for (const blocked of [false, true]) {
    await test(`caller BEGIN/savepoint rejected without pending mutation blocked=${blocked}`, async () => {
      if (blocked) await seed("queued");
      db.exec("CREATE TABLE main.conditional_pending(value TEXT)");
      try {
        db.exec("BEGIN; INSERT INTO main.conditional_pending VALUES('pending')");
        const before = rows();
        for (const savepoint of [false, true]) {
          if (savepoint) db.exec("SAVEPOINT caller");
          await assert.rejects(enqueue(), /Nested write transaction/);
          assert(db.inTransaction); assert.deepEqual(rows(), before);
          assert.deepEqual(db.query("SELECT * FROM main.conditional_pending").all(), [{ value: "pending" }]);
          const external = openSQLite(path);
          try { assert.deepEqual(external.query("SELECT * FROM main.conditional_pending").all(), []); }
          finally { external.close(); }
          if (savepoint) db.exec("RELEASE caller");
        }
        db.exec("ROLLBACK");
        assert.deepEqual(db.query("SELECT * FROM main.conditional_pending").all(), []);
      } finally { if (db.inTransaction) db.exec("ROLLBACK"); db.exec("DROP TABLE main.conditional_pending"); }
    });
  }
  await test("closed DB rejects, reopen and subsequent reservation recover", async () => {
    const closed = openSQLite(path); closed.close();
    await assert.rejects(enqueue(input(), closed)); assert(await enqueue());
  });
  const durable = (await enqueue({ ...input(), correlationId: "Bun-durable-conditional" }))!;
  const reopened = openSQLite(path);
  try { assert.equal(getSQLiteJobRecord(reopened, durable.id)!.correlationId, "Bun-durable-conditional"); }
  finally { reopened.close(); }
  assert.equal(cases, 105, `complete conditional fixture coverage ${cases}`);
  console.log(`COMPLETE conditional enqueue cases=${cases}`);
} else throw new Error(`Unknown mode ${mode}`);
db.close();
