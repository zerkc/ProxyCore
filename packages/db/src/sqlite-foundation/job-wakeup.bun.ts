import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import { createSQLiteJobWakeup, SQLITE_JOB_CURSOR_SQL } from "./job-wakeup.js";

// "fake" uses an injected clock; "migrated"/"cross" read the real cursor from a Go-migrated DB.
let checks = 0;
function equal(actual: unknown, expected: unknown) {
  assert.strictEqual(actual, expected);
  checks++;
}
function deep(actual: unknown, expected: unknown) {
  assert.deepStrictEqual(actual, expected);
  checks++;
}
const tick = () => new Promise<void>((resolve) => setImmediate(resolve));
async function settle() { for (let i = 0; i < 10; i++) await tick(); }
const replacer = (_: string, value: unknown) => (typeof value === "bigint" ? `${value}n` : value);

// Manual clock: sleeps park until advance() fires the oldest one, or the signal aborts.
function clock() {
  const sleeps: number[] = [];
  const waiting: Array<() => void> = [];
  return {
    sleeps,
    pending: () => waiting.length,
    sleep(ms: number, signal?: AbortSignal) {
      sleeps.push(ms);
      return new Promise<void>((resolve) => {
        if (signal?.aborted) return resolve();
        const done = () => { signal?.removeEventListener("abort", done); const i = waiting.indexOf(done); if (i >= 0) waiting.splice(i, 1); resolve(); };
        waiting.push(done);
        signal?.addEventListener("abort", done, { once: true });
      });
    },
    async advance() { await settle(); waiting.shift()?.(); await settle(); },
  };
}

async function fake() {
  // Start fires once; unchanged never fires; each change fires once with undefined.
  {
    const c = clock();
    const calls: unknown[] = [];
    let value: unknown = "a";
    let reads = 0;
    const controller = new AbortController();
    const done = createSQLiteJobWakeup({ sleep: c.sleep }).run({
      signal: controller.signal, pollIntervalMs: 250,
      onJob: (id) => { calls.push(id); },
      readCursor: () => { reads++; return value; },
    });
    await settle();
    deep(calls, [undefined]);
    await c.advance(); await c.advance();
    deep(calls, [undefined]);
    equal(reads, 3);
    value = "b";
    await c.advance();
    deep(calls, [undefined, undefined]);
    await c.advance();
    deep(calls, [undefined, undefined]);
    value = 2n; await c.advance();
    value = 2n; await c.advance();
    equal(calls.length, 3);
    deep(c.sleeps.every((ms) => ms === 250), true);
    controller.abort();
    await done;
    equal(c.pending(), 0);
  }
  // Validation, default interval, pre-aborted signal.
  for (const bad of [0, -1, NaN, Infinity, "5" as unknown as number]) {
    await assert.rejects(createSQLiteJobWakeup().run({ pollIntervalMs: bad, onJob() {}, readCursor: () => 1 }), RangeError);
    checks++;
  }
  await assert.rejects(createSQLiteJobWakeup().run({ errorDelayMs: 0, onJob() {}, readCursor: () => 1 }), RangeError);
  checks++;
  {
    const c = clock();
    const controller = new AbortController();
    const done = createSQLiteJobWakeup({ sleep: c.sleep }).run({ signal: controller.signal, onJob() {}, readCursor: () => 1 });
    await settle();
    deep(c.sleeps, [500]);
    controller.abort(); await done;
    const pre = new AbortController(); pre.abort();
    let touched = 0;
    await createSQLiteJobWakeup({ sleep: c.sleep }).run({ signal: pre.signal, onJob: () => { touched++; }, readCursor: () => { touched++; return 1; } });
    equal(touched, 0);
  }
  // readCursor failure: reported, backoff >= error delay (no hot spin), recovery re-fires like a reconnect.
  {
    const c = clock();
    const errors: unknown[] = [];
    const calls: unknown[] = [];
    const boom = new Error("locked");
    let mode: "ok" | "throw" | "reject" = "ok";
    const controller = new AbortController();
    const done = createSQLiteJobWakeup({ sleep: c.sleep }).run({
      signal: controller.signal, pollIntervalMs: 250,
      onJob: (id) => { calls.push(id); }, onError: (e) => { errors.push(e); },
      readCursor: () => { if (mode === "throw") throw boom; if (mode === "reject") return Promise.reject(boom); return "same"; },
    });
    await settle();
    mode = "throw"; await c.advance();
    deep(errors, [boom]);
    equal(c.sleeps.at(-1), 1_000);
    mode = "reject"; await c.advance();
    deep(errors, [boom, boom]);
    equal(c.pending(), 1);
    mode = "ok"; await c.advance();
    deep(calls, [undefined, undefined]); // recovery re-fires although the cursor is unchanged
    equal(c.sleeps.at(-1), 250);
    await c.advance();
    equal(calls.length, 2);
    controller.abort(); await done;
    const throwing = createSQLiteJobWakeup({ sleep: c.sleep });
    const stop = new AbortController();
    const second = throwing.run({ signal: stop.signal, pollIntervalMs: 2_000, errorDelayMs: 100, onJob() {}, onError() { throw new Error("ignored"); }, readCursor: () => { throw boom; } });
    await settle();
    equal(c.sleeps.at(-1), 2_000);
    stop.abort(); await second;
  }
  // onJob throw / rejection: reported, cursor advanced, no re-fire, loop continues.
  {
    const c = clock();
    const errors: unknown[] = [];
    let calls = 0;
    let value = 1;
    const sync = new Error("sync"), async = new Error("async");
    const controller = new AbortController();
    const done = createSQLiteJobWakeup({ sleep: c.sleep }).run({
      signal: controller.signal, onError: (e) => { errors.push(e); },
      onJob: () => { calls++; if (calls === 1) throw sync; if (calls === 2) return Promise.reject(async) as never; },
      readCursor: () => value,
    });
    await settle();
    deep(errors, [sync]);
    await c.advance();
    equal(calls, 1);
    value = 2; await c.advance();
    deep(errors, [sync, async]);
    value = 3; await c.advance();
    equal(calls, 3);
    controller.abort(); await done;
  }
  // Abort during an in-flight read: resolves, result ignored, late rejection not unhandled, onError silent.
  {
    const c = clock();
    const calls: unknown[] = [];
    const errors: unknown[] = [];
    let release!: (v: unknown) => void; let fail!: (e: unknown) => void;
    const controller = new AbortController();
    const done = createSQLiteJobWakeup({ sleep: c.sleep }).run({
      signal: controller.signal, onJob: (id) => { calls.push(id); }, onError: (e) => { errors.push(e); },
      readCursor: () => new Promise((resolve, reject) => { release = resolve; fail = reject; }),
    });
    await settle();
    controller.abort();
    await done;
    release(1); fail(new Error("late")); await settle();
    deep(calls, []); deep(errors, []); equal(c.sleeps.length, 0);
  }
  // No overlap: a slow read is never re-entered by the next tick.
  {
    const c = clock();
    let active = 0, peak = 0, release!: () => void;
    const controller = new AbortController();
    const done = createSQLiteJobWakeup({ sleep: c.sleep }).run({
      signal: controller.signal, onJob() {},
      readCursor: async () => { active++; peak = Math.max(peak, active); await new Promise<void>((r) => { release = r; }); active--; return 1; },
    });
    await settle(); await c.advance();
    equal(c.sleeps.length, 0);
    release(); await settle(); await c.advance(); await settle();
    equal(peak, 1);
    controller.abort(); release(); await done;
  }
  // Real timers: abort during a long sleep resolves promptly and leaves no timer behind.
  {
    const controller = new AbortController();
    const started = performance.now();
    const done = createSQLiteJobWakeup().run({ signal: controller.signal, pollIntervalMs: 60_000, onJob() {}, readCursor: () => 1 });
    await new Promise((r) => setTimeout(r, 20));
    controller.abort();
    await done;
    equal(performance.now() - started < 5_000, true);
  }
}

function cursor(db: Database): unknown {
  const statement = db.query(SQLITE_JOB_CURSOR_SQL);
  try { return (statement.get() as { cursor: string }).cursor; } finally { statement.finalize(); }
}

async function real(path: string, mode: string) {
  const db = new Database(path, { strict: true, safeIntegers: true });
  try {
    const revision = "cccccccc-cccc-cccc-cccc-cccccccccccc";
    const first = cursor(db);
    equal(typeof first, "string");
    deep(JSON.parse(first as string), [0, 0, 0, "", "", ""]);
    if (mode === "migrated") {
      // Same-process writer through a separate connection: insert and status change both move the cursor.
      const writer = new Database(path, { strict: true, safeIntegers: true });
      try {
        const calls: unknown[] = [];
        const controller = new AbortController();
        const c = clock();
        const done = createSQLiteJobWakeup({ sleep: c.sleep }).run({
          signal: controller.signal, onJob: (id) => { calls.push(id); }, readCursor: () => cursor(db),
        });
        await settle(); await c.advance();
        equal(calls.length, 1);
        writer.exec(`INSERT INTO apply_jobs(id,revision_id,actor_user_id,target,correlation_id,created_at)
          VALUES('ffffffff-ffff-ffff-ffff-ffffffffffff','${revision}',NULL,'combined','fixture-only','0')`);
        await c.advance(); equal(calls.length, 2);
        await c.advance(); equal(calls.length, 2);
        const inserted = cursor(db);
        deep(JSON.parse(inserted as string), [1, 1, 1, "", "", ""]);
        writer.exec("UPDATE apply_jobs SET status='validating',claimed_at='0'");
        await c.advance(); equal(calls.length, 3);
        writer.exec("UPDATE apply_jobs SET status='applied',finished_at='2'");
        await c.advance(); equal(calls.length, 4);
        equal(cursor(db) !== inserted, true);
        controller.abort(); await done;
        console.log(`job-wakeup cursor=${JSON.stringify(JSON.parse(cursor(db) as string), replacer)}`);
      } finally { writer.close(); }
    } else {
      // "cross": the Go test inserts, then updates, from its own process/connection.
      const controller = new AbortController();
      let fired = 0;
      let wake!: () => void;
      let resume = new Promise<void>((resolve) => { wake = resolve; });
      const timeout = setTimeout(() => { console.error("timeout waiting for wakeup"); process.exit(2); }, 20_000);
      const done = createSQLiteJobWakeup().run({
        signal: controller.signal, pollIntervalMs: 50,
        onJob: () => { fired++; wake(); }, readCursor: () => cursor(db),
      });
      await resume; equal(fired, 1);
      resume = new Promise<void>((resolve) => { wake = resolve; });
      console.log("job-wakeup ready");
      await resume; equal(fired, 2);
      deep(JSON.parse(cursor(db) as string).slice(0, 3), [1, 1, 1]);
      resume = new Promise<void>((resolve) => { wake = resolve; });
      console.log("job-wakeup insert-detected");
      await resume; equal(fired, 3);
      console.log("job-wakeup status-detected");
      controller.abort(); await done; clearTimeout(timeout);
    }
  } finally { db.close(); }
}

const [path, mode] = process.argv.slice(2);
if (mode === "fake") await fake();
else if (path && (mode === "migrated" || mode === "cross")) await real(path, mode);
else throw new Error("usage: job-wakeup.bun.ts <db> fake|migrated|cross");
console.log(`job-wakeup checks=${checks}`);
