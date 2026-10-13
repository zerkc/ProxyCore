// Disposable real-Bun acceptance runner, never a production entry point.
import { Database } from "bun:sqlite";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { createInterface } from "node:readline";
import { queuedSQLiteJobIDs, startSQLiteJobDetection } from "./sqlite-job-detection";

const delay = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

async function units(): Promise<void> {
  const db = new Database("detector-unit.db", { create: true, strict: true });
  try {
    db.exec("CREATE TABLE apply_jobs (id TEXT PRIMARY KEY, status TEXT NOT NULL)");
    db.exec("INSERT INTO apply_jobs VALUES ('first', 'queued'), ('inactive', 'applying')");
    assert.deepEqual([...queuedSQLiteJobIDs(db)], ["first"]);
    for (const intervalMs of [1, 4_000]) {
      const valid = startSQLiteJobDetection({ db, intervalMs, onJob() {} });
      valid.stop();
      await valid.done;
    }
    for (const intervalMs of [0, -1, 0.5, NaN, Infinity, 4_001]) {
      assert.throws(() => startSQLiteJobDetection({ db, intervalMs, onJob() {} }), RangeError);
    }
    const events = new EventEmitter();
    let scans = 0;
    const observations: string[] = [];
    const controller = new AbortController();
    const detector = startSQLiteJobDetection({
      db, intervalMs: 15, signal: controller.signal,
      onJob: (id) => { observations.push(id); },
      onScan: () => { scans++; events.emit("scan"); },
    });
    const waitScans = async (target: number) => {
      const deadline = setTimeout(() => events.emit("timeout"), 2_000);
      try {
        await new Promise<void>((resolve, reject) => {
          const check = () => {
            if (scans >= target) { cleanup(); resolve(); }
          };
          const timeout = () => { cleanup(); reject(new Error("scan deadline")); };
          const cleanup = () => {
            events.off("scan", check);
            events.off("timeout", timeout);
          };
          events.on("scan", check);
          events.on("timeout", timeout);
          check();
        });
      } finally { clearTimeout(deadline); }
    };
    assert.equal(scans, 1, "immediate startup");
    await waitScans(3);
    assert.equal(observations.filter((id) => id === "first").length, 3);
    db.exec("UPDATE apply_jobs SET status = 'failed'");
    const previous = observations.length;
    await waitScans(scans + 2);
    assert.equal(observations.length, previous);
    db.exec("UPDATE apply_jobs SET status = 'queued' WHERE id = 'first'");
    await waitScans(scans + 1);
    assert.equal(observations.at(-1), "first", "refire without watermark");
    controller.abort();
    await detector.done;
    const stoppedScans = scans;
    const stoppedJobs = observations.length;
    await delay(60);
    assert.equal(scans, stoppedScans);
    assert.equal(observations.length, stoppedJobs);
    assert.deepEqual([...queuedSQLiteJobIDs(db)], ["first"], "caller still owns DB");
    for (let i = 0; i < 200; i++) {
      db.query("INSERT INTO apply_jobs VALUES (?, 'queued')").run(`backlog-${i}`);
    }
    assert.equal([...queuedSQLiteJobIDs(db)].length, 201, "no LIMIT starvation");
    let both = 0;
    for (let i = 0; i < 2; i++) {
      const reader = new Database("detector-unit.db", { readonly: true });
      const restarted = startSQLiteJobDetection({ db: reader, onJob: () => { both++; } });
      restarted.stop();
      await restarted.done;
      reader.close();
    }
    assert.equal(both, 402, "two readers observe without claiming");
    const duringScan = new AbortController();
    let cancelledJobs = 0;
    let cancelledScans = 0;
    await startSQLiteJobDetection({
      db, signal: duringScan.signal,
      onJob: () => { cancelledJobs++; duringScan.abort(); },
      onScan: () => { cancelledScans++; },
    }).done;
    assert.equal(cancelledJobs, 1);
    assert.equal(cancelledScans, 0, "abort inside callback stops the current scan");
    const scanError = new Error("scan callback failure");
    await assert.rejects(startSQLiteJobDetection({
      db, onJob() {}, onScan: () => { throw scanError; },
    }).done, (e) => e === scanError);
    const callbackError = new Error("callback failure");
    let reported: unknown;
    await assert.rejects(startSQLiteJobDetection({
      db, onJob: () => { throw callbackError; }, onError: (e) => { reported = e; },
    }).done, (e) => e === callbackError);
    assert.equal(reported, callbackError);
    const handlerError = new Error("error handler failure");
    await assert.rejects(startSQLiteJobDetection({
      db, onJob: () => { throw callbackError; }, onError: () => { throw handlerError; },
    }).done, (e) => e === handlerError);
    await assert.rejects(startSQLiteJobDetection({
      db, onJob: () => Promise.resolve(),
    }).done, /synchronous/);
    await assert.rejects(startSQLiteJobDetection({
      db, onJob: () => { throw callbackError; }, onError: () => Promise.resolve(),
    }).done, /synchronous/);
    const regressions: string[] = [];
    for (const slot of ["onJob", "onScan", "onError"] as const) {
      for (const custom of [false, true]) {
        const escaped: unknown[] = [];
        const observe = (reason: unknown) => { escaped.push(reason); };
        const reason = new Error(`${slot}/${custom ? "thenable" : "promise"}`);
        let queries = 0;
        let callbacks = 0;
        let assimilated = 0;
        const originalPrepare = db.prepare;
        db.prepare = function (...args: Parameters<Database["prepare"]>) {
          queries++;
          return originalPrepare.apply(db, args);
        } as Database["prepare"];
        const rejected = custom
          ? () => ({ then: (_resolve: unknown, reject: (error: unknown) => void) => {
              assimilated++; reject(reason);
            } })
          : async () => { throw reason; };
        process.on("unhandledRejection", observe); // Disposable fixture only.
        try {
          const controller = new AbortController();
          const options = {
            db, intervalMs: 15, signal: controller.signal,
            onJob: () => { callbacks++; if (slot === "onError") throw reason; },
            onScan: () => { callbacks++; },
            onError: () => { callbacks++; },
          };
          options[slot] = () => { callbacks++; return rejected(); };
          const failed = startSQLiteJobDetection(options);
          await assert.rejects(failed.done, (error) =>
            error instanceof TypeError && error.message === "Detector callbacks must be synchronous/nonblocking");
          const stopped = [queries, callbacks];
          await delay(60); // Multiple bounded event-loop turns and four timer intervals.
          controller.abort();
          failed.stop();
          assert.deepEqual([queries, callbacks], stopped, "terminal cleanup stopped all work");
          assert.deepEqual(escaped, [], `escaped rejection: ${reason.message}`);
          if (custom) assert.equal(assimilated, 1, "custom rejecting thenable was consumed");
          assert.equal([...queuedSQLiteJobIDs(db)].length, 201, "cursor released; DB caller-owned");
        } catch (error) {
          regressions.push(String(error));
        } finally {
          process.off("unhandledRejection", observe);
          db.prepare = originalPrepare;
        }
      }
    }
    assert.deepEqual(regressions, [], "rejected callback regression matrix");
    const laterClosed = new Database("detector-unit.db", { readonly: true });
    let lateErrors = 0;
    const closing = startSQLiteJobDetection({
      db: laterClosed, intervalMs: 15, onJob() {},
      onError: () => { lateErrors++; },
    });
    const closedFailure = assert.rejects(closing.done);
    laterClosed.close();
    await closedFailure;
    await delay(45);
    assert.equal(lateErrors, 1, "closed DB terminates instead of retrying silently");
    const stopped = startSQLiteJobDetection({ db, intervalMs: 15, onJob() {} });
    stopped.stop();
    await stopped.done;
    db.close();
    await delay(60); // A retained timer querying this closed DB would reject/crash.
    const aborted = new AbortController();
    aborted.abort();
    await startSQLiteJobDetection({
      db, signal: aborted.signal, onJob: () => assert.fail("aborted callback"),
    }).done;
    let closedError = false;
    await assert.rejects(startSQLiteJobDetection({
      db, onJob() {}, onError: () => { closedError = true; },
    }).done);
    assert.equal(closedError, true);
    const broken = new Database("detector-broken.db", { create: true });
    try {
      let emptyScan = false;
      await assert.rejects(startSQLiteJobDetection({
        db: broken, onJob() {}, onScan: () => { emptyScan = true; },
      }).done);
      assert.equal(emptyScan, false, "SQL errors are not empty queues");
    } finally { broken.close(); }
  } finally { db.close(); }
  console.log("UNIT_OK");
}

async function bridge(path: string): Promise<void> {
  const db = new Database(path, { readonly: true, strict: true });
  // Truthful suspended-async-processor fixture, NOT the render/apply pipeline.
  // Importing poller would drag apply/render/runtime into this unwired boundary.
  let release!: () => void;
  let busy = true;
  const processing = (async () => {
    await new Promise<void>((resolve) => { release = resolve; });
    busy = false;
  })();
  let scans = 0;
  const start = () => startSQLiteJobDetection({
    db,
    onJob: (id) => console.log(`JOB ${id} ${busy}`),
    onScan: () => console.log(`SCAN ${++scans} ${busy}`),
  });
  let detector = start();
  // Observe fatal errors immediately; no hidden liveness or unhandled rejection.
  const watch = () => detector.done.catch((error) => {
    console.log("FATAL");
    throw error;
  });
  let watched = watch();
  const input = createInterface({ input: process.stdin });
  try {
    console.log("READY");
    for await (const command of input) {
      if (command === "checkpoint") console.log("CHECKPOINT");
      else if (command === "stop") {
        detector.stop();
        await watched;
        console.log("STOP");
      } else if (command === "start") {
        detector = start();
        watched = watch();
        console.log("START");
      } else if (command === "exit") break;
      else throw new Error("Unknown fixture command");
    }
  } finally {
    detector.stop();
    await watched;
    input.close();
    release();
    await processing;
    db.close();
  }
  console.log("EXIT");
}

if (process.argv[2] === "unit") await units();
else if (process.argv[2]) await bridge(process.argv[2]);
else throw new Error("Disposable fixture mode/path required");
