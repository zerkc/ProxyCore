import assert from "node:assert/strict";
import { Database, type SQLQueryBindings } from "bun:sqlite";
import type { JobRecord, JobStore } from "../ports.js";
import { openSQLite } from "./open.js";
import { encodeTimestamp } from "./timestamp.js";
import { updateSQLiteJobRecord } from "./job-update-record.js";

type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2)
      ? true : false
    : false;
type Expect<T extends true> = T;
type CanonicalParameters = Expect<Identical<Parameters<typeof updateSQLiteJobRecord>,
  [db: Database, id: Parameters<JobStore["update"]>[0], patch: Parameters<JobStore["update"]>[1]]>>;
type CanonicalReturn = Expect<Identical<ReturnType<typeof updateSQLiteJobRecord>,
  ReturnType<JobStore["update"]>>>;

const [path, originalID] = process.argv.slice(2);
const db = openSQLite(path);
const original = db.query<Record<string, unknown>, [string]>("SELECT * FROM main.apply_jobs WHERE id=?").get(originalID)!;
const revision = original.revision_id as string;
const id = "bbbbbbbb-bbbb-bbbb-bbbb-000000000001";
const missing = "bbbbbbbb-bbbb-bbbb-bbbb-000000000002";
const rawJSON = ' \n{"n":922337203685477580812345,"snow":"雪🦊"} \t';
const stamp = (seconds: bigint, nanos = 0) => encodeTimestamp({ kind: "finite", seconds, nanos })!;
const patch = (value: unknown) => value as Partial<JobRecord>;
let cases = 0;
function row(connection = db, key = id) {
  // Own uncached verification queries so ALTER TABLE fixtures cannot leave a
  // cached SELECT * column shape stale after schema restoration.
  const statement = connection.prepare<Record<string, unknown>, [string]>("SELECT * FROM main.apply_jobs WHERE id=?");
  try { return statement.get(key)!; }
  finally { statement.finalize(); }
}
function insert() {
  db.query(`INSERT INTO main.apply_jobs(id,revision_id,target,status,source,correlation_id,created_at,
    claimed_at,started_at,finished_at,validation_output,apply_output,health_output,error_message,
    source_primary_id,source_node_id,source_revision_id,snapshot_content_hash,snapshot_version,
    replication_version,leadership_generation)
    VALUES(?,?,'combined','queued','sync','correlation','0','2',?,NULL,?,'null',NULL,'old',
      ?,?,?,'',-2147483648,2147483647,9223372036854775807)`)
    .run(id, revision, stamp(-1n, 999999999), rawJSON, missing, missing, missing);
}
async function update(value: unknown, connection = db, key = id) {
  const promise = updateSQLiteJobRecord(connection, key, patch(value));
  assert(promise instanceof Promise);
  return promise;
}
async function test(name: string, body: () => void | Promise<void>) {
  db.query("DELETE FROM main.apply_jobs WHERE id=?").run(id);
  insert();
  const revisions = db.query("SELECT * FROM main.config_revisions ORDER BY id").all();
  await body();
  assert.equal(db.inTransaction, false);
  assert.deepEqual(db.query("SELECT * FROM main.config_revisions ORDER BY id").all(), revisions);
  assert.deepEqual(row(db, originalID), original);
  cases++;
  console.log(`PASS ${name}`);
}
async function unchanged(body: () => Promise<unknown>, expected?: RegExp | ((error: unknown) => boolean)) {
  const before = row();
  if (expected) await assert.rejects(body, expected);
  else await assert.rejects(body);
  assert.deepEqual(row(), before);
  const reopened = openSQLite(path);
  try { assert.deepEqual(row(reopened), before); }
  finally { reopened.close(); }
}

// Real statements behind a local facade: no production hook and no reread.
// all() must reach SQLITE_DONE before finalize(), validation or publication.
function facade(options: { transform?: (row: Record<string, unknown>) => unknown;
  prepareError?: Error; executeError?: Error; finalizeError?: Error } = {}) {
  const state = { prepares: 0, executions: 0, completions: 0, finalizations: 0 };
  const connection = {
    prepare(sql: string) {
      state.prepares++;
      assert.match(sql, /^UPDATE main\.apply_jobs SET /);
      assert.match(sql, / WHERE id = \? RETURNING /);
      assert.doesNotMatch(sql, /SELECT|BEGIN|COMMIT/i);
      if (options.prepareError) throw options.prepareError;
      const statement = db.prepare<Record<string, unknown>, SQLQueryBindings[]>(sql);
      return {
        all(...bindings: SQLQueryBindings[]) {
          state.executions++;
          if (options.executeError) throw options.executeError;
          const rows = statement.all(...bindings);
          state.completions++;
          return options.transform ? rows.map(options.transform) : rows;
        },
        finalize() {
          state.finalizations++;
          statement.finalize();
          if (options.finalizeError) throw options.finalizeError;
        },
      };
    },
  } as unknown as Database;
  return { connection, state };
}

try {
  await test("actual-implicit-COMMIT-failure-full-rollback-no-publication", async () => {
    const before = row();
    const changes = () => db.query<{ n: bigint }, []>("SELECT total_changes() AS n").get()!.n;
    db.exec(`CREATE TABLE main.public_update_guard(job_id TEXT REFERENCES apply_jobs(id) DEFERRABLE INITIALLY DEFERRED);
      CREATE TEMP TRIGGER public_update_guard_insert AFTER UPDATE ON main.apply_jobs
        WHEN OLD.status='queued' AND NEW.status='applied' BEGIN
        INSERT INTO public_update_guard VALUES('missing-fixture-' || NEW.status); END`);
    const baseline = changes();
    try {
      assert.equal(db.inTransaction, false, "no explicit transaction: statement owns its implicit commit");
      let published: JobRecord | undefined;
      const seam = facade();
      await assert.rejects(async () => { published = await update({ status: "applied" }, seam.connection); }, /FOREIGN KEY/);
      assert.equal(published, undefined);
      assert.equal(seam.state.executions, 1);
      assert.equal(seam.state.completions, 0, "SQLITE_DONE failed at implicit COMMIT");
      assert.equal(seam.state.finalizations, 1, "failed RETURNING cursor was finalized");
      // An implicit-commit failure excludes the outer UPDATE from total_changes.
      // Its AFTER trigger's completed INSERT still counts: the WHEN clause
      // proves the actual OLD->NEW mutation, not a before-execution error.
      assert.equal(changes() - baseline, 1n, "AFTER UPDATE observed queued->applied and inserted the deferred violation");
      assert.equal(db.inTransaction, false);
      assert.deepEqual(row(), before);
      assert.equal(db.query<{ n: bigint }, []>("SELECT count(*) AS n FROM public_update_guard").get()!.n, 0n);
      const reopened = openSQLite(path);
      try {
        assert.deepEqual(row(reopened), before);
        assert.equal(reopened.query<{ n: bigint }, []>("SELECT count(*) AS n FROM public_update_guard").get()!.n, 0n);
      } finally { reopened.close(); }
    } finally { db.exec("DROP TRIGGER temp.public_update_guard_insert; DROP TABLE main.public_update_guard"); }
    assert.equal((await update({ status: "applied" })).status, "applied");
  });
  for (const value of [{}, { status: undefined }, { ignored: true },
    { status: undefined, validationOutput: undefined, applyOutput: undefined, healthOutput: undefined,
      errorMessage: undefined, claimedAt: undefined, startedAt: undefined, finishedAt: undefined },
    { id: missing, revisionId: missing, target: "nginx", source: "ordinary", createdAt: new Date(0),
      leadershipGeneration: 1, snapshotVersion: 1, replicationVersion: 1 }]) {
    await test(`empty-effective-${JSON.stringify(value)}`, async () => {
      const seam = facade();
      await unchanged(() => update(value, seam.connection), /^Error: No values to set$/);
      assert.equal(seam.state.prepares, 0);
      await assert.rejects(update(value, seam.connection, missing), /^Error: No values to set$/);
    });
  }
  await test("inherited-selected-nonundefined-and-own-undefined-shadow", async () => {
    const value = Object.create({ status: "applied", validationOutput: { inherited: true }, ignored: true });
    const result = await update(value);
    assert.equal(result.status, "applied");
    assert.deepEqual(result.validationOutput, { inherited: true });
    value.status = undefined;
    value.validationOutput = undefined;
    await unchanged(() => update(value), /No values to set/);
  });
  await test("ignored-getters-not-accessed-own-undefined-not-selected", async () => {
    const value = { status: "failed", validationOutput: undefined,
      get leadershipGeneration() { throw new Error("ignored getter"); } };
    const before = row();
    await update(value);
    assert.deepEqual(row(), { ...before, status: "failed" });
  });
  const selected = ["status", "validationOutput", "applyOutput", "healthOutput", "errorMessage",
    "claimedAt", "startedAt", "finishedAt"];
  const native = ["status", "validation_output", "apply_output", "health_output", "error_message",
    "claimed_at", "started_at", "finished_at"];
  for (let index = 1; index < selected.length; index++) {
    await test(`nullable-clear-${selected[index]}`, async () => {
      const before = row();
      const result = await update({ [selected[index]]: null });
      assert.deepEqual(row(), { ...before, [native[index]]: null });
      assert(Object.hasOwn(result, selected[index]));
      assert.equal((result as unknown as Record<string, unknown>)[selected[index]], undefined);
    });
  }
  for (const value of [null, "invalid-status"]) {
    await test(`status-database-constraint-${value}`, async () => {
      await unchanged(() => update({ status: value }), /NOT NULL|CHECK/);
    });
  }
  for (const status of ["queued", "validating", "applying", "applied", "failed", "rolled-back"]) {
    await test(`six-states-without-autoclock-${status}`, async () => {
      const before = row();
      const result = await update({ status });
      assert.equal(result.status, status);
      assert.deepEqual(row(), { ...before, status });
    });
  }
  for (const field of ["validationOutput", "applyOutput", "healthOutput"]) {
    for (const value of [{ deep: [1, "雪🦊", null] }, false, 0, "", [1], NaN, Infinity,
      () => undefined, Symbol("undefined"), { toJSON() { return undefined; } }]) {
      await test(`JSON-${field}-${String(value)}`, async () => {
        const text = JSON.stringify(value) ?? null;
        const result = await update({ [field]: value });
        const column = native[selected.indexOf(field)];
        assert.equal(row()[column], text);
        assert.deepEqual((result as unknown as Record<string, unknown>)[field], text === null ? undefined : JSON.parse(text) ?? undefined);
      });
    }
    for (const kind of ["cycle", "bigint", "throw"]) {
      await test(`JSON-error-${field}-${kind}-before-SQL-and-missing-ID`, async () => {
        const sentinel = new Error("serializer sentinel");
        const cycle: Record<string, unknown> = {}; cycle.self = cycle;
        const value = kind === "cycle" ? cycle : kind === "bigint" ? 1n : { toJSON() { throw sentinel; } };
        const seam = facade();
        const expected = kind === "throw" ? (error: unknown) => error === sentinel : /circular|cyclic|BigInt/i;
        await unchanged(() => update({ status: "applied", [field]: value }, seam.connection), expected);
        await assert.rejects(update({ [field]: value }, seam.connection, missing), expected);
        assert.equal(seam.state.prepares, 0);
      });
    }
  }
  for (const field of ["claimedAt", "startedAt", "finishedAt"]) {
    for (const [millis, seconds, nanos] of [
      [0, 0n, 0], [-1, -1n, 999000000], [-1001, -2n, 999000000], [123456, 123n, 456000000],
      [8640000000000000, 8640000000000n, 0], [-8640000000000000, -8640000000000n, 0],
      [0.9, 0n, 0], [-0.9, 0n, 0],
    ] as const) {
      await test(`Date-${field}-${millis}`, async () => {
        const date = new Date(millis);
        let calls = 0;
        date.toISOString = () => { calls++; return Date.prototype.toISOString.call(date); };
        const result = await update({ [field]: date });
        assert.equal(calls, 1);
        assert.equal(row()[native[selected.indexOf(field)]], stamp(seconds, nanos));
        const projected = (result as unknown as Record<string, Date>)[field];
        assert.equal(projected.getTime(), date.getTime());
        assert.notEqual(projected, date);
      });
    }
    for (const value of [new Date(NaN), "2020-01-01", 0, {}, undefined]) {
      if (value === undefined) continue;
      await test(`Date-encoder-failure-${field}-${String(value)}`, async () => {
        const seam = facade();
        await unchanged(() => update({ [field]: value }, seam.connection), /Invalid time|Invalid Date|toISOString/);
        await assert.rejects(update({ [field]: value }, seam.connection, missing), /Invalid time|Invalid Date|toISOString/);
        assert.equal(seam.state.prepares, 0);
      });
    }
    await test(`Date-encoder-exception-identity-${field}`, async () => {
      const sentinel = new Error("date encoder sentinel");
      const value = { toISOString() { throw sentinel; } };
      await unchanged(() => update({ [field]: value }), (error) => error === sentinel);
    });
    await test(`Date-out-of-type-toISOString-call-${field}`, async () => {
      let calls = 0;
      const result = await update({ [field]: { toISOString() { calls++; return "1970-01-01T00:00:00.001Z"; } } });
      assert.equal(calls, 1);
      assert.equal((result as unknown as Record<string, Date>)[field].getTime(), 1);
    });
  }
  await test("schema-encoder-order-not-patch-insertion-order", async () => {
    const events: string[] = [];
    const sentinel = new Error("validation first");
    const value = {
      finishedAt: { toISOString() { events.push("finished"); throw new Error("late"); } },
      healthOutput: { toJSON() { events.push("health"); throw new Error("late"); } },
      validationOutput: { toJSON() { events.push("validation"); throw sentinel; } },
    };
    await unchanged(() => update(value), (error) => error === sentinel);
    assert.deepEqual(events, ["validation"]);
    events.length = 0;
    await update({ finishedAt: { toISOString() { events.push("finished"); return new Date(0).toISOString(); } },
      startedAt: { toISOString() { events.push("started"); return new Date(0).toISOString(); } },
      claimedAt: { toISOString() { events.push("claimed"); return new Date(0).toISOString(); } },
      healthOutput: { toJSON() { events.push("health"); return 3; } },
      applyOutput: { toJSON() { events.push("apply"); return 2; } },
      validationOutput: { toJSON() { events.push("validation"); return 1; } } });
    assert.deepEqual(events, ["validation", "apply", "health", "claimed", "started", "finished"]);
  });
  for (const initial of ["queued", "validating", "applying", "applied", "failed", "rolled-back"]) {
    for (const status of ["queued", "validating", "applying", "applied", "failed", "rolled-back"]) {
      await test(`unrestricted-transition-${initial}-to-${status}`, async () => {
        db.query("UPDATE main.apply_jobs SET status=? WHERE id=?").run(initial, id);
        const before = row();
        assert.equal((await update({ status })).status, status);
        assert.deepEqual(row(), { ...before, status });
      });
    }
  }
  for (const historical of ["0", "2", stamp(-9223372036854775808n),
    stamp(9223372036854775807n, 999999999), stamp(-1n, 999999999), null]) {
    await test(`unchanged-read-Date-policy-${historical}`, async () => {
      db.query("UPDATE main.apply_jobs SET finished_at=? WHERE id=?").run(historical, id);
      const before = row();
      const result = await update({ errorMessage: "new" });
      assert.deepEqual(row(), { ...before, error_message: "new" });
      assert(Object.hasOwn(result, "finishedAt"));
      if (historical === null) assert.equal(result.finishedAt, undefined);
      else if (historical === stamp(-1n, 999999999)) assert.equal(result.finishedAt!.getTime(), -1);
      else assert(Number.isNaN(result.finishedAt!.getTime()));
    });
  }
  await test("validation-projection-after-finalize-error-identity-durable-autocommit", async () => {
    const sentinel = new Error("projection boundary sentinel");
    const seam = facade({ transform(value) {
      return { ...value, get validation_output() {
        assert.equal(seam.state.completions, 1);
        assert.equal(seam.state.finalizations, 1);
        assert.equal(db.inTransaction, false);
        const reopened = openSQLite(path);
        try { assert.equal(row(reopened).status, "applied"); }
        finally { reopened.close(); }
        throw sentinel;
      } };
    } });
    await assert.rejects(update({ status: "applied" }, seam.connection), (error) => error === sentinel);
    assert.equal(row().status, "applied");
  });
  await test("projection-error-in-caller-transaction-remains-pending-for-caller-rollback", async () => {
    const before = row();
    const seam = facade({ transform(value) { return { ...value, validation_output: "{" }; } });
    const other = openSQLite(path);
    db.exec("BEGIN IMMEDIATE");
    try {
      await assert.rejects(update({ status: "applied" }, seam.connection));
      assert.equal(seam.state.finalizations, 1);
      assert.equal(db.inTransaction, true);
      assert.equal(row().status, "applied");
      assert.deepEqual(row(other), before);
    } finally { db.exec("ROLLBACK"); other.close(); }
    assert.deepEqual(row(), before);
  });
  await test("all-eight-single-statement-exact-returned-row-no-reread", async () => {
    const seam = facade();
    const result = await update({ status: "applied", validationOutput: { a: 1 }, applyOutput: [2],
      healthOutput: false, errorMessage: "new", claimedAt: new Date(1), startedAt: new Date(-1),
      finishedAt: new Date(123456) }, seam.connection);
    assert.equal(result.status, "applied");
    assert.equal(result.errorMessage, "new");
    assert.deepEqual(seam.state, { prepares: 1, executions: 1, completions: 1, finalizations: 1 });
    const reopened = openSQLite(path);
    try { assert.deepEqual(row(reopened), row()); }
    finally { reopened.close(); }
  });
  await test("literal-bound-ID-missing-after-execution-serialization", async () => {
    for (const key of [missing, id.toUpperCase(), "not-a-uuid", "' OR 1=1 --"]) {
      const seam = facade();
      let serialized = false;
      await unchanged(() => update({ validationOutput: { toJSON() { serialized = true; return 1; } } }, seam.connection, key),
        (error) => error instanceof Error && error.message === `Job not found: ${key}`);
      assert(serialized);
      assert.equal(seam.state.executions, 1);
      assert.equal(seam.state.finalizations, 1);
    }
  });
  await test("int64-metadata-preserved-Number-rounding-own-keys-detached", async () => {
    for (const leadership of [-9223372036854775808n, 9007199254740993n, 9223372036854775807n, null]) {
      db.query("UPDATE main.apply_jobs SET leadership_generation=? WHERE id=?").run(leadership, id);
      const before = row();
      const result = await update({ errorMessage: "new", leadershipGeneration: 7, source: "ordinary", createdAt: new Date(0) });
      assert.deepEqual(row(), { ...before, error_message: "new" });
      assert.equal(result.leadershipGeneration, leadership === null ? undefined : Number(leadership));
      assert.equal(result.snapshotVersion, -2147483648);
      assert.equal(result.replicationVersion, 2147483647);
      assert.equal(Object.keys(result).length, 22);
      for (const key of ["actorUserId", "finishedAt", "applyOutput", "healthOutput"]) {
        assert(Object.hasOwn(result, key));
        assert.equal((result as unknown as Record<string, unknown>)[key], undefined);
      }
      assert(Number.isNaN(result.createdAt.getTime()));
      assert(Number.isNaN(result.claimedAt!.getTime()));
      assert.equal(result.startedAt!.getTime(), -1);
      const another = await update({ errorMessage: "new" });
      assert.notEqual(another.createdAt, result.createdAt);
      assert.notEqual(another.validationOutput, result.validationOutput);
      (result.validationOutput as { snow: string }).snow = "changed";
      assert.equal((another.validationOutput as { snow: string }).snow, "雪🦊");
      assert.equal(row().validation_output, rawJSON);
    }
  });
  await test("caller-transaction-pending-visible-nested-allowed-no-implicit-commit", async () => {
    const before = row();
    const other = openSQLite(path);
    db.exec("BEGIN IMMEDIATE; SAVEPOINT caller_nested");
    try {
      const result = await update({ status: "applied" });
      assert.equal(result.status, "applied");
      assert.equal(db.inTransaction, true);
      assert.equal(row().status, "applied");
      assert.deepEqual(row(other), before);
      db.exec("ROLLBACK TO caller_nested; RELEASE caller_nested");
      assert.deepEqual(row(), before);
      await update({ status: "failed" });
      assert.equal(db.inTransaction, true);
      assert.deepEqual(row(other), before);
    } finally { db.exec("ROLLBACK"); other.close(); }
    assert.deepEqual(row(), before);
    assert.equal((await update({ status: "applied" })).status, "applied");
  });
  for (const boundary of ["prepareError", "executeError", "finalizeError"] as const) {
    await test(`source-error-identity-${boundary}`, async () => {
      const sentinel = new Error(boundary);
      const seam = facade({ [boundary]: sentinel });
      let published: unknown;
      await assert.rejects(async () => { published = await update({ status: "applied" }, seam.connection); },
        (error) => error === sentinel);
      assert.equal(published, undefined);
      assert.equal(seam.state.finalizations, boundary === "prepareError" ? 0 : 1);
      assert.equal(row().status, boundary === "finalizeError" ? "applied" : "queued");
      assert.equal((await update({ status: "failed" })).status, "failed");
    });
  }
  await test("SQL-trigger-execution-error-no-publication-next-success", async () => {
    db.exec("CREATE TEMP TRIGGER public_update_fail BEFORE UPDATE ON main.apply_jobs BEGIN SELECT RAISE(ABORT,'fixture-update'); END");
    try { await unchanged(() => update({ status: "applied" }), /fixture-update/); }
    finally { db.exec("DROP TRIGGER temp.public_update_fail"); }
    assert.equal((await update({ status: "applied" })).status, "applied");
  });
  await test("BUSY-release-next-write-reopen", async () => {
    const other = openSQLite(path);
    db.exec("PRAGMA busy_timeout=150");
    other.exec("BEGIN IMMEDIATE");
    try {
      const start = Date.now();
      await unchanged(() => update({ status: "applied" }), /locked|busy/i);
      assert(Date.now() - start >= 100 && Date.now() - start < 2000);
    } finally { other.exec("ROLLBACK"); other.close(); db.exec("PRAGMA busy_timeout=5000"); }
    await update({ status: "applied" });
    const reopened = openSQLite(path);
    try { assert.equal(row(reopened).status, "applied"); }
    finally { reopened.close(); }
  });
  const corruptions: [string, (row: Record<string, unknown>) => unknown][] = [
    ["null", () => null], ["array", () => []], ["extra-column", (row) => ({ ...row, extra: 1 })],
    ["missing-column", (row) => { delete row.health_output; return row; }],
    ["inherited-column", (row) => { const value = Object.create({ health_output: row.health_output }); delete row.health_output; return Object.assign(value, row); }],
    ["UUID", (row) => ({ ...row, source_node_id: "bad" })],
    ["NUL", (row) => ({ ...row, error_message: "bad\0text" })],
    ["timestamp", (row) => ({ ...row, finished_at: "bad" })],
    ["JSON", (row) => ({ ...row, validation_output: "{" })],
    ["status", (row) => ({ ...row, status: "bad" })],
    ["target", (row) => ({ ...row, target: "bad" })],
    ["source", (row) => ({ ...row, source: "bad" })],
    ["required-null", (row) => ({ ...row, correlation_id: null })],
    ["integer-number", (row) => ({ ...row, leadership_generation: 1 })],
    ["integer-int32-overflow", (row) => ({ ...row, snapshot_version: 2147483648n })],
    ["integer-int64-overflow", (row) => ({ ...row, leadership_generation: 9223372036854775808n })],
  ];
  for (const [name, transform] of corruptions) {
    await test(`returned-native-validation-after-statement-completion-${name}`, async () => {
      const seam = facade({ transform });
      let published: unknown;
      await assert.rejects(async () => { published = await update({ status: "applied" }, seam.connection); });
      assert.equal(published, undefined);
      assert.equal(seam.state.completions, 1);
      assert.equal(seam.state.finalizations, 1);
      assert.equal(row().status, "applied", "projection boundary cannot roll back an autocommitted statement");
      const reopened = openSQLite(path);
      try { assert.equal(row(reopened).status, "applied"); }
      finally { reopened.close(); }
    });
  }
  await test("own-statement-cache-preserved-and-RETURNING-exact-columns", async () => {
    const cached = db.query("SELECT * FROM main.apply_jobs WHERE id=?");
    db.exec("ALTER TABLE main.apply_jobs ADD COLUMN fixture_only TEXT");
    try {
      db.query("UPDATE main.apply_jobs SET fixture_only='opaque' WHERE id=?").run(id);
      assert.equal((await update({ status: "applied" })).status, "applied");
      assert.equal((cached.get(id) as Record<string, unknown>).fixture_only, "opaque");
    } finally { db.exec("ALTER TABLE main.apply_jobs DROP COLUMN fixture_only"); }
  });
  await test("actual-native-unsafe-integers-projection-error-durable-boundary", async () => {
    const unsafe = new Database(path, { strict: true, safeIntegers: false });
    try { await assert.rejects(update({ status: "applied" }, unsafe), /integer/); }
    finally { unsafe.close(); }
    assert.equal(row().status, "applied");
  });
  await test("closed-connection-Promise-error", async () => {
    const closed = openSQLite(path); closed.close();
    await unchanged(() => update({ status: "applied" }, closed), /closed|finalized/i);
  });
  db.query("DELETE FROM main.apply_jobs WHERE id=?").run(id);
  const final = await update({ status: "applied" }, db, originalID);
  assert.equal(final.id, originalID);
  console.log(`TOTAL ${cases} Bun cases; implicit COMMIT and caller transaction boundaries separate`);
} finally { db.close(); }
