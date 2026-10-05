import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { createInterface } from "node:readline";
import { Database, type SQLQueryBindings } from "bun:sqlite";
import { createSnapshot, checksumSnapshot, type ConfigurationSnapshot } from "../../../domain/src/snapshot.js";
import type { RevisionRecord, RevisionStore } from "../ports.js";
import { openSQLite } from "./open.js";
import { encodeTimestamp, decodeTimestamp } from "./timestamp.js";
import { getSQLiteRevisionRecord } from "./revision-record.js";
import { latestSQLiteRevisionRecord } from "./revision-latest-record.js";
import { createSQLiteRevisionRecord } from "./revision-create-record.js";

type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2) ? true : false : false;
type Expect<T extends true> = T;
type ParametersMatch = Expect<Identical<Parameters<typeof createSQLiteRevisionRecord>,
  [db: Database, ...args: Parameters<RevisionStore["create"]>]>>;
type ReturnMatches = Expect<Identical<ReturnType<typeof createSQLiteRevisionRecord>, Promise<RevisionRecord>>>;
type SnapshotMatches = Expect<Identical<Parameters<typeof createSQLiteRevisionRecord>[1], ConfigurationSnapshot>>;

const [mode, path, actor] = process.argv.slice(2);
const db = openSQLite(path);
const id = (n: number) => `aaaaaaaa-aaaa-aaaa-aaaa-${String(n).padStart(12, "0")}`;
const stamp = (seconds: bigint, nanos = 0) => encodeTimestamp({ kind: "finite", seconds, nanos })!;
const columns = ["id", "revision_number", "checksum", "snapshot", "actor_user_id", "created_at", "applied_at",
  "source", "source_primary_id", "source_node_id", "source_revision_id", "snapshot_content_hash",
  "snapshot_version", "replication_version", "leadership_generation"];
const nullable = ["actorUserId", "appliedAt", "sourcePrimaryId", "sourceNodeId", "sourceRevisionId",
  "snapshotContentHash", "snapshotVersion", "replicationVersion", "leadershipGeneration"];
const keys = ["id", "revisionNumber", "checksum", "snapshot", "source", "createdAt", ...nullable];
let assertions = 0, scenarios = 0;
const a = {
  ok(value: unknown) { assertions++; assert(value); },
  equal(actual: unknown, expected: unknown) { assertions++; assert.equal(actual, expected); },
  deep(actual: unknown, expected: unknown) { assertions++; assert.deepEqual(actual, expected); },
  async rejects(body: Promise<unknown>, expected?: RegExp | object) {
    assertions++;
    if (expected) await assert.rejects(body, expected); else await assert.rejects(body);
  },
};
function rows(connection = db) { return connection.query("SELECT * FROM main.config_revisions ORDER BY revision_number").all(); }
function settings(connection = db) { return connection.query("SELECT * FROM main.installation_settings ORDER BY id").all(); }
function state(connection = db) { return [rows(connection), settings(connection)]; }
const originalSettings = settings();
function seed(number = 1n, checksum = checksumSnapshot({ content: "seed" })) {
  db.query(`INSERT INTO main.config_revisions (id,revision_number,checksum,snapshot,created_at)
    VALUES (?,?,?,'{}',?)`).run(id(2), number, checksum, stamp(-1n, 999999999));
  return id(2);
}
function edit(column: string, value: SQLQueryBindings, key = id(2)) {
  db.query(`UPDATE main.config_revisions SET ${column}=? WHERE id=?`).run(value, key);
}
async function create(snapshot: unknown = { content: "new" }, user?: string, connection = db) {
  const promise = createSQLiteRevisionRecord(connection, snapshot as ConfigurationSnapshot, user);
  a.ok(promise instanceof Promise);
  const result = await promise;
  a.equal(connection.inTransaction, false);
  a.deep(Object.keys(result).sort(), [...keys].sort());
  return result;
}
async function rejected(snapshot: unknown = { content: "new" }, user?: string, connection = db, pattern?: RegExp | object) {
  const before = state();
  await a.rejects(create(snapshot, user, connection), pattern);
  a.deep(state(), before); a.equal(db.inTransaction, false);
}
async function test(name: string, body: () => void | Promise<void>) {
  try { await body(); a.equal(db.inTransaction, false); a.deep(settings(), originalSettings); }
  finally {
    if (db.inTransaction) db.exec("ROLLBACK");
    db.exec("PRAGMA ignore_check_constraints=OFF; DELETE FROM main.config_revisions");
  }
  scenarios++; console.log(`PASS ${name}`);
}
// Instance-local facade delegates real SQLite work. Finalizer faults occur AFTER
// actual native finalize: reproducible native-finalize failure remains unproved.
function facade(target: string, stage: "prepare" | "fetch" | "finalize" | "row" = "row",
  alter: (row: Record<string, unknown>) => void = () => {}, error = new Error(stage), after = () => {},
  alterResult: (result: Record<string, unknown>[]) => void = () => {}) {
  const events: string[] = [], owned: ReturnType<Database["prepare"]>[] = [];
  const connection = {
    get inTransaction() { return db.inTransaction; },
    transaction: db.transaction.bind(db),
    prepare(sql: string) {
      events.push(sql); a.ok(sql.includes("main.config_revisions"));
      const match = sql.startsWith(target);
      if (match && stage === "prepare") throw error;
      const statement = db.prepare<Record<string, unknown>, SQLQueryBindings[]>(sql);
      owned.push(statement);
      return {
        all(...bindings: SQLQueryBindings[]) {
          if (match && stage === "fetch") throw error;
          const result = statement.all(...bindings);
          if (match && stage === "row") for (const value of result) alter(value);
          if (match) { after(); alterResult(result); }
          return result;
        },
        finalize() {
          statement.finalize(); events.push("FINALIZED");
          if (match && stage === "finalize") throw error;
        },
      };
    },
  } as unknown as Database;
  return { connection, events, owned, error };
}
const dedupSQL = "SELECT id", maxSQL = "SELECT revision_number", insertSQL = "INSERT INTO";

if (mode === "child") {
  db.exec("PRAGMA busy_timeout=150");
  console.log("READY");
  const reader = createInterface({ input: process.stdin });
  for await (const command of reader) {
    if (command === "exit") break;
    if (command === "hold") { db.exec("BEGIN IMMEDIATE"); console.log("HELD"); }
    else if (command === "release") { db.exec("ROLLBACK"); console.log("RELEASED"); }
    else if (command.startsWith("create ")) {
      try {
        const result = await createSQLiteRevisionRecord(db, { content: command.slice(7) }, actor || undefined);
        console.log(`CREATED ${result.id} ${result.revisionNumber} ${result.actorUserId ?? "NULL"}`);
      } catch (error) {
        if (/locked|busy/i.test(String(error))) console.log("BUSY"); else throw error;
      }
    } else throw new Error(`Unknown command ${command}`);
  }
  reader.close();
} else if (mode === "unit") {
  await test("canonical helpers, no factory/options/clock widening, dedup before UUID and counter", () => {
    const source = readFileSync(new URL("./revision-create-record.ts", import.meta.url), "utf8");
    a.ok(source.includes('from "../../../domain/src/snapshot.js"'));
    a.ok(source.indexOf("createSnapshot(snapshot)") < source.indexOf("checksumSnapshot(normalized)"));
    a.ok(source.indexOf("checksumSnapshot(normalized)") < source.indexOf("new SQLiteWriter(db)"));
    a.ok(source.indexOf("if (existing)") < source.indexOf("ORDER BY revision_number DESC"));
    a.ok(source.indexOf("if (existing)") < source.indexOf("newUUID()"));
    a.equal(source.includes("createHash"), false); a.equal(source.includes("stableStringify"), false);
  });
  await test("initial one, canonical fifteen own keys, SQL NULLs, explicit native clock, valid actor", async () => {
    const before = Date.now();
    const result = await create({ b: 2, a: 1 }, actor);
    a.equal(result.revisionNumber, 1); a.equal(result.actorUserId, actor); a.equal(result.source, "ordinary");
    a.ok(result.createdAt instanceof Date); a.ok(result.createdAt.getTime() >= before);
    a.ok(result.createdAt.getTime() <= Date.now());
    const raw = rows()[0] as Record<string, unknown>;
    a.deep(Object.keys(raw).sort(), [...columns].sort());
    a.equal(raw.snapshot, '{"a":1,"b":2}'); a.equal(raw.actor_user_id, actor);
    a.equal(decodeTimestamp(raw.created_at as string).kind, "finite");
    for (const key of nullable.filter(key => key !== "actorUserId")) a.equal(result[key as keyof RevisionRecord], undefined);
    for (const key of columns.slice(6).filter(key => key !== "source")) a.equal(raw[key], null);
    a.deep(await getSQLiteRevisionRecord(db, result.id), result);
    a.deep(await latestSQLiteRevisionRecord(db), result); a.equal(rows().length, 1);
  });
  await test("deep sorted normalization, omitted object members, arrays, Date ISO and hash normalized twice", async () => {
    const input = { z: { y: 2, x: 1, absent: undefined, fn: () => 2, symbol: Symbol("x") },
      a: [undefined, null, () => 1, Symbol("y"), new Date(-1), { z: true, a: false }] };
    const expected = createSnapshot(input), hash = checksumSnapshot(expected);
    const result = await create(input);
    a.deep(result.snapshot, expected); a.equal(result.checksum, hash); a.ok(/^[0-9a-f]{64}$/.test(hash));
    a.equal(result.actorUserId, undefined); a.equal((rows()[0] as Record<string, unknown>).actor_user_id, null);
    a.equal((rows()[0] as Record<string, unknown>).snapshot, JSON.stringify(expected));
    a.equal((result.snapshot.a as unknown[])[4], new Date(-1).toISOString());
    input.z.x = 99; a.equal((result.snapshot.z as { x: number }).x, 1);
    a.equal((result.snapshot.z as Record<string, unknown>).absent, undefined);
    a.equal(Object.hasOwn(result.snapshot.z as object, "fn"), false);
    const same = await create({ a: expected.a, z: { x: 1, y: 2 } }, id(99));
    a.deep(same, result); a.equal(rows().length, 1);
  });
  await test("integer-looking keys distinguish canonical hash from JSON storage hash", async () => {
    const snapshot = { "2": "two", "10": "ten", "1": "one" };
    const result = await create(snapshot), normalized = createSnapshot(snapshot);
    a.equal(result.checksum, checksumSnapshot(normalized));
    a.equal(result.checksum === createHash("sha256").update(JSON.stringify(normalized)).digest("hex"), false);
    a.equal((rows()[0] as Record<string, unknown>).snapshot, JSON.stringify(normalized));
  });
  for (const value of [null, undefined, false, 0, "", NaN, Infinity, Symbol("top"), () => 1, [undefined, new Date(0)], new Date(0)]) {
    await test(`typed object is not runtime snapshot validator: ${String(value)}`, async () => {
      const result = await createSQLiteRevisionRecord(db, value as unknown as ConfigurationSnapshot);
      a.deep(result.snapshot, createSnapshot(value as unknown as ConfigurationSnapshot));
      a.equal(result.checksum, checksumSnapshot(createSnapshot(value as unknown as ConfigurationSnapshot)));
      a.equal(rows().length, 1);
    });
  }
  const cyclic: Record<string, unknown> = {}; cyclic.self = cyclic;
  for (const [name, value, errorClass] of [["invalid Date", { date: new Date(NaN) }, RangeError],
    ["cycles", cyclic, RangeError], ["BigInt", { value: 1n }, TypeError]] as const) {
    await test(`${name} rejects before SQL, actor, UUID and caller transaction rejection`, async () => {
      const boundary = { get inTransaction(): boolean { throw new Error("SQL boundary reached"); } } as Database;
      await a.rejects(createSQLiteRevisionRecord(boundary, value, id(99)), errorClass);
      for (const nesting of ["BEGIN", "SAVEPOINT caller"]) {
        db.exec(nesting); seed();
        const pending = state();
        await a.rejects(createSQLiteRevisionRecord(db, value, id(99)), errorClass);
        a.equal(db.inTransaction, true); a.deep(state(), pending); db.exec("ROLLBACK");
      }
    });
  }
  await test("source domain exceptions retain identity before checksum or database", async () => {
    const error = new Error("domain getter"), snapshot = { get value() { throw error; } };
    await rejected(snapshot, id(99), db, error);
  });
  for (const start of [-2147483648n, -9n, -1n, 0n, 2147483646n]) {
    await test(`native greatest plus one including negative and last int32: ${start}`, async () => {
      seed(start); const result = await create(); a.equal(result.revisionNumber, Number(start + 1n));
      a.equal(rows().length, 2); a.equal((rows()[1] as Record<string, unknown>).revision_number, start + 1n);
    });
  }
  await test("overflow rejects, but matching checksum returns max stored row without allocating", async () => {
    seed(2147483647n); await rejected();
    const before = state(), existing = await create({ content: "seed" }, id(99));
    a.equal(existing.revisionNumber, 2147483647); a.deep(state(), before);
  });
  await test("native greatest counter ordering, not timestamp/id/lexical ordering", async () => {
    seed(9n);
    db.query("INSERT INTO main.config_revisions (id,revision_number,checksum,snapshot,created_at) VALUES (?,10,'ten','{}','0')").run(id(3));
    edit("created_at", "2"); const result = await create(); a.equal(result.revisionNumber, 11);
  });
  for (const snapshot of [null, false, 0, "", { generic: [1, "raw"] }]) {
    await test(`dedup returns stored JSON without revalidation, original actor and all leadership fields: ${JSON.stringify(snapshot)}`, async () => {
      seed(); edit("snapshot", JSON.stringify(snapshot)); edit("source", "sync"); edit("actor_user_id", actor);
      for (const key of ["source_primary_id", "source_node_id", "source_revision_id"]) edit(key, id(7));
      edit("snapshot_content_hash", "native-hash"); edit("snapshot_version", -2147483648n);
      edit("replication_version", 2147483647n); edit("leadership_generation", 9007199254740993n);
      edit("applied_at", stamp(-1n, 999999999));
      const before = state(), f = facade(dedupSQL), result = await create({ content: "seed" }, id(99), f.connection);
      a.deep(result, await getSQLiteRevisionRecord(db, id(2))); a.deep(result.snapshot, snapshot);
      a.equal(result.actorUserId, actor); a.equal(result.leadershipGeneration, Number(9007199254740993n));
      a.equal(result.createdAt.getTime(), -1); a.equal(result.appliedAt!.getTime(), -1);
      a.equal(f.events.length, 2); a.ok(f.events[0].includes("LIMIT 1")); a.equal(f.events[1], "FINALIZED");
      a.deep(state(), before);
      const next = await create({ content: "seed" });
      a.equal(next.createdAt === result.createdAt, false);
      if (snapshot !== null && typeof snapshot === "object") a.equal(next.snapshot === result.snapshot, false);
    });
  }
  for (const infinity of ["0", "2"]) await test(`dedup public infinity becomes Invalid Date; native bytes ${infinity} unchanged`, async () => {
    seed(); edit("created_at", infinity); edit("applied_at", infinity);
    const before = state(), result = await create({ content: "seed" });
    a.ok(Number.isNaN(result.createdAt.getTime())); a.ok(Number.isNaN(result.appliedAt!.getTime()));
    for (const key of nullable.filter(key => key !== "appliedAt")) a.equal(result[key as keyof RevisionRecord], undefined);
    a.deep(state(), before);
  });
  for (const user of [id(99), "bad-actor", ""]) await test(`new actor constraint failure fully rolls back: ${user}`, async () => {
    await rejected({ fresh: true }, user); a.equal(rows().length, 0);
    const result = await create({ fresh: true }, actor); a.equal(result.revisionNumber, 1);
  });
  await test("real uniqueness constraint failure rolls back, no counter consumed", async () => {
    seed();
    db.exec(`CREATE TRIGGER main.create_unique BEFORE INSERT ON config_revisions BEGIN
      INSERT INTO config_revisions (id,revision_number,checksum,snapshot,created_at) VALUES ('${id(8)}',NEW.revision_number,'conflict','{}','0'); END`);
    try { await rejected(); a.equal(rows().length, 1); }
    finally { db.exec("DROP TRIGGER main.create_unique"); }
    a.equal((await create()).revisionNumber, 2);
  });
  await test("captured RETURNING survives AFTER edits; finalization precedes projection, not reread", async () => {
    db.exec("CREATE TRIGGER main.create_after AFTER INSERT ON config_revisions BEGIN UPDATE config_revisions SET snapshot='null',source='import' WHERE id=NEW.id; END");
    try {
      const f = facade(insertSQL), result = await create({ capture: true }, undefined, f.connection);
      a.deep(result.snapshot, { capture: true }); a.equal(result.source, "ordinary");
      const stored = await getSQLiteRevisionRecord(db, result.id);
      a.equal(stored!.snapshot, null); a.equal(stored!.source, "import");
      a.equal(f.events.filter(sql => sql.startsWith("SELECT")).length, 2);
      a.equal(f.events.at(-1), "FINALIZED");
    } finally { db.exec("DROP TRIGGER main.create_after"); }
  });
  const faults: [string, unknown][] = [
    ["id", "bad"], ["revision_number", 2147483648n], ["revision_number", 1], ["checksum", 1],
    ["snapshot", "{"], ["snapshot", null], ["actor_user_id", "bad"], ["created_at", "bad"],
    ["applied_at", "bad"], ["source", "other"], ["source_primary_id", "bad"], ["source_node_id", "bad"],
    ["source_revision_id", "bad"], ["snapshot_content_hash", "nul\0"], ["snapshot_version", 2147483648n],
    ["replication_version", -2147483649n], ["leadership_generation", 9223372036854775808n],
  ];
  for (const target of [dedupSQL, insertSQL]) {
    for (const [key, value] of faults) await test(`${target} invalid returned ${key} ${String(value)} rolls back before COMMIT`, async () => {
      if (target === dedupSQL) seed();
      const f = facade(target, "row", row => { row[key] = value; });
      await rejected({ content: target === dedupSQL ? "seed" : "new" }, undefined, f.connection);
      a.equal(f.events.at(-1), "FINALIZED"); a.equal(rows().length, target === dedupSQL ? 1 : 0);
    });
    for (const shape of ["missing", "extra"]) await test(`${target} ${shape} columns reject`, async () => {
      if (target === dedupSQL) seed();
      const f = facade(target, "row", row => { if (shape === "missing") delete row.source; else row.extra = 1; });
      await rejected({ content: target === dedupSQL ? "seed" : "new" }, undefined, f.connection);
    });
  }
  for (const value of [2147483648n, -2147483649n, 9223372036854775807n, 1, "1"]) {
    await test(`invalid native counter ${String(value)} never silently narrows`, async () => {
      seed(); const f = facade(maxSQL, "row", row => { row.revision_number = value; });
      await rejected(undefined, undefined, f.connection);
    });
  }
  for (const target of [dedupSQL, maxSQL, insertSQL]) for (const stage of ["prepare", "fetch", "finalize"] as const) {
    await test(`${target} ${stage} error identity, owned cleanup, no caller cache damage`, async () => {
      const cached = db.query("SELECT 17 AS value"), f = facade(target, stage);
      await rejected(undefined, undefined, f.connection, f.error);
      if (stage !== "prepare") a.ok(f.events.includes("FINALIZED"));
      a.deep(cached.get(), { value: 17n }); a.equal((await create()).revisionNumber, 1);
    });
  }
  for (const nesting of ["BEGIN", "SAVEPOINT caller"]) await test(`${nesting} rejected with caller pending row/cache intact`, async () => {
    db.exec(nesting); seed(); const pending = state(), cached = db.query("SELECT count(*) AS n FROM main.config_revisions");
    await a.rejects(createSQLiteRevisionRecord(db, { content: "seed" }, id(99)), /Nested write/);
    a.equal(db.inTransaction, true); a.deep(state(), pending); a.deep(cached.get(), { n: 1n });
    const other = openSQLite(path);
    try { a.equal(rows(other).length, 0); } finally { other.close(); }
    db.exec("ROLLBACK"); a.equal(rows().length, 0);
  });
  await test("TEMP shadow ignored and caller cache remains owned", async () => {
    db.exec("CREATE TEMP TABLE config_revisions (fake TEXT); INSERT INTO temp.config_revisions VALUES ('shadow')");
    try {
      const cached = db.query("SELECT * FROM temp.config_revisions");
      const result = await create(); a.equal(result.revisionNumber, 1);
      a.deep(cached.all(), [{ fake: "shadow" }]); a.equal(rows().length, 1);
    } finally { db.exec("DROP TABLE temp.config_revisions"); }
  });
  await test("missing settings remain absent; no initialization or pointer writes", async () => {
    db.exec("DELETE FROM main.installation_settings");
    try {
      const record = await create({ withoutSettings: true });
      a.equal(settings().length, 0); a.equal(record.revisionNumber, 1);
      a.deep(await create({ withoutSettings: true }), record);
      a.equal(settings().length, 0);
    } finally {
      for (const saved of originalSettings as Record<string, SQLQueryBindings>[]) {
        const names = Object.keys(saved);
        db.query(`INSERT INTO main.installation_settings (${names.join(",")}) VALUES (${names.map(() => "?").join(",")})`)
          .run(...Object.values(saved));
      }
    }
  });
  for (const number of [2147483648n, -2147483649n, 9223372036854775807n]) {
    await test(`actual corrupt native counter ${number} rejected in max and dedup`, async () => {
      seed(); db.exec("PRAGMA ignore_check_constraints=ON"); edit("revision_number", number);
      await rejected(); await rejected({ content: "seed" });
      a.equal((rows()[0] as Record<string, unknown>).revision_number, number);
    });
  }
  for (const target of [dedupSQL, insertSQL]) {
    for (const raw of [null, [], "not-row"]) await test(`${target} invalid row shape ${JSON.stringify(raw)}`, async () => {
      if (target === dedupSQL) seed();
      const f = facade(target, "row", () => {}, new Error("unused"), () => {}, result => {
        result[0] = raw as unknown as Record<string, unknown>;
      });
      await rejected({ content: target === dedupSQL ? "seed" : "new" }, undefined, f.connection);
    });
  }
  await test("missing RETURNING row rejects instead of publishing empty success", async () => {
    const f = facade(insertSQL, "row", () => {}, new Error("unused"), () => {}, result => { result.length = 0; });
    await rejected(undefined, undefined, f.connection, /returned no row/);
  });
  await test("projection happens only after owned native cursor finalized", async () => {
    const f = facade(insertSQL, "row", row => {
      const value = row.snapshot;
      Object.defineProperty(row, "snapshot", { enumerable: true, get() {
        a.equal(f.events.at(-1), "FINALIZED"); return value;
      } });
    });
    a.deep((await create({ finalized: true }, undefined, f.connection)).snapshot, { finalized: true });
    a.equal(rows().length, 1);
  });
  await test("closed connection rejects; invalid domain wins; independent reopen succeeds", async () => {
    const closed = openSQLite(path); closed.close();
    await a.rejects(createSQLiteRevisionRecord(closed, {}));
    await a.rejects(createSQLiteRevisionRecord(closed, { bad: 1n }), TypeError);
    const reopened = openSQLite(path);
    try { a.equal((await create({}, undefined, reopened)).revisionNumber, 1); }
    finally { reopened.close(); }
  });
  for (const dedup of [false, true]) await test(`real deferred FK COMMIT failure ${dedup ? "dedup" : "insert"}, no publication/full rollback/reopen/recovery`, async () => {
    if (dedup) seed();
    db.exec("CREATE TABLE main.create_guard(id TEXT REFERENCES config_revisions(id) DEFERRABLE INITIALLY DEFERRED)");
    if (!dedup) db.exec(`CREATE TRIGGER main.create_commit AFTER INSERT ON config_revisions BEGIN INSERT INTO create_guard VALUES ('${id(99)}'); END`);
    const before = state(), changes = (db.query("SELECT total_changes() AS n").get() as { n: bigint }).n;
    // A SELECT cannot itself violate deferred FKs. For the dedup branch only, an
    // instance-local real SELECT facade inserts a deferred violation inside the
    // actual owned transaction; COMMIT is native, never mocked or replaced.
    const f = facade(dedupSQL, "row", () => {}, new Error("unused"), () => {
      if (dedup) db.exec(`INSERT INTO main.create_guard VALUES ('${id(99)}')`);
    });
    let published: RevisionRecord | undefined;
    try {
      await a.rejects(createSQLiteRevisionRecord(dedup ? f.connection : db,
        { content: dedup ? "seed" : "new" }).then(record => { published = record; }), /FOREIGN KEY/i);
      a.equal(published, undefined); a.equal(db.inTransaction, false); a.deep(state(), before);
      a.deep(db.query("SELECT * FROM main.create_guard").all(), []);
      a.equal((db.query("SELECT total_changes() AS n").get() as { n: bigint }).n - changes, dedup ? 1n : 2n);
      const reopened = openSQLite(path);
      try { a.deep(state(reopened), before); } finally { reopened.close(); }
    } finally {
      if (!dedup) db.exec("DROP TRIGGER main.create_commit");
      db.exec("DROP TABLE main.create_guard");
    }
    const result = await create({ content: dedup ? "seed" : "new" });
    a.equal(result.revisionNumber, 1); a.equal(rows().length, 1);
  });
  a.deep(settings(), originalSettings);
  a.equal(rows().length, 0);
  console.log(`COMPLETE create assertions=${assertions} scenarios=${scenarios}`);
} else throw new Error(`Unknown mode ${mode}`);
if (db.inTransaction) db.exec("ROLLBACK");
db.close();
