import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import type { ContinuityPersistencePort, ContinuityTransactionPort } from "../ports.js";
import { createSQLiteContinuityPersistence } from "./continuity-persistence.js";

// Real-database modes run in Go-launched processes; "fake" injects statement failures.
let checks = 0;
function equal(actual: unknown, expected: unknown, message?: string) {
  if (message === undefined) assert.strictEqual(actual, expected);
  else assert.strictEqual(actual, expected, message);
  checks++;
}
function deep(actual: unknown, expected: unknown) {
  assert.deepStrictEqual(actual, expected);
  checks++;
}
async function rejection(promise: Promise<unknown>, error: unknown) {
  try {
    await promise;
    assert.fail("expected rejection");
  } catch (actual) {
    equal(actual, error);
  }
}

const node = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
const revision = "cccccccc-cccc-cccc-cccc-cccccccccccc";
const token = "dddddddd-dddd-dddd-dddd-dddddddddddd";
const primary = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee";
const job = "ffffffff-ffff-ffff-ffff-ffffffffffff";
const installation = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const attemptID = "11111111-1111-1111-1111-111111111111";
const syncID = "22222222-2222-2222-2222-222222222222";
const snapshotID = "33333333-3333-3333-3333-333333333333";
const binaryID = "44444444-4444-4444-4444-444444444444";
const nullID = "55555555-5555-5555-5555-555555555555";
const rollbackID = "66666666-6666-6666-6666-666666666666";
const missingRevision = "77777777-7777-7777-7777-777777777777";
const hash = "  hash 雪  ";
const date = new Date(0);
const state: Parameters<ContinuityTransactionPort["saveNodeState"]>[0] = {
  id: "default", syncEnabled: true, consecutiveFailures: 3, updatedAt: date,
  primaryUrl: "https://fixture.invalid", enrollmentAttemptId: attemptID,
};
const attempt: Parameters<ContinuityTransactionPort["createEnrollmentAttempt"]>[0] = {
  id: attemptID, state: "draft", primaryUrl: "https://fixture.invalid",
  localNodeIp: "127.0.0.42", ephemeralPrivateKeyWrapped: "fixture-only",
  bootstrapPayload: new Uint8Array([65, 66]), createdAt: date, updatedAt: date,
};
const grant: Parameters<ContinuityTransactionPort["createEnrollmentGrant"]>[0] = {
  attemptId: attemptID, tokenId: token, installationId: installation,
  nodeId: node, primaryId: primary, primaryGeneration: 9007199254740992,
  nodeEphemeralPublicKey: "fixture-only", sealedBootstrapPayload: "fixture-only",
  payloadHash: "fixture-only", createdAt: date, expiresAt: new Date(1),
};
const sync: Parameters<ContinuityTransactionPort["recordSyncAttempt"]>[0] = {
  id: syncID, nodeId: node, trigger: "manual", status: "queued",
  revisionId: revision, applyJobId: job, contentHash: hash,
  createdAt: date, updatedAt: date,
};
const snapshot: Parameters<ContinuityTransactionPort["recordAppliedSnapshot"]>[0] = {
  id: snapshotID, sourcePrimaryId: primary, leadershipGeneration: 9007199254740992,
  snapshotVersion: -2147483648, replicationVersion: 2147483647,
  contentHash: hash, snapshotBody: new Uint8Array(), revisionId: revision,
  status: "applied", applyJobId: job, appliedAt: date,
};
const ack: Parameters<ContinuityTransactionPort["recordSnapshotAcknowledgement"]>[0] = {
  nodeId: node, contentHash: hash, snapshotVersion: 1, replicationVersion: 2,
  revisionId: revision, leadershipGeneration: 9007199254740992,
  appliedAt: date, receivedAt: new Date(1),
};

function rows(db: Database, sql: string) {
  const statement = db.prepare(sql);
  try { return statement.all(); } finally { statement.finalize(); }
}
function raw(db: Database) {
  return [
    rows(db, "SELECT * FROM main.node_state ORDER BY id"),
    rows(db, "SELECT * FROM main.enrollment_attempts ORDER BY id"),
    rows(db, "SELECT * FROM main.enrollment_grants ORDER BY attempt_id"),
    rows(db, "SELECT * FROM main.sync_attempts ORDER BY id"),
    rows(db, "SELECT * FROM main.applied_snapshots ORDER BY id"),
    rows(db, "SELECT * FROM main.node_snapshot_acks ORDER BY node_id,content_hash"),
  ];
}
async function writeBatch(tx: ContinuityTransactionPort) {
  await tx.saveNodeState(state);
  await tx.createEnrollmentAttempt(attempt);
  await tx.createEnrollmentGrant(grant);
  await tx.recordSyncAttempt(sync);
  await tx.recordAppliedSnapshot(snapshot);
  await tx.recordAppliedSnapshot({ ...snapshot, id: binaryID,
    contentHash: "binary", snapshotBody: new Uint8Array([0, 255, 128, 65]) });
  await tx.recordAppliedSnapshot({ ...snapshot, id: nullID,
    contentHash: "null", snapshotBody: undefined });
  await tx.recordSnapshotAcknowledgement(ack);
  await tx.recordSnapshotAcknowledgement({ ...ack, snapshotVersion: 3, replicationVersion: 4 });
}
async function projection(tx: ContinuityTransactionPort) {
  return [await tx.getNodeState(), await tx.getEnrollmentAttempt(attemptID),
    await tx.getEnrollmentGrant(attemptID), await tx.getAppliedSnapshot(snapshotID),
    await tx.getAppliedSnapshot(binaryID), await tx.getAppliedSnapshot(nullID),
    await tx.getSnapshotAcknowledgement(node, hash)];
}

// Statement-level fake: records exec calls and injects failures by SQL text.
function fakeDatabase(failures: Record<string, unknown> = {}) {
  const log: string[] = [];
  let open = false;
  const db = {
    get inTransaction() { return open; },
    exec(sql: string) {
      log.push(sql);
      if (sql === "BEGIN IMMEDIATE") open = true;
      if (sql in failures) {
        if (sql !== "COMMIT") open = false; // ROLLBACK failure leaves nothing for later calls
        throw failures[sql];
      }
      if (sql === "COMMIT" || sql === "ROLLBACK") open = false;
    },
  } as unknown as Database;
  return { db, log };
}

async function fake() {
  // Construction and passthrough: value, Promise and tx identity.
  let { db, log } = fakeDatabase();
  let persistence: ContinuityPersistencePort = createSQLiteContinuityPersistence(db);
  deep(log, []);
  const value = Object.freeze({ sentinel: "value" });
  let seen: ContinuityTransactionPort | undefined;
  const promise = persistence.withTransaction(async (tx) => { seen = tx; return value; });
  equal(promise instanceof Promise, true);
  equal(await promise, value);
  deep(Object.keys(seen!).sort(), ["createEnrollmentAttempt", "createEnrollmentGrant", "getAppliedSnapshot",
    "getEnrollmentAttempt", "getEnrollmentGrant", "getNodeState", "getSnapshotAcknowledgement",
    "recordAppliedSnapshot", "recordSnapshotAcknowledgement", "recordSyncAttempt", "saveNodeState"]);
  deep(log, ["BEGIN IMMEDIATE", "COMMIT"]);

  // Callback throw: original identity wins, ROLLBACK attempted, never COMMIT.
  const original = Object.freeze({ original: "identity" });
  ({ db, log } = fakeDatabase());
  persistence = createSQLiteContinuityPersistence(db);
  await rejection(persistence.withTransaction(async () => { throw original; }), original);
  deep(log, ["BEGIN IMMEDIATE", "ROLLBACK"]);
  // Synchronous throw from a non-async callback is also a rejection.
  const syncThrow = persistence.withTransaction((() => { throw original; }) as never);
  equal(syncThrow instanceof Promise, true);
  await rejection(syncThrow, original);
  // Rejection with a falsy value keeps identity as well.
  await rejection(persistence.withTransaction(async () => { throw undefined; }), undefined);

  // Rollback failure never replaces the original error.
  const rollbackError = Object.freeze({ rollback: "failure" });
  ({ db, log } = fakeDatabase({ ROLLBACK: rollbackError }));
  persistence = createSQLiteContinuityPersistence(db);
  await rejection(persistence.withTransaction(async () => { throw original; }), original);
  deep(log, ["BEGIN IMMEDIATE", "ROLLBACK"]);

  // Commit failure surfaces and attempts rollback; rollback failure does not mask it.
  const commitError = Object.freeze({ commit: "failure" });
  ({ db, log } = fakeDatabase({ COMMIT: commitError }));
  persistence = createSQLiteContinuityPersistence(db);
  await rejection(persistence.withTransaction(async () => value), commitError);
  deep(log, ["BEGIN IMMEDIATE", "COMMIT", "ROLLBACK"]);
  ({ db, log } = fakeDatabase({ COMMIT: commitError, ROLLBACK: rollbackError }));
  persistence = createSQLiteContinuityPersistence(db);
  await rejection(persistence.withTransaction(async () => value), commitError);
  deep(log, ["BEGIN IMMEDIATE", "COMMIT", "ROLLBACK"]);

  // BEGIN failure: callback never runs, no rollback and no sticky active flag.
  const beginError = Object.freeze({ begin: "failure" });
  ({ db, log } = fakeDatabase({ "BEGIN IMMEDIATE": beginError }));
  persistence = createSQLiteContinuityPersistence(db);
  let ran = false;
  await rejection(persistence.withTransaction(async () => { ran = true; }), beginError);
  equal(ran, false);
  deep(log, ["BEGIN IMMEDIATE"]);
}

async function real(path: string, mode: string) {
  const db = new Database(path, { strict: true, safeIntegers: true });
  db.exec("PRAGMA foreign_keys=ON");
  const external = new Database(path, { strict: true, safeIntegers: true });
  const persistence = createSQLiteContinuityPersistence(db);
  const sibling = createSQLiteContinuityPersistence(db);
  const cached = db.query("SELECT 41 AS value");
  try {
    if (mode === "migrated") {
      const initial = raw(db);
      // Rollback probes: inserts and updates vanish, original error identity wins.
      const original = Object.freeze({ probe: "rollback" });
      await rejection(persistence.withTransaction(async (tx) => {
        await writeBatch(tx);
        equal(db.inTransaction, true);
        deep(raw(external), initial);
        throw original;
      }), original);
      equal(db.inTransaction, false);
      deep(raw(db), initial);
      // Reentrancy (same and sibling instance) and caller-owned transactions are rejected.
      const committed = Object.freeze({ committed: true });
      const outer = await persistence.withTransaction(async (tx) => {
        await tx.saveNodeState(state);
        await assert.rejects(persistence.withTransaction(async () => assert.fail("nested ran")), /[Nn]ested/);
        await assert.rejects(sibling.withTransaction(async () => assert.fail("sibling ran")), /[Nn]ested/);
        checks += 2;
        equal(db.inTransaction, true, "rejected nested call leaves outer transaction open");
        return committed;
      });
      equal(outer, committed);
      equal(db.inTransaction, false);
      equal((await persistence.withTransaction((tx) => tx.getNodeState()))!.consecutiveFailures, 3);
      // Concurrent (non-nested) call while one is pending is rejected, outer unaffected.
      let release!: () => void;
      const gate = new Promise<void>((resolve) => { release = resolve; });
      const pending = persistence.withTransaction(async (tx) => { await gate; return tx.getNodeState(); });
      await assert.rejects(sibling.withTransaction(async () => assert.fail("concurrent ran")), /[Nn]ested/);
      checks++;
      release();
      equal((await pending)!.id, "default");
      db.exec("BEGIN");
      await assert.rejects(persistence.withTransaction(async () => assert.fail("caller tx ran")), /[Nn]ested/);
      checks++;
      equal(db.inTransaction, true);
      db.exec("ROLLBACK");
      // Real COMMIT failure: deferred foreign key violation surfaces and is rolled back.
      const before = raw(db);
      await assert.rejects(persistence.withTransaction(async (tx) => {
        db.exec("PRAGMA defer_foreign_keys=ON");
        await tx.recordSyncAttempt({ ...sync, id: rollbackID, revisionId: missingRevision });
      }), /FOREIGN KEY/i);
      checks++;
      equal(db.inTransaction, false);
      deep(raw(db), before);
      // Facade is reusable after every failure; commit the full batch, reading through tx.
      await persistence.withTransaction(async (tx) => { await writeBatch(tx); });
      equal(db.inTransaction, false);
      deep(raw(external), raw(db));
      // Update rollback against durable rows with a non-active attempt state.
      const durable = raw(db);
      await assert.rejects(persistence.withTransaction(async (tx) => {
        await tx.saveNodeState({ ...state, consecutiveFailures: 99 });
        await tx.recordSnapshotAcknowledgement({ ...ack, snapshotVersion: 99 });
        await tx.createEnrollmentAttempt({ ...attempt, id: rollbackID, state: "cancelled" });
        await tx.recordSyncAttempt({ ...sync, id: rollbackID });
        throw new Error("probe");
      }), /probe/);
      checks++;
      deep(raw(db), durable);
    }
    const saved = await persistence.withTransaction(projection);
    equal((saved[0] as { consecutiveFailures: number }).consecutiveFailures, 3);
    deep(saved, await createSQLiteContinuityPersistence(external).withTransaction(projection));
    equal(await persistence.withTransaction((tx) => tx.getEnrollmentAttempt(rollbackID)), undefined);
    equal(db.query("SELECT 41 AS value"), cached, "caller statement cache untouched");
    deep(cached.get(), { value: 41n });
    equal(db.inTransaction, false);
    console.log(`continuity-persistence durable=${JSON.stringify({ raw: raw(db), projection: saved }, (_key, value) => typeof value === "bigint" ? `${value}n` : value)}`);
  } finally {
    external.close();
    db.close();
  }
}

const mode = process.argv[3];
if (mode === "fake") await fake();
else await real(process.argv[2]!, mode!);
console.log(`continuity-persistence checks=${checks} mode=${mode}`);
