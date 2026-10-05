import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { Database, type SQLQueryBindings } from "bun:sqlite";
import type { RevisionRecord, RevisionStore } from "../ports.js";
import type { ConfigurationSnapshot } from "../../../domain/src/snapshot.js";
import { createSQLiteRevisionStore } from "./revision-store.js";
import { openSQLite } from "./open.js";
import { encodeTimestamp } from "./timestamp.js";

type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2) ? true : false : false;
type Expect<T extends true> = T;
type FactoryInput = Expect<Identical<Parameters<typeof createSQLiteRevisionStore>, [db: Database]>>;
type FactoryOutput = Expect<Identical<ReturnType<typeof createSQLiteRevisionStore>, RevisionStore>>;
type SnapshotInput = Expect<Identical<Parameters<RevisionStore["create"]>,
  [snapshot: ConfigurationSnapshot, actorUserId?: string]>>;
type GetInput = Expect<Identical<Parameters<RevisionStore["get"]>, [id: string]>>;
type LatestInput = Expect<Identical<Parameters<RevisionStore["latest"]>, []>>;
type ApplyInput = Expect<Identical<Parameters<RevisionStore["markApplied"]>, [id: string, appliedAt?: Date]>>;
type Outputs = Expect<Identical<{
  [K in "create" | "markApplied"]: ReturnType<RevisionStore[K]>
}["create" | "markApplied"], Promise<RevisionRecord>>>;
type ReadOutputs = Expect<Identical<{
  [K in "get" | "latest"]: ReturnType<RevisionStore[K]>
}["get" | "latest"], Promise<RevisionRecord | undefined>>>;
function canonicalChecks(store: ReturnType<typeof createSQLiteRevisionStore>) {
  // @ts-expect-error The snapshot remains required.
  store.create();
  // @ts-expect-error No hook, dependency or options injection.
  createSQLiteRevisionStore(db, {});
  // @ts-expect-error No close method or connection lifetime ownership.
  store.close();
  // @ts-expect-error No number coercion at the public ID boundary.
  store.get(1);
  // @ts-expect-error No factory-specific latest filters.
  store.latest({});
  // @ts-expect-error Dates retain their canonical type.
  store.markApplied("id", "today");
}
void canonicalChecks;

const [mode, path, actor] = process.argv.slice(2);
const db = openSQLite(path);
const store = createSQLiteRevisionStore(db);
const other = createSQLiteRevisionStore(db);
const methods = ["create", "get", "latest", "markApplied"];
const keys = ["id", "revisionNumber", "checksum", "snapshot", "actorUserId", "source",
  "sourcePrimaryId", "sourceNodeId", "sourceRevisionId", "snapshotContentHash", "snapshotVersion",
  "replicationVersion", "leadershipGeneration", "createdAt", "appliedAt"];
const id = (n: number) => `aaaaaaaa-aaaa-aaaa-aaaa-${String(n).padStart(12, "0")}`;
const stamp = (millis: number) => {
  const ms = BigInt(millis), remainder = ((ms % 1000n) + 1000n) % 1000n;
  return encodeTimestamp({ kind: "finite", seconds: (ms - remainder) / 1000n,
    nanos: Number(remainder) * 1_000_000 })!;
};
let assertions = 0, scenarios = 0;
const a = {
  ok(value: unknown) { assertions++; assert(value); },
  equal(actual: unknown, expected: unknown) { assertions++; assert.equal(actual, expected); },
  deep(actual: unknown, expected: unknown) { assertions++; assert.deepEqual(actual, expected); },
  async rejects(pending: Promise<unknown>, expected?: RegExp | ((error: unknown) => boolean)) {
    assertions++;
    if (expected) await assert.rejects(pending, expected); else await assert.rejects(pending);
  },
};
async function promised<T>(pending: Promise<T>): Promise<T> {
  a.ok(pending instanceof Promise);
  return pending;
}
function record(value: RevisionRecord | undefined): asserts value is RevisionRecord {
  a.ok(value);
  a.deep(Object.keys(value!).sort(), [...keys].sort());
  a.ok(value!.createdAt instanceof Date);
}
function rows(connection = db) {
  return connection.query<Record<string, SQLQueryBindings>, []>("SELECT * FROM main.config_revisions ORDER BY revision_number").all();
}
function settings(connection = db) {
  return connection.query<Record<string, SQLQueryBindings>, []>("SELECT * FROM main.installation_settings ORDER BY id").all();
}
const originalSettings = settings();
function restoreSettings() {
  db.exec("DELETE FROM main.installation_settings");
  for (const row of originalSettings) {
    const names = Object.keys(row);
    db.query(`INSERT INTO main.installation_settings (${names.join(",")}) VALUES (${names.map(() => "?").join(",")})`)
      .run(...names.map((name) => row[name]));
  }
}
async function test(name: string, body: () => void | Promise<void>) {
  try { await body(); }
  finally {
    if (db.inTransaction) db.exec("ROLLBACK");
    db.exec("DELETE FROM main.config_revisions");
    restoreSettings();
  }
  a.equal(db.inTransaction, false);
  a.deep(settings(), originalSettings);
  scenarios++;
  console.log(`PASS ${name}`);
}
const calls = (s: RevisionStore) => [
  () => s.create({ content: "new" }), () => s.get(id(2)),
  () => s.latest(), () => s.markApplied(id(2), new Date(123)),
];

try {
  if (mode === "persist") {
    const value = await store.create({ z: 1, a: [3, null] }, actor);
    await store.markApplied(value.id, new Date(123));
    console.log(`PERSIST ${value.id}`);
  } else if (mode === "reopen") {
    const value = await store.latest();
    record(value);
    a.equal(value.revisionNumber, 1);
    a.deep(value.snapshot, { a: [3, null], z: 1 });
    a.equal(value.appliedAt?.getTime(), 123);
    a.equal(value.actorUserId, actor);
    a.deep(await store.get(value.id), value);
    a.equal(settings()[0].current_applied_revision_id, value.id);
    console.log(`COMPLETE reopen assertions=${assertions} scenarios=1`);
  } else {
    a.ok(store !== other);
    a.deep(Object.keys(store).sort(), [...methods].sort());
    await test("construction has no connection access, SQL, lifetime or singleton policy", async () => {
      let accesses = 0;
      const sentinel = new Error("unexpected connection access");
      const traced = new Proxy({} as Database, { get() { accesses++; throw sentinel; } });
      const bound = createSQLiteRevisionStore(traced);
      a.equal(accesses, 0);
      a.deep(Object.keys(bound).sort(), [...methods].sort());
      // Getter errors cross its async boundary; no synchronous error escapes.
      for (const call of calls(bound)) {
        await a.rejects(promised(call()), (error) => error === sentinel);
      }
      const closed = new Database(":memory:");
      closed.close();
      const closedStore = createSQLiteRevisionStore(closed);
      for (const call of calls(closedStore)) await a.rejects(promised(call()));
      a.equal(await other.get("missing"), undefined);
    });

    await test("source evidence: async primitives return directly without a new Promise seam", () => {
      // Promise identity is not externally observable through async primitives;
      // verify direct forwarding without adding a production injection surface.
      const source = readFileSync(new URL("./revision-store.ts", import.meta.url), "utf8");
      for (const [name, args, helper, bound] of [
        ["create", "snapshot, actorUserId", "createSQLiteRevisionRecord", "db, snapshot, actorUserId"],
        ["latest", "", "latestSQLiteRevisionRecord", "db"],
        ["markApplied", "id, appliedAt", "markAppliedSQLiteRevisionRecord", "db, id, appliedAt"],
      ]) {
        a.ok(source.includes(`${name}(${args}) {\n      return ${helper}(${bound});\n    }`));
        a.equal(source.includes(`async ${name}(`), false);
      }
      a.ok(source.includes("async get(id) {"));
      a.ok(source.includes("return getSQLiteRevisionRecord(db, id);"));
    });

    await test("canonical create-get-latest-apply, independent fields and durable bytes", async () => {
      a.equal(await promised(store.get("missing")), undefined);
      a.equal(await promised(store.latest()), undefined);
      const before = settings();
      const low = Date.now();
      const created = await promised(store.create({ z: 2, a: [3, undefined], omitted: undefined }, actor));
      const high = Date.now();
      record(created);
      a.equal(created.revisionNumber, 1);
      a.equal(created.source, "ordinary");
      a.equal(created.actorUserId, actor);
      a.equal(created.appliedAt, undefined);
      a.deep(created.snapshot, { a: [3, null], z: 2 });
      a.equal(created.checksum, createHash("sha256").update('{"a":[3,null],"z":2}').digest("hex"));
      a.ok(created.createdAt.getTime() >= low && created.createdAt.getTime() <= high);
      for (const key of keys.filter((key) => key.startsWith("source") && key !== "source")) {
        a.equal(created[key as keyof RevisionRecord], undefined);
      }
      for (const key of ["snapshotContentHash", "snapshotVersion", "replicationVersion", "leadershipGeneration"] as const) {
        a.equal(created[key], undefined);
      }
      a.deep(settings(), before);
      a.deep(await promised(other.get(created.id)), created);
      a.deep(await promised(other.latest()), created);
      const applied = await promised(store.markApplied(created.id, new Date(-123)));
      record(applied);
      a.equal(applied.appliedAt?.getTime(), -123);
      a.equal(applied.createdAt.getTime(), created.createdAt.getTime());
      a.equal(settings()[0].current_applied_revision_id, created.id);
      a.equal(settings()[0].updated_at, stamp(-123));
      a.equal(settings()[0].current_desired_revision_id, before[0].current_desired_revision_id);
      a.equal(rows()[0].snapshot, '{"a":[3,null],"z":2}');
      a.equal(rows()[0].applied_at, stamp(-123));
      a.deep(await other.get(created.id), applied);
      const appliedSettings = settings();
      const reused = await other.create({ a: [3, null], z: 2 });
      a.deep(settings(), appliedSettings);
      a.equal(reused.id, created.id);
      a.equal(reused.actorUserId, actor);
      a.equal(reused.appliedAt?.getTime(), -123);
      a.equal(rows().length, 1);
      const second = await store.create({ content: "next" });
      record(second);
      a.equal(second.actorUserId, undefined);
      a.equal(second.revisionNumber, 2);
      a.equal((await other.latest())?.id, second.id);
    });

    for (const explicit of [false, true]) {
      await test(`omitted and undefined defaults are captured by primitives: ${explicit}`, async () => {
        const before = settings();
        const value = explicit ? await store.create({ default: true }, undefined) : await store.create({ default: true });
        a.equal(value.actorUserId, undefined);
        a.deep(settings(), before);
        const dedup = await store.create({ default: true }, actor);
        a.equal(dedup.id, value.id);
        a.equal(dedup.actorUserId, undefined);
        a.deep(settings(), before);
        // Construction happened earlier: invocation, not factory time, owns Date.
        await Bun.sleep(12);
        const low = Date.now();
        const result = explicit ? await other.markApplied(value.id, undefined) : await other.markApplied(value.id);
        const high = Date.now();
        a.ok(result.appliedAt!.getTime() >= low && result.appliedAt!.getTime() <= high);
        a.equal(settings()[0].updated_at, stamp(result.appliedAt!.getTime()));
      });
    }

    await test("original snapshot getters and Date receiver survive without factory copies", async () => {
      const input: ConfigurationSnapshot = {};
      let reads = 0, receiver: unknown;
      Object.defineProperty(input, "value", { enumerable: true, get() { receiver = this; reads++; return 7; } });
      const created = await store.create(input);
      a.equal(receiver, input);
      // Canonical stableStringify reads a property once for filtering, once for output.
      a.equal(reads, 2);
      a.deep(created.snapshot, { value: 7 });
      a.equal(Object.getOwnPropertyDescriptor(input, "value")?.get !== undefined, true);
      const date = new Date(456);
      let dateReads = 0;
      date.toISOString = function () { a.equal(this, date); dateReads++; return "1970-01-01T00:00:00.456Z"; };
      const applied = await store.markApplied(created.id, date);
      a.equal(dateReads, 1);
      a.equal(applied.appliedAt?.getTime(), 456);
      const sentinel = new Error("original getter");
      const bad = { get value() { throw sentinel; } };
      const before = rows();
      await a.rejects(promised(store.create(bad)), (error) => error === sentinel);
      date.toISOString = function () { a.equal(this, date); throw sentinel; };
      await a.rejects(promised(store.markApplied(created.id, date)), (error) => error === sentinel);
      a.deep(rows(), before);
    });

    await test("toJSON is not a factory cloning hook; original snapshot stays mutable", async () => {
      let jsonCalls = 0;
      const input = { value: 1, toJSON() { jsonCalls++; throw new Error("unexpected JSON clone"); } };
      const first = await store.create(input);
      a.equal(jsonCalls, 0);
      a.deep(first.snapshot, { value: 1 });
      a.equal(Object.isFrozen(input), false);
      input.value = 2;
      const second = await other.create(input);
      a.deep(second.snapshot, { value: 2 });
      a.equal(second.revisionNumber, 2);
      a.equal(jsonCalls, 0);
      a.deep(first.snapshot, { value: 1 });
    });

    for (const [method, stage] of [
      ["get", "prepare"], ["get", "fetch"], ["get", "finalize"],
      ["latest", "prepare"], ["create", "prepare"], ["markApplied", "prepare"],
    ] as const) {
      await test(`local connection ${method}/${stage} preserves original error identity`, async () => {
        const value = await store.create({ fault: true });
        const sentinel = new Error(`${method}/${stage}`);
        const before = [rows(), settings()];
        let finalized = 0;
        const facade = {
          get inTransaction() { return db.inTransaction; },
          transaction: db.transaction.bind(db),
          prepare(sql: string) {
            a.ok(sql.includes("main.config_revisions"));
            if (stage === "prepare") throw sentinel;
            const statement = db.prepare(sql);
            return {
              get(...args: SQLQueryBindings[]) {
                a.deep(args, [value.id]);
                if (stage === "fetch") throw sentinel;
                return statement.get(...args);
              },
              finalize() {
                // Fault follows actual native finalize, not a native-finalizer proof.
                statement.finalize(); finalized++;
                if (stage === "finalize") throw sentinel;
              },
            };
          },
        } as unknown as Database;
        const bound = createSQLiteRevisionStore(facade);
        const invoke = { get: () => bound.get(value.id), latest: () => bound.latest(),
          create: () => bound.create({ fault: "new" }), markApplied: () => bound.markApplied(value.id) };
        await a.rejects(promised(invoke[method]()), (error) => error === sentinel);
        a.equal(finalized, stage === "prepare" ? 0 : 1);
        a.deep([rows(), settings()], before);
        a.equal(db.inTransaction, false);
        a.deep(await store.get(value.id), value);
      });
    }

    await test("caller BEGIN and savepoint reads see pending revisions, writes reject intact", async () => {
      const reader = openSQLite(path);
      try {
        const before = settings();
        db.exec("BEGIN; SAVEPOINT caller");
        db.query(`INSERT INTO main.config_revisions (id,revision_number,checksum,snapshot,created_at)
          VALUES (?,9,'pending','{"pending":true}',?)`).run(id(9), stamp(0));
        const pending = await promised(store.get(id(9)));
        record(pending);
        a.equal(pending.revisionNumber, 9);
        a.deep(pending.snapshot, { pending: true });
        a.equal((await promised(other.latest()))?.id, id(9));
        a.equal(await createSQLiteRevisionStore(reader).get(id(9)), undefined);
        a.equal(await createSQLiteRevisionStore(reader).latest(), undefined);
        const state = rows();
        await a.rejects(promised(store.create({ valid: true })), /Nested write transaction/);
        await a.rejects(promised(other.markApplied(id(9))), /Nested write transaction/);
        const sentinel = new Error("domain failure precedes caller rejection");
        await a.rejects(store.create({ get value() { throw sentinel; } }), (error) => error === sentinel);
        await a.rejects(store.markApplied(id(9), new Date(NaN)), (error) => error instanceof RangeError);
        a.deep(rows(), state);
        a.deep(settings(), before);
        a.equal(db.inTransaction, true);
        db.exec("ROLLBACK TO caller; RELEASE caller");
        a.equal(await store.get(id(9)), undefined);
        a.equal(await other.latest(), undefined);
        a.equal(db.inTransaction, true);
        db.exec("ROLLBACK");
      } finally { reader.close(); }
    });

    await test("caller cached statement and TEMP shadows are not a factory cache", async () => {
      const cached = db.query("SELECT 42 AS value");
      db.exec("CREATE TEMP TABLE config_revisions (id TEXT); CREATE TEMP TABLE installation_settings (id TEXT)");
      try {
        const value = await store.create({ fresh: true }, actor);
        await other.markApplied(value.id, new Date(1));
        record(await store.get(value.id));
        record(await other.latest());
        a.equal((cached.get() as { value: bigint }).value, 42n);
        a.equal(db.query<{ n: bigint }, []>("SELECT count(*) AS n FROM temp.config_revisions").get()!.n, 0n);
        a.equal(db.query<{ n: bigint }, []>("SELECT count(*) AS n FROM temp.installation_settings").get()!.n, 0n);
      } finally { db.exec("DROP TABLE temp.config_revisions; DROP TABLE temp.installation_settings"); }
    });

    await test("get and latest return fresh JSON and Dates, reopen preserves native rows", async () => {
      const value = await store.create({ nested: { value: 1 } });
      await store.markApplied(value.id, new Date(100));
      const raw = rows();
      const first = (await store.get(value.id))!;
      (first.snapshot.nested as { value: number }).value = 99;
      first.createdAt.setTime(999);
      first.appliedAt!.setTime(999);
      const second = (await other.latest())!;
      a.deep(second.snapshot, { nested: { value: 1 } });
      a.equal(second.appliedAt?.getTime(), 100);
      a.equal(second.createdAt.getTime(), value.createdAt.getTime());
      a.ok(first.snapshot !== second.snapshot);
      a.ok(first.createdAt !== second.createdAt);
      a.ok(first.appliedAt !== second.appliedAt);
      const reopened = openSQLite(path);
      try {
        a.deep(rows(reopened), raw);
        a.deep(await createSQLiteRevisionStore(reopened).get(value.id), second);
        a.deep(await createSQLiteRevisionStore(reopened).latest(), second);
      } finally { reopened.close(); }
      a.deep(rows(), raw);
    });

    await test("errors are not empty results; no factory error replacement or settings initialization", async () => {
      const before = settings();
      await a.rejects(promised(store.markApplied("missing")), /Revision not found/);
      a.equal(await store.get("missing"), undefined);
      a.equal(await store.latest(), undefined);
      a.deep(settings(), before);
      const value = await store.create({ malformed: true });
      db.exec("PRAGMA ignore_check_constraints=ON");
      try { db.query("UPDATE main.config_revisions SET snapshot='not-json' WHERE id=?").run(value.id); }
      finally { db.exec("PRAGMA ignore_check_constraints=OFF"); }
      await a.rejects(promised(store.get(value.id)), (error) => error instanceof SyntaxError);
      await a.rejects(promised(store.latest()), (error) => error instanceof SyntaxError);
      a.deep(settings(), before);
      db.exec("DELETE FROM main.config_revisions; DELETE FROM main.installation_settings");
      a.equal(settings().length, 0);
      a.equal(await other.latest(), undefined);
      await a.rejects(promised(store.markApplied("missing")), /Revision not found/);
      a.equal(settings().length, 0);
      const created = await other.create({ settings: "absent" });
      a.equal(settings().length, 0);
      record(await store.get(created.id));
      a.equal(settings().length, 0);
      await store.markApplied(created.id, new Date(2));
      a.equal(settings().length, 1);
      a.equal(settings()[0].current_applied_revision_id, created.id);
    });
    console.log(`COMPLETE store assertions=${assertions} scenarios=${scenarios}`);
  }
} finally {
  if (db.inTransaction) db.exec("ROLLBACK");
  db.close();
}
