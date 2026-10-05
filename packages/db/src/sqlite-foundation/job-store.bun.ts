import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import { openSQLite } from "./open.js";
import { createSQLiteJobStore } from "./job-store.js";
import { claimNextSQLiteJobRecord } from "./job-claim-record.js";
import type { JobEnqueueInput, JobRecord, JobStore } from "../ports.js";

type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2)
      ? true : false : false;
type Expect<T extends true> = T;
type FactoryInput = Expect<Identical<Parameters<typeof createSQLiteJobStore>, [db: Database]>>;
type FactoryOutput = Expect<Identical<ReturnType<typeof createSQLiteJobStore>, JobStore>>;
type PromiseMethods = Expect<Identical<{
  [K in keyof JobStore]: ReturnType<JobStore[K]> extends Promise<unknown> ? true : false
}[keyof JobStore], true>>;
type ClaimArguments = Expect<Identical<Parameters<JobStore["claimNext"]>,
  [target?: JobRecord["target"]]>>;
type RecoveryArguments = Expect<Identical<Parameters<JobStore["recoverStale"]>,
  [leaseMs: number, now?: Date]>>;
// Compile-only negative checks. Do not execute invalid calls or create fixtures.
function canonicalChecks(store: ReturnType<typeof createSQLiteJobStore>, job: JobEnqueueInput) {
  const get: Promise<JobRecord | undefined> = store.get("id");
  const claim: Promise<JobRecord | undefined> = store.claimNext();
  const recover: Promise<number> = store.recoverStale(1);
  const update: Promise<JobRecord> = store.update("id", { healthOutput: {} });
  const list: Promise<JobRecord[]> = store.list();
  const enqueue: Promise<JobRecord> = store.enqueue(job);
  const conditional: Promise<JobRecord | undefined> = store.enqueueIfNotActive(job);
  // @ts-expect-error Required canonical lease must not become optional.
  store.recoverStale();
  // @ts-expect-error No public factory dependency or hook options.
  createSQLiteJobStore(db, { hook() {} });
  // @ts-expect-error No extra canonical method.
  store.close();
  // @ts-expect-error No widened claim target.
  store.claimNext("other");
  void [get, claim, recover, update, list, enqueue, conditional];
}
void canonicalChecks;

const [path, originalID, revision] = process.argv.slice(2);
const db = openSQLite(path);
const store = createSQLiteJobStore(db);
const other = createSQLiteJobStore(db);
const keys = ["id", "revisionId", "actorUserId", "target", "status", "source", "sourcePrimaryId",
  "sourceNodeId", "sourceRevisionId", "snapshotContentHash", "snapshotVersion", "replicationVersion",
  "leadershipGeneration", "correlationId", "createdAt", "claimedAt", "startedAt", "finishedAt",
  "validationOutput", "applyOutput", "healthOutput", "errorMessage"];
const methodKeys = ["get", "claimNext", "recoverStale", "update", "list", "enqueue", "enqueueIfNotActive"];
let cases = 0;
const input = (target: JobRecord["target"] = "combined"): JobEnqueueInput => ({
  revisionId: revision, target, correlationId: "store-fixture",
});
function rows(connection = db) {
  return connection.query("SELECT * FROM main.apply_jobs ORDER BY id").all();
}
function record(value: JobRecord | undefined): asserts value is JobRecord {
  assert(value);
  assert.deepEqual(Object.keys(value).sort(), [...keys].sort());
  assert(value.createdAt instanceof Date);
  assert.equal(value.revisionId, revision);
  cases++;
}
async function promised<T>(pending: Promise<T>): Promise<T> {
  assert(pending instanceof Promise);
  cases++;
  return pending;
}
async function test(name: string, body: () => void | Promise<void>) {
  const before = rows();
  try { await body(); }
  finally {
    if (db.inTransaction) db.exec("ROLLBACK");
    db.query("DELETE FROM main.apply_jobs WHERE id != ?").run(originalID);
  }
  assert.deepEqual(rows(), before);
  assert.equal(db.inTransaction, false);
  cases++;
  console.log(`PASS ${name}`);
}
const calls = (s: JobStore): (() => Promise<unknown>)[] => [
  () => s.get(originalID), () => s.claimNext(), () => s.recoverStale(1),
  () => s.update(originalID, { status: "applied" }), () => s.list(),
  () => s.enqueue(input()), () => s.enqueueIfNotActive(input()),
];

try {
  assert.notEqual(store, other);
  assert.deepEqual(Object.keys(store).sort(), [...methodKeys].sort());
  // Protect the Go-created fixture from default claims while exercising new jobs.
  db.query("UPDATE main.apply_jobs SET status='applied' WHERE id=?").run(originalID);

  await test("constructor captures only the connection, even when closed", async () => {
    let accesses = 0;
    const closed = new Database(":memory:");
    closed.close();
    const traced = new Proxy(closed, { get() { accesses++; throw new Error("constructor access"); } });
    const bound = createSQLiteJobStore(traced);
    assert.equal(accesses, 0);
    assert.deepEqual(Object.keys(bound).sort(), [...methodKeys].sort());
    const closedStore = createSQLiteJobStore(closed);
    for (const call of calls(closedStore)) {
      await assert.rejects(promised(call()));
      cases++;
    }
    // These factories neither close nor replace the live supplied connection.
    assert.equal((await other.get(originalID))?.id, originalID);
  });

  await test("complete lifecycle and global terminal retry", async () => {
    assert.equal(await promised(store.get("missing")), undefined);
    assert.equal(await promised(store.claimNext()), undefined);
    assert.equal(await promised(store.claimNext(undefined)), undefined);
    const queued = await promised(store.enqueue({ ...input(), validationOutput: { steps: [1] } }));
    record(queued);
    assert.equal(queued.status, "queued");
    const fetched = await promised(other.get(queued.id));
    record(fetched);
    assert.deepEqual(fetched, queued);
    const listed = await promised(store.list());
    listed.forEach(record);
    assert.equal(listed.filter((job) => job.id === queued.id).length, 1);
    assert.equal(await promised(other.enqueueIfNotActive(input("certificate"))), undefined);
    const claimed = await promised(store.claimNext("combined"));
    record(claimed);
    assert.equal(claimed.id, queued.id);
    assert.equal(claimed.status, "validating");
    assert(claimed.claimedAt instanceof Date);
    const old = new Date(0);
    record(await promised(other.update(queued.id, { claimedAt: old, status: "applying" })));
    assert.equal(await promised(store.recoverStale(1, new Date(100))), 1);
    assert.equal((await other.get(queued.id))?.status, "queued");
    assert.equal(await store.enqueueIfNotActive(input("nginx")), undefined);
    const final = await promised(store.update(queued.id, { status: "applied", finishedAt: new Date(100) }));
    record(final);
    const retry = await promised(other.enqueueIfNotActive(input("certificate")));
    record(retry);
    assert.equal(retry.target, "certificate");
    assert.notEqual(retry.id, queued.id);
    assert.equal(await store.recoverStale(1), 0);
    assert.equal(await store.recoverStale(1, undefined), 0);
  });

  for (const target of ["coredns", "nginx", "combined", "certificate"] as const) {
    await test(`omitted and explicit undefined claim preserve wrapper default: ${target}`, async () => {
      const job = await store.enqueue(input(target));
      const expected = await claimNextSQLiteJobRecord(db);
      if (expected) {
        assert.equal(expected.id, job.id);
        db.query("UPDATE main.apply_jobs SET status='queued',claimed_at=NULL WHERE id=?").run(job.id);
      }
      for (const call of [() => store.claimNext(), () => other.claimNext(undefined)]) {
        const actual = await promised(call());
        assert.equal(actual?.id, expected?.id);
        assert.equal(actual?.target, expected?.target);
        assert.equal(actual?.status, expected?.status);
        if (actual) {
          record(actual);
          assert(actual.claimedAt instanceof Date);
          db.query("UPDATE main.apply_jobs SET status='queued',claimed_at=NULL WHERE id=?").run(job.id);
        }
      }
    });
  }

  for (const target of ["coredns", "nginx", "combined", "certificate"] as const) {
    for (const status of ["queued", "validating", "applying"] as const) {
      await test(`global conditional guard: ${target}/${status}`, async () => {
        const active = await store.enqueue({ ...input(target), status });
        record(active);
        const before = rows();
        assert.equal(await other.enqueueIfNotActive(input(target === "certificate" ? "nginx" : "certificate")), undefined);
        assert.deepEqual(rows(), before);
      });
    }
  }
  for (const status of ["applied", "failed", "rolled-back"] as const) {
    await test(`conditional terminal retry: ${status}`, async () => {
      record(await store.enqueue({ ...input("nginx"), status }));
      const retry = await other.enqueueIfNotActive({ ...input("certificate"), status: undefined, source: undefined });
      record(retry);
      assert.equal(retry.status, "queued");
      assert.equal(retry.source, "ordinary");
    });
  }

  await test("explicit Date identity and canonical non-update keys retain wrapper policy", async () => {
    const created = await store.enqueue({ ...input(), status: undefined, source: undefined });
    assert.equal(created.status, "queued");
    assert.equal(created.source, "ordinary");
    const patched = await other.update(created.id, { status: "validating", claimedAt: new Date(0),
      target: "certificate", correlationId: "ignored", createdAt: new Date(0) });
    assert.equal(patched.target, created.target);
    assert.equal(patched.correlationId, created.correlationId);
    assert.deepEqual(patched.createdAt, created.createdAt);
    let reads = 0;
    class ExplicitNow extends Date {
      override getTime() { assert.equal(this, now); reads++; return 100; }
    }
    const now = new ExplicitNow(100);
    assert.equal(await store.recoverStale(1, now), 1);
    assert.equal(reads, 1);
    assert.equal((await other.get(created.id))?.status, "queued");
  });

  await test("fresh own fields and isolated JSON and Dates across both factories", async () => {
    const original = await store.enqueue({ ...input(), validationOutput: { nested: [1] },
      applyOutput: [2], healthOutput: { ok: true }, startedAt: new Date(10),
      snapshotVersion: 2, replicationVersion: 3, leadershipGeneration: 4 });
    record(original);
    const a = await store.get(original.id), b = await other.get(original.id);
    record(a); record(b);
    assert.notEqual(a, b);
    assert.notEqual(a.createdAt, b.createdAt);
    assert.notEqual(a.startedAt, b.startedAt);
    assert.notEqual(a.validationOutput, b.validationOutput);
    (a.validationOutput as { nested: number[] }).nested.push(9);
    (a.applyOutput as number[]).push(9);
    (a.healthOutput as { ok: boolean }).ok = false;
    a.createdAt.setTime(0);
    a.startedAt!.setTime(0);
    assert.deepEqual(b, original);
    const c = (await store.list()).find((job) => job.id === original.id);
    record(c);
    assert.deepEqual(c, original);
    assert.notEqual(c.validationOutput, b.validationOutput);
    assert.equal(Object.hasOwn(c, "errorMessage"), true);
    assert.equal(c.errorMessage, undefined);
  });

  await test("forward original objects, encoder exceptions and explicit optional arguments", async () => {
    let encodes = 0, reads = 0;
    const payload = { toJSON() { assert.equal(this, payload); encodes++; return { ok: true }; } };
    const job = input();
    Object.defineProperty(job, "validationOutput", { enumerable: true,
      get() { assert.equal(this, job); reads++; return payload; } });
    const created = await store.enqueue(job);
    record(created);
    assert(reads > 0);
    assert.equal(encodes, 1);
    assert.deepEqual(created.validationOutput, { ok: true });
    const sentinel = new Error("original patch getter");
    const patch: Partial<JobRecord> = {};
    Object.defineProperty(patch, "status", { get() { assert.equal(this, patch); throw sentinel; } });
    await assert.rejects(promised(store.update(created.id, patch)), (error) => error === sentinel);
    const encodeError = new Error("original JSON encoder");
    await assert.rejects(store.update(created.id, { applyOutput: { toJSON() { throw encodeError; } } }),
      (error) => error === encodeError);
    await assert.rejects(store.enqueue({ ...input(), healthOutput: { toJSON() { throw encodeError; } } }),
      (error) => error === encodeError);
    let blockedReads = 0;
    const blocked = new Proxy(input("certificate"), { get() { blockedReads++; throw sentinel; } });
    assert.equal(await store.enqueueIfNotActive(blocked), undefined);
    assert.equal(blockedReads, 0);
    await assert.rejects(store.update(created.id, { finishedAt: new Date(NaN) }), RangeError);
    await assert.rejects(store.recoverStale(1, new Date(NaN)), /finite Date/);
    await assert.rejects(store.update("missing", { status: "failed" }), /Job not found: missing/);
    assert.equal((await store.get(created.id))?.status, "queued");
    assert.equal(await store.recoverStale(0, new Date(100)), 0);
    assert.equal(await store.claimNext("certificate"), undefined);
    assert.equal((await store.claimNext("combined"))?.id, created.id);
  });

  for (const boundary of ["BEGIN", "SAVEPOINT caller"]) {
    await test(`${boundary}: pending visibility, owned-write rejection and rollback`, async () => {
      const job = await store.enqueue(input());
      const observer = openSQLite(path);
      const cached = db.query<{ status: string }, [string]>("SELECT status FROM main.apply_jobs WHERE id=?");
      try {
        db.exec(boundary);
        const pending = await promised(other.update(job.id, { status: "failed", errorMessage: "pending" }));
        record(pending);
        assert.equal(pending.errorMessage, "pending");
        assert.equal((await store.get(job.id))?.status, "failed");
        assert.equal((await store.list()).find((row) => row.id === job.id)?.errorMessage, "pending");
        assert.equal(cached.get(job.id)?.status, "failed");
        assert.equal((await createSQLiteJobStore(observer).get(job.id))?.status, "queued");
        const before = rows();
        for (const call of [() => store.enqueue(input()), () => other.enqueueIfNotActive(input()),
          () => store.claimNext("combined"), () => other.recoverStale(1, new Date(100))]) {
          await assert.rejects(promised<unknown>(call()), /Nested write transaction/);
          assert.equal(db.inTransaction, true);
          assert.deepEqual(rows(), before);
          cases++;
        }
        if (boundary === "BEGIN") db.exec("ROLLBACK");
        else db.exec("ROLLBACK TO caller; RELEASE caller");
        assert.equal((await store.get(job.id))?.status, "queued");
        assert.equal(cached.get(job.id)?.status, "queued");
        assert.equal((await other.list()).find((row) => row.id === job.id)?.errorMessage, undefined);
      } finally { cached.finalize(); observer.close(); }
    });
  }

  await test("TEMP shadow cannot redirect composed main-table operations", async () => {
    db.exec("CREATE TEMP TABLE apply_jobs(id TEXT, status TEXT)");
    db.exec("INSERT INTO temp.apply_jobs VALUES ('shadow','queued')");
    try {
      const job = await store.enqueue(input("nginx"));
      record(await other.get(job.id));
      assert.equal(await store.get("shadow"), undefined);
      assert.equal((await store.list()).some((row) => row.id === "shadow"), false);
      record(await store.claimNext("nginx"));
      record(await store.update(job.id, { status: "applying", claimedAt: new Date(0) }));
      assert.equal(await other.recoverStale(1, new Date(100)), 1);
      assert.equal(await other.enqueueIfNotActive(input("certificate")), undefined);
      await store.update(job.id, { status: "rolled-back" });
      record(await other.enqueueIfNotActive(input("certificate")));
      assert.deepEqual(db.query("SELECT * FROM temp.apply_jobs").all(), [{ id: "shadow", status: "queued" }]);
    } finally { db.exec("DROP TABLE temp.apply_jobs"); }
  });

  await test("all seven boundaries preserve original source exceptions", async () => {
    const sentinel = new Error("local connection sentinel");
    const connection = new Proxy(db, {
      get(target, property) {
        if (property === "inTransaction") return false;
        if (["prepare", "query", "transaction"].includes(String(property))) return () => { throw sentinel; };
        const value = Reflect.get(target, property, target);
        return typeof value === "function" ? value.bind(target) : value;
      },
    });
    const bound = createSQLiteJobStore(connection);
    for (const call of calls(bound)) {
      await assert.rejects(promised(call()), (error) => error === sentinel);
      cases++;
    }
  });

  await test("writer contention rejects without wrapping or losing durable state", async () => {
    const job = await store.enqueue(input());
    const holder = openSQLite(path);
    db.exec("PRAGMA busy_timeout=150");
    try {
      holder.exec("BEGIN IMMEDIATE");
      const before = rows();
      assert.equal((await other.get(job.id))?.status, "queued");
      assert.equal((await other.list()).length, 2);
      for (const call of [() => store.enqueue(input()), () => other.enqueueIfNotActive(input()),
        () => store.claimNext("combined"), () => other.recoverStale(1, new Date(100)),
        () => store.update(job.id, { status: "failed" })]) {
        const start = performance.now();
        await assert.rejects(promised<unknown>(call()), /locked|busy/i);
        assert(performance.now() - start >= 100);
        assert.deepEqual(rows(), before);
        assert.equal(db.inTransaction, false);
        cases++;
      }
      holder.exec("ROLLBACK");
      assert.equal((await other.claimNext("combined"))?.id, job.id);
      record(await store.update(job.id, { status: "applied" }));
    } finally {
      if (holder.inTransaction) holder.exec("ROLLBACK");
      holder.close(); db.exec("PRAGMA busy_timeout=5000");
    }
  });

  // Leave one terminal lifecycle record for independent Go/reopen SQL checks.
  const durable = await store.enqueueIfNotActive({ ...input("certificate"), correlationId: "store-durable" });
  record(durable);
  record(await other.claimNext("certificate"));
  record(await store.update(durable.id, { status: "applied", healthOutput: { durable: true } }));
  const reopened = openSQLite(path);
  try {
    const reread = await createSQLiteJobStore(reopened).get(durable.id);
    record(reread);
    assert.equal(reread.status, "applied");
    assert.deepEqual(reread.healthOutput, { durable: true });
  } finally { reopened.close(); }
  db.query("UPDATE main.apply_jobs SET status='queued' WHERE id=?").run(originalID);
  console.log(`COMPLETE job store cases=${cases}`);
} finally {
  if (db.inTransaction) db.exec("ROLLBACK");
  db.close();
}
