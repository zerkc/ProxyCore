import assert from "node:assert/strict";
import { Database, type SQLQueryBindings } from "bun:sqlite";
import { createInterface } from "node:readline";
import { openSQLite } from "./open.js";
import { encodeTimestamp } from "./timestamp.js";
import { getSQLiteJobRow } from "./job-read.js";
import { getSQLiteJobRecord, projectSQLiteJobRecord } from "./job-record.js";
import { enqueueSQLiteJobRecord, createInternalSQLiteJobEnqueuer } from "./job-enqueue-record.js";
import type { JobEnqueueInput, JobRecord, JobStore } from "../ports.js";

type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2)
      ? true : false : false;
type Expect<T extends true> = T;
type CanonicalParameters = Expect<Identical<Parameters<typeof enqueueSQLiteJobRecord>,
  [db: Database, job: JobEnqueueInput]>>;
type CanonicalReturn = Expect<Identical<ReturnType<typeof enqueueSQLiteJobRecord>, ReturnType<JobStore["enqueue"]>>>;
type CanonicalMapper = Expect<Identical<ReturnType<typeof projectSQLiteJobRecord>, JobRecord>>;

const [mode, path, originalID, revision, correlation] = process.argv.slice(2);
const db = openSQLite(path);
const id = (n: number) => `aaaaaaaa-aaaa-aaaa-aaaa-${String(n).padStart(12, "0")}`;
const input = (): JobEnqueueInput => ({ revisionId: revision, target: "combined", correlationId: correlation ?? "child-correlation" });
const keys = ["id", "revisionId", "actorUserId", "target", "status", "source", "sourcePrimaryId",
  "sourceNodeId", "sourceRevisionId", "snapshotContentHash", "snapshotVersion", "replicationVersion",
  "leadershipGeneration", "correlationId", "createdAt", "claimedAt", "startedAt", "finishedAt",
  "validationOutput", "applyOutput", "healthOutput", "errorMessage"];
const outputs = ["validationOutput", "applyOutput", "healthOutput"] as const;
const outputColumns = ["validation_output", "apply_output", "health_output"];
const numeric = ["snapshotVersion", "replicationVersion", "leadershipGeneration"] as const;
const dates = ["claimedAt", "startedAt", "finishedAt"] as const;
const dateColumns = ["claimed_at", "started_at", "finished_at"];
const optional = ["actorUserId", "sourcePrimaryId", "sourceNodeId", "sourceRevisionId",
  "snapshotContentHash", ...numeric, ...dates, ...outputs, "errorMessage"] as const;
let cases = 0;
function rows() { return db.query("SELECT * FROM main.apply_jobs ORDER BY id").all(); }
async function check(name: string, body: () => void | Promise<void>) {
  await body(); cases++; console.log(`PASS ${name}`);
}
async function test(name: string, body: () => void | Promise<void>) {
  const before = rows();
  await check(name, async () => {
    try {
      await body();
      assert.equal(db.inTransaction, false);
    } finally {
      if (db.inTransaction) db.exec("ROLLBACK");
      db.query("DELETE FROM main.apply_jobs WHERE id != ?").run(originalID);
    }
    assert.deepEqual(rows(), before);
  });
}
async function enqueue(job = input(), connection = db) {
  const promise = enqueueSQLiteJobRecord(connection, job);
  assert(promise instanceof Promise);
  const result = await promise;
  assert.deepEqual(Object.keys(result).sort(), [...keys].sort());
  return result;
}
async function rejected(job: JobEnqueueInput, pattern?: RegExp) {
  const before = rows();
  if (pattern) await assert.rejects(enqueue(job), pattern);
  else await assert.rejects(enqueue(job));
  assert.deepEqual(rows(), before);
  assert.equal(db.inTransaction, false);
}

// A connection-local facade owns real statements; it never patches globals or
// cached caller statements. Errors are injected around actual SQLite operations.
function facade(stage: "prepare" | "step" | "finalize" | "numbers", error = new Error(stage),
  alter?: (row: Record<string, unknown>) => void) {
  let finalized = 0, stepped = 0;
  const connection = {
    get inTransaction() { return db.inTransaction; },
    transaction: db.transaction.bind(db),
    prepare(sql: string) {
      assert(sql.startsWith("INSERT INTO main.apply_jobs"));
      assert(sql.includes("RETURNING") && !sql.includes("RETURNING *"));
      if (stage === "prepare") throw error;
      const statement = db.prepare<Record<string, unknown>, SQLQueryBindings[]>(sql);
      return {
        all(...bindings: SQLQueryBindings[]) {
          if (stage === "step") throw error;
          const result = statement.all(...bindings);
          stepped++;
          if (stage === "numbers") {
            for (const row of result) {
              if (alter) alter(row);
              else row.leadership_generation = 42;
            }
          }
          return result;
        },
        finalize() {
          statement.finalize(); finalized++;
          if (stage === "finalize") throw error;
        },
      };
    },
  } as unknown as Database;
  return { connection, finalized: () => finalized, stepped: () => stepped };
}

if (mode === "child") {
  db.exec("PRAGMA busy_timeout=150");
  const reader = createInterface({ input: process.stdin });
  console.log("READY");
  for await (const command of reader) {
    if (command === "exit") break;
    if (command === "hold") { db.exec("BEGIN IMMEDIATE"); console.log("HELD"); }
    else if (command === "release") { db.exec("ROLLBACK"); console.log("RELEASED"); }
    else if (command === "enqueue") {
      try { console.log(`ENQUEUED ${(await enqueue()).id}`); }
      catch (error) {
        if (/locked|busy/i.test(String(error))) console.log("BUSY");
        else throw error;
      }
    } else throw new Error(`Unknown command ${command}`);
  }
  reader.close();
  if (db.inTransaction) db.exec("ROLLBACK");
} else if (mode === "unit") {
  await test("defaults, generated UUID, exact own keys and detached values", async () => {
    const before = Date.now();
    const job = await enqueue();
    assert.match(job.id, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    assert.equal(job.revisionId, revision);
    assert.equal(job.correlationId, correlation);
    assert.equal(job.status, "queued"); assert.equal(job.source, "ordinary");
    assert(job.createdAt.getTime() >= before && job.createdAt.getTime() <= Date.now());
    for (const key of optional) assert.equal(job[key], undefined);
    const raw = getSQLiteJobRow(db, job.id)!;
    const second = getSQLiteJobRecord(db, job.id)!;
    assert.notEqual(job.createdAt, second.createdAt);
    job.createdAt.setTime(0);
    assert.deepEqual(getSQLiteJobRow(db, job.id), raw);
  });
  for (const target of ["coredns", "nginx", "combined", "certificate"] as const) {
    for (const status of ["queued", "validating", "applying", "applied", "failed", "rolled-back"] as const) {
      for (const source of ["ordinary", "import", "sync"] as const) {
        await test(`domain ${target}/${status}/${source}`, async () => {
          const job = await enqueue({ ...input(), target, status, source });
          assert.equal(job.target, target); assert.equal(job.status, status); assert.equal(job.source, source);
        });
      }
    }
  }
  for (const value of [undefined, null]) {
    await test(`nullish status/source defaults ${value}`, async () => {
      const job = await enqueue({ ...input(), status: value, source: value } as unknown as JobEnqueueInput);
      assert.equal(job.status, "queued"); assert.equal(job.source, "ordinary");
    });
    await test(`all optional fields ${value}`, async () => {
      const job = input();
      for (const key of optional) Object.assign(job, { [key]: value });
      const result = await enqueue(job);
      for (const key of optional) assert.equal(result[key], undefined);
      const row = getSQLiteJobRow(db, result.id)!;
      for (const column of [...outputColumns, ...dateColumns, "actor_user_id", "snapshot_version",
        "replication_version", "leadership_generation"]) assert.equal(row[column as keyof typeof row], null);
    });
  }
  for (const key of ["revisionId", "target", "correlationId"] as const) {
    for (const value of [undefined, null]) {
      await test(`required ${key} ${value}`, async () => {
        await rejected({ ...input(), [key]: value } as unknown as JobEnqueueInput);
      });
    }
    await test(`required absent ${key}`, async () => {
      const job = input(); delete (job as Partial<JobEnqueueInput>)[key]; await rejected(job);
    });
  }
  for (const patch of [{ target: "invalid" }, { status: "invalid" }, { source: "invalid" },
    { revisionId: id(99) }, { actorUserId: id(98) }]) {
    await test(`invalid domain/reference ${JSON.stringify(patch)}`, async () => {
      await rejected({ ...input(), ...patch } as JobEnqueueInput);
    });
  }
  await test("all supported metadata, optional UUIDs and literal correlation preserved", async () => {
    const job = await enqueue({ ...input(), sourcePrimaryId: id(1), sourceNodeId: id(2),
      sourceRevisionId: id(3), snapshotContentHash: "", errorMessage: "", correlationId: "opaque:{ID}/é",
      snapshotVersion: 3, replicationVersion: -4, leadershipGeneration: 5,
      claimedAt: new Date(-1), startedAt: new Date(0), finishedAt: new Date(123),
      validationOutput: { nested: [1] }, applyOutput: [false], healthOutput: "" });
    assert.equal(job.sourcePrimaryId, id(1)); assert.equal(job.sourceNodeId, id(2));
    assert.equal(job.sourceRevisionId, id(3)); assert.equal(job.snapshotContentHash, "");
    assert.equal(job.errorMessage, ""); assert.equal(job.correlationId, "opaque:{ID}/é");
    assert.equal(job.claimedAt!.getTime(), -1); assert.equal(job.startedAt!.getTime(), 0);
    assert.equal(job.finishedAt!.getTime(), 123);
    (job.validationOutput as { nested: number[] }).nested.push(2);
    assert.deepEqual(getSQLiteJobRecord(db, job.id)!.validationOutput, { nested: [1] });
  });
  for (const key of outputs) {
    for (const value of [null, false, 0, "", NaN, Infinity, -Infinity,
      { toJSON() { return undefined; } }, () => 1, Symbol("x")]) {
      await test(`JSON ${key} ${String(value)}`, async () => {
        const job = await enqueue({ ...input(), [key]: value });
        const encoded = JSON.stringify(value);
        assert.equal(getSQLiteJobRow(db, job.id)![outputColumns[outputs.indexOf(key)] as "validation_output"],
          value === null || encoded === undefined ? null : encoded);
        assert.deepEqual(job[key], encoded === undefined ? undefined : JSON.parse(encoded) ?? undefined);
      });
    }
    await test(`JSON serialized null ${key}`, async () => {
      const job = await enqueue({ ...input(), [key]: { toJSON() { return null; } } });
      assert.equal(job[key], undefined);
      assert.equal(getSQLiteJobRow(db, job.id)![outputColumns[outputs.indexOf(key)] as "validation_output"], "null");
    });
    for (const kind of ["throw", "bigint", "cycle"] as const) {
      await test(`JSON encoder pre-mutation ${key}/${kind}`, async () => {
        const sentinel = new Error("toJSON sentinel");
        const cycle: Record<string, unknown> = {}; cycle.self = cycle;
        const value = kind === "throw" ? { toJSON() { throw sentinel; } } : kind === "bigint" ? 1n : cycle;
        let prepared = false;
        const connection = {
          get inTransaction() { return db.inTransaction; }, transaction: db.transaction.bind(db),
          prepare() { prepared = true; throw new Error("must not prepare"); },
        } as unknown as Database;
        const before = rows();
        await assert.rejects(enqueue({ ...input(), [key]: value }, connection),
          kind === "throw" ? (error: unknown) => error === sentinel : /BigInt|circular|cyclic/i);
        assert.equal(prepared, false); assert.deepEqual(rows(), before);
      });
    }
  }
  for (const key of numeric) {
    const bounds = key === "leadershipGeneration"
      ? [-9223372036854775808n, 9223372036854775807n, 9007199254740993n]
      : [-2147483648, 2147483647, 0];
    for (const value of bounds) {
      await test(`integer ${key}/${value}`, async () => {
        const job = await enqueue({ ...input(), [key]: value } as unknown as JobEnqueueInput);
        assert.equal(job[key], Number(value));
      });
    }
    for (const value of [NaN, Infinity, -Infinity, 1.25, key === "leadershipGeneration" ? 2 ** 63 : 2147483648]) {
      await test(`numeric boundary ${key}/${value}`, async () => {
        await rejected({ ...input(), [key]: value });
      });
    }
  }
  for (const key of dates) {
    for (const value of [new Date(-8640000000000000), new Date(8640000000000000), new Date(-1)]) {
      await test(`date ${key}/${value.toISOString()}`, async () => {
        const job = await enqueue({ ...input(), [key]: value });
        assert.equal(job[key]!.getTime(), value.getTime()); assert.notEqual(job[key], value);
      });
    }
    await test(`invalid Date before SQL ${key}`, async () => {
      const f = facade("prepare", new Error("must not prepare"));
      await assert.rejects(enqueue({ ...input(), [key]: new Date(NaN) }, f.connection), RangeError);
      assert.equal(f.finalized(), 0);
    });
  }
  await test("caller BEGIN and savepoint rejected without altering pending data", async () => {
    db.exec("CREATE TABLE main.enqueue_pending(value TEXT)");
    try {
      db.exec("BEGIN; INSERT INTO main.enqueue_pending VALUES('pending')");
      const before = rows();
      for (const savepoint of [false, true]) {
        if (savepoint) db.exec("SAVEPOINT caller");
        await assert.rejects(enqueue(), /Nested write transaction/);
        assert.equal(db.inTransaction, true); assert.deepEqual(rows(), before);
        assert.equal(db.query<{ value: string }, []>("SELECT value FROM main.enqueue_pending").get()!.value, "pending");
        const external = openSQLite(path);
        try { assert.deepEqual(external.query("SELECT * FROM main.enqueue_pending").all(), []); }
        finally { external.close(); }
        if (savepoint) db.exec("RELEASE caller");
      }
      db.exec("ROLLBACK");
      assert.deepEqual(db.query("SELECT * FROM main.enqueue_pending").all(), []);
    } finally { if (db.inTransaction) db.exec("ROLLBACK"); db.exec("DROP TABLE main.enqueue_pending"); }
  });
  await test("internal local hook runs after finalized INSERT, before projection and COMMIT", async () => {
    db.exec("CREATE TABLE main.enqueue_hook(id TEXT)");
    let finalized = false, projected = false;
    const connection = {
      get inTransaction() { return db.inTransaction; }, transaction: db.transaction.bind(db),
      query: db.query.bind(db),
      prepare(sql: string) {
        const statement = db.prepare<Record<string, unknown>, SQLQueryBindings[]>(sql);
        return { all: statement.all.bind(statement), finalize() { statement.finalize(); finalized = true; } };
      },
    } as unknown as Database;
    try {
      const run = createInternalSQLiteJobEnqueuer({
        hook(suppliedConnection, insertedID) {
          assert.equal(suppliedConnection, connection); assert.equal(db.inTransaction, true);
          assert(finalized && !projected);
          assert(db.query("SELECT id FROM main.apply_jobs WHERE id=?").get(insertedID));
          db.query("INSERT INTO main.enqueue_hook VALUES(?)").run(insertedID);
          const external = openSQLite(path);
          try { assert.equal(external.query("SELECT id FROM main.apply_jobs WHERE id=?").get(insertedID), null); }
          finally { external.close(); }
        },
        project(row) { projected = true; assert(db.inTransaction); return projectSQLiteJobRecord(row); },
      });
      const result = await run(connection, input());
      assert(projected && !db.inTransaction);
      assert.equal(db.query<{ id: string }, []>("SELECT id FROM main.enqueue_hook").get()!.id, result.id);
    } finally { db.exec("DROP TABLE main.enqueue_hook"); }
  });
  await test("throwing local hook rolls back job and hook SQL", async () => {
    db.exec("CREATE TABLE main.enqueue_hook(id TEXT)");
    const before = rows(), sentinel = new Error("hook failure");
    try {
      await assert.rejects(createInternalSQLiteJobEnqueuer({ hook(connection, insertedID) {
        connection.query("INSERT INTO main.enqueue_hook VALUES(?)").run(insertedID);
        throw sentinel;
      } })(db, input()), (error: unknown) => error === sentinel);
      assert.deepEqual(rows(), before); assert.deepEqual(db.query("SELECT * FROM main.enqueue_hook").all(), []);
    } finally { db.exec("DROP TABLE main.enqueue_hook"); }
  });
  await test("async hook rejected before invocation; thenables never executed", async () => {
    let invoked = false, assimilated = false;
    await assert.rejects(createInternalSQLiteJobEnqueuer({ hook: async () => { invoked = true; } })(db, input()), /Async/);
    assert.equal(invoked, false);
    const before = rows();
    await assert.rejects(createInternalSQLiteJobEnqueuer({ hook: () => ({ then() { assimilated = true; } }) })(db, input()), /Async|thenable/);
    assert.equal(assimilated, false); assert.deepEqual(rows(), before);
    await assert.rejects(createInternalSQLiteJobEnqueuer({ hook: () => Promise.reject(new Error("rejected hook")) })(db, input()), /Async|thenable/);
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.deepEqual(rows(), before);
  });
  await test("projection exception inside owned callback rolls back complete effect", async () => {
    const before = rows(), sentinel = new Error("projection failure");
    let hookRan = false;
    await assert.rejects(createInternalSQLiteJobEnqueuer({
      hook() { hookRan = true; },
      project(row) {
        assert(db.inTransaction && hookRan); projectSQLiteJobRecord(row); throw sentinel;
      },
    })(db, input()), (error: unknown) => error === sentinel);
    assert.deepEqual(rows(), before); assert.equal(db.inTransaction, false);
  });
  await test("real deferred FK COMMIT failure: executed INSERT, no result, full rollback", async () => {
    db.exec(`CREATE TABLE main.enqueue_guard(id TEXT REFERENCES config_revisions(id) DEFERRABLE INITIALLY DEFERRED)`);
    const before = rows();
    let insertedID = "", projected = false, published: JobRecord | undefined;
    try {
      const run = createInternalSQLiteJobEnqueuer({
        hook(connection, value) {
          insertedID = value;
          assert(connection.query("SELECT id FROM main.apply_jobs WHERE id=?").get(value));
          connection.query("INSERT INTO main.enqueue_guard VALUES(?)").run(id(99));
        },
        project(row) { projected = true; assert(db.inTransaction); return projectSQLiteJobRecord(row); },
      });
      await assert.rejects(async () => { published = await run(db, input()); }, /FOREIGN KEY/i);
      assert(insertedID && projected); assert.equal(published, undefined);
      assert.equal(db.inTransaction, false); assert.deepEqual(rows(), before);
      assert.deepEqual(db.query("SELECT * FROM main.enqueue_guard").all(), []);
      const external = openSQLite(path);
      try { assert.equal(getSQLiteJobRow(external, insertedID), undefined); } finally { external.close(); }
      await enqueue();
    } finally { db.exec("DROP TABLE main.enqueue_guard"); }
  });
  for (const stage of ["prepare", "step", "finalize", "numbers"] as const) {
    await test(`statement/validation error ${stage} cleans up and preserves caller cache`, async () => {
      const cached = db.query("SELECT id FROM main.apply_jobs WHERE id=?");
      const sentinel = new Error(stage), f = facade(stage, sentinel), before = rows();
      await assert.rejects(enqueue(input(), f.connection), stage === "numbers" ? /integer/ : (error: unknown) => error === sentinel);
      assert.equal(f.finalized(), stage === "prepare" ? 0 : 1);
      assert.equal(f.stepped(), stage === "finalize" || stage === "numbers" ? 1 : 0);
      assert.deepEqual(rows(), before); assert.equal(db.inTransaction, false);
      assert.equal((cached.get(originalID) as { id: string }).id, originalID);
      await enqueue();
    });
  }
  await test("TEMP shadow cannot redirect INSERT", async () => {
    db.exec("CREATE TEMP TABLE apply_jobs(sentinel TEXT)");
    try {
      await enqueue(); assert.deepEqual(db.query("SELECT * FROM temp.apply_jobs").all(), []);
    } finally { db.exec("DROP TABLE temp.apply_jobs"); }
  });
  const malformed: [string, (row: Record<string, unknown>) => void][] = [
    ["missing column", (row) => { delete row.id; }],
    ["extra column", (row) => { row.extra = 1; }],
    ["required null", (row) => { row.correlation_id = null; }],
    ["NUL text", (row) => { row.error_message = "bad\0text"; }],
    ["UUID", (row) => { row.source_primary_id = "bad"; }],
    ["UUID newline", (row) => { row.source_node_id = `${id(1)}\n`; }],
    ["JSON", (row) => { row.validation_output = "{"; }],
    ["timestamp", (row) => { row.created_at = "bad"; }],
    ["target", (row) => { row.target = "bad"; }],
    ["status", (row) => { row.status = "bad"; }],
    ["source", (row) => { row.source = "bad"; }],
    ["int32", (row) => { row.snapshot_version = 2147483648n; }],
    ["int64", (row) => { row.leadership_generation = 9223372036854775808n; }],
  ];
  for (const [name, alter] of malformed) {
    await test(`malformed RETURNING ${name} rejects complete effect`, async () => {
      const before = rows(), f = facade("numbers", undefined, alter);
      await assert.rejects(enqueue(input(), f.connection));
      assert.equal(f.stepped(), 1); assert.equal(f.finalized(), 1);
      assert.deepEqual(rows(), before); assert.equal(db.inTransaction, false);
    });
  }
  await test("property capture precedes serializers and ignores extra input properties", async () => {
    const accesses: string[] = [];
    const job = Object.create({ correlationId: correlation }) as JobEnqueueInput;
    Object.assign(job, { revisionId: revision, target: "combined", createdAt: new Date(NaN), id: id(99) });
    Object.defineProperty(job, "validationOutput", { get() {
      accesses.push("JSON getter");
      return { toJSON() { accesses.push("serialize"); return { ok: true }; } };
    } });
    Object.defineProperty(job, "claimedAt", { get() { accesses.push("date getter"); return new Date(0); } });
    const result = await enqueue(job);
    assert.deepEqual(accesses, ["JSON getter", "date getter", "serialize"]);
    assert.notEqual(result.id, id(99)); assert(Number.isFinite(result.createdAt.getTime()));
    assert.equal(result.correlationId, correlation); assert.deepEqual(result.validationOutput, { ok: true });
  });
  await test("async projection result rolls back via writer result guard", async () => {
    const before = rows();
    const project = (() => Promise.resolve({})) as unknown as (row: Parameters<typeof projectSQLiteJobRecord>[0]) => JobRecord;
    await assert.rejects(createInternalSQLiteJobEnqueuer({ project })(db, input()), /Async write results/);
    assert.deepEqual(rows(), before); assert.equal(db.inTransaction, false);
  });
  await test("general infinity Invalid Date policy leaves native bytes intact", async () => {
    for (const value of ["0", "2"]) {
      const f = facade("numbers", undefined, (row) => { row.claimed_at = value; row.created_at = value; });
      const result = await createInternalSQLiteJobEnqueuer({ hook(_connection, insertedID) {
        // Same physical connection: store the instrumented RETURNING bytes to
        // prove projection accepts infinities without changing native storage.
        db.query("UPDATE main.apply_jobs SET claimed_at=?,created_at=? WHERE id=?").run(value, value, insertedID);
      } })(f.connection, input());
      assert(Number.isNaN(result.createdAt.getTime())); assert(Number.isNaN(result.claimedAt!.getTime()));
      assert.equal(getSQLiteJobRow(db, result.id)!.claimed_at, value);
      assert.equal(getSQLiteJobRow(db, result.id)!.created_at, value);
    }
  });
  await test("closed database rejects; next ordinary connection still writes", async () => {
    const closed = openSQLite(path); closed.close();
    await assert.rejects(enqueue(input(), closed)); await enqueue();
  });
  for (const value of [Number.MAX_SAFE_INTEGER, Number.MAX_SAFE_INTEGER + 1, -(Number.MAX_SAFE_INTEGER + 1)]) {
    await test(`number-mode unsafe leadership ${value}`, async () => {
      const job = await enqueue({ ...input(), leadershipGeneration: value });
      assert.equal(job.leadershipGeneration, value);
      assert.equal(getSQLiteJobRow(db, job.id)!.leadership_generation, BigInt(value));
    });
  }
  for (const key of dates) {
    await test(`non-Date input invokes source encoder ${key}`, async () => {
      await rejected({ ...input(), [key]: "2020-01-01" } as unknown as JobEnqueueInput, /toISOString/);
    });
  }
  await test("actual number-mode connection rejects native numeric row and rolls back", async () => {
    const connection = new Database(path, { strict: true, safeIntegers: false });
    connection.exec("PRAGMA foreign_keys=ON");
    const before = rows();
    try { await assert.rejects(enqueue({ ...input(), leadershipGeneration: 1 }, connection), /integer/); }
    finally { connection.close(); }
    assert.deepEqual(rows(), before);
  });
  const durable = await enqueue({ ...input(), correlationId: "Bun-durable-enqueue" });
  const reopened = openSQLite(path);
  try { assert.equal(getSQLiteJobRecord(reopened, durable.id)!.correlationId, "Bun-durable-enqueue"); }
  finally { reopened.close(); }
  assert.equal(cases, 205, `complete fixture coverage ${cases}`);
  console.log(`COMPLETE enqueue cases=${cases}`);
} else throw new Error(`Unknown mode ${mode}`);
db.close();
