import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import { mock } from "bun:test";
import type { ContinuityTransactionPort } from "../ports.js";

// Mocks and real database exercises run in separate Go-launched processes.
const bindings = [
  ["getNodeState", "node-state-record", "getSQLiteNodeStateRecord", 0],
  ["saveNodeState", "node-state-save-record", "saveSQLiteNodeStateRecord", 1],
  ["createEnrollmentAttempt", "enrollment-attempt-create-record", "createSQLiteEnrollmentAttemptRecord", 1],
  ["getEnrollmentAttempt", "enrollment-attempt-read-record", "getSQLiteEnrollmentAttemptRecord", 1],
  ["createEnrollmentGrant", "enrollment-grant-create-record", "createSQLiteEnrollmentGrantRecord", 1],
  ["getEnrollmentGrant", "enrollment-grant-read-record", "getSQLiteEnrollmentGrantRecord", 1],
  ["recordSyncAttempt", "sync-attempt-create-record", "createSQLiteSyncAttemptRecord", 1],
  ["recordAppliedSnapshot", "applied-snapshot-create-record", "createSQLiteAppliedSnapshotRecord", 1],
  ["getAppliedSnapshot", "applied-snapshot-read-record", "getSQLiteAppliedSnapshotRecord", 1],
  ["recordSnapshotAcknowledgement", "snapshot-acknowledgement-save-record", "saveSQLiteSnapshotAcknowledgementRecord", 1],
  ["getSnapshotAcknowledgement", "snapshot-acknowledgement-read-record", "getSQLiteSnapshotAcknowledgementRecord", 2],
] as const satisfies readonly (readonly [keyof ContinuityTransactionPort, string, string, number])[];
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
function invoke(port: ContinuityTransactionPort, key: keyof ContinuityTransactionPort, args: unknown[]) {
  // Intentional malformed arguments test the runtime boundary without changing
  // the canonical compile-time assignability check below.
  return (port[key] as (...args: unknown[]) => Promise<unknown>)(...args);
}
async function rejection(promise: Promise<unknown>, error: unknown) {
  try {
    await promise;
    assert.fail("expected rejection");
  } catch (actual) {
    equal(actual, error);
  }
}

async function forwarding() {
  let calls: { key: string; args: unknown[] }[] = [];
  let result: Promise<unknown> = Promise.resolve(undefined);
  for (const [key, module, symbol] of bindings) {
    mock.module(`./${module}.js`, () => ({
      [symbol]: (...args: unknown[]) => {
        calls.push({ key, args });
        return result;
      },
    }));
  }
  const { createSQLiteContinuityTransactionPort: create } = await import("./continuity-transaction-port.js");
  const db = new Proxy({} as Database, {
    get(_target, property) {
      assert.fail(`factory touched connection property ${String(property)}`);
    },
  });
  const originalNow = Date.now;
  Date.now = () => assert.fail("factory consulted clock");
  let port: ContinuityTransactionPort;
  try {
    port = create(db);
    equal(calls.length, 0, "construction calls no primitives");
    const second = create(db);
    equal(calls.length, 0);
    equal(second === port, false, "no cached factory object");
    deep(Object.keys(port).sort(), bindings.map(([key]) => key).sort());
    for (const [key, , , arity] of bindings) {
      equal(port[key].length, arity);
      const record = Object.freeze({ sentinel: key, bytes: new Uint8Array([0, 255]) });
      const scalar = "  literal 雪  ";
      const standard = arity === 2 ? [scalar, "second hash"] : arity === 1 ? [record] : [];
      const cases = arity === 2
        ? [standard, ["second hash", scalar], [], [undefined, undefined], [null, 17], [record, record]]
        : arity === 1 ? [standard, [scalar], [], [undefined], [null], [17]] : [[], [record]];
      for (const args of cases) {
        calls = [];
        result = Promise.resolve(record);
        const returned = invoke(port, key, args);
        equal(returned, result, `${key} direct Promise identity`);
        equal(calls.length, 1);
        equal(calls[0]!.key, key);
        const expected = [db, ...Array.from({ length: arity }, (_, i) => args[i])];
        equal(calls[0]!.args.length, expected.length);
        expected.forEach((value, i) => equal(calls[0]!.args[i], value));
        equal(await returned, record, "resolved object identity");
      }
      for (const value of [undefined, null, record]) {
        result = Promise.resolve(value);
        const returned = invoke(port, key, standard);
        equal(returned, result);
        equal(await returned, value, "absence/void/value identity");
      }
      const error = Object.freeze({ primitive: key });
      result = Promise.reject(error);
      const returned = invoke(port, key, standard);
      equal(returned, result);
      await rejection(returned, error);
    }
  } finally {
    Date.now = originalNow;
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

// Owned fixture statements only: no changes to primitive cursor/cache policy.
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
async function projection(port: ContinuityTransactionPort) {
  return [await port.getNodeState(), await port.getEnrollmentAttempt(attemptID),
    await port.getEnrollmentGrant(attemptID), await port.getAppliedSnapshot(snapshotID),
    await port.getAppliedSnapshot(binaryID), await port.getAppliedSnapshot(nullID),
    await port.getSnapshotAcknowledgement(node, hash)];
}
async function absent(port: ContinuityTransactionPort) {
  equal(await port.getEnrollmentAttempt(rollbackID), undefined);
  equal(await port.getEnrollmentGrant(rollbackID), undefined);
  equal(await port.getAppliedSnapshot(rollbackID), undefined);
  equal(await port.getSnapshotAcknowledgement(node, "rollback"), undefined);
}
async function writeBatch(port: ContinuityTransactionPort) {
  await port.saveNodeState(state);
  await port.createEnrollmentAttempt(attempt);
  await port.createEnrollmentGrant(grant);
  await port.recordSyncAttempt(sync);
  await port.recordAppliedSnapshot(snapshot);
  await port.recordAppliedSnapshot({ ...snapshot, id: binaryID,
    contentHash: "binary", snapshotBody: new Uint8Array([0, 255, 128, 65]) });
  await port.recordAppliedSnapshot({ ...snapshot, id: nullID,
    contentHash: "null", snapshotBody: undefined });
  await port.recordSnapshotAcknowledgement(ack);
  await port.recordSnapshotAcknowledgement({ ...ack, snapshotVersion: 3, replicationVersion: 4 });
}
async function checkProjection(port: ContinuityTransactionPort) {
  const savedState = await port.getNodeState();
  equal(savedState!.id, "default");
  equal(savedState!.consecutiveFailures, 3);
  equal(savedState!.primaryUrl, state.primaryUrl);
  equal(savedState!.credentialId, undefined);
  equal(savedState!.updatedAt.getTime(), 0);
  const savedAttempt = await port.getEnrollmentAttempt(attemptID);
  equal(savedAttempt!.state, "draft");
  deep(savedAttempt!.bootstrapPayload, attempt.bootstrapPayload);
  equal(savedAttempt!.archiveId, undefined);
  equal(savedAttempt!.createdAt.getTime(), 0);
  const savedGrant = await port.getEnrollmentGrant(attemptID);
  equal(savedGrant!.tokenId, token);
  equal(savedGrant!.primaryGeneration, grant.primaryGeneration);
  equal(savedGrant!.verifiedPreviewDigest, undefined);
  equal(savedGrant!.expiresAt.getTime(), 1);
  equal(await port.getEnrollmentGrant(token), undefined, "attempt key, not token key");
  const savedSnapshot = await port.getAppliedSnapshot(snapshotID);
  deep(savedSnapshot!.snapshotBody, new Uint8Array());
  equal(savedSnapshot!.snapshotBody === snapshot.snapshotBody, false);
  equal(savedSnapshot!.snapshotVersion, -2147483648);
  equal(savedSnapshot!.replicationVersion, 2147483647);
  equal(savedSnapshot!.leadershipGeneration, 9007199254740992);
  equal(savedSnapshot!.failureCode, undefined);
  deep((await port.getAppliedSnapshot(binaryID))!.snapshotBody, new Uint8Array([0, 255, 128, 65]));
  equal((await port.getAppliedSnapshot(nullID))!.snapshotBody, undefined);
  const savedAck = await port.getSnapshotAcknowledgement(node, hash);
  equal(savedAck!.snapshotVersion, 3, "UPSERT update branch");
  equal(savedAck!.replicationVersion, 4);
  equal(savedAck!.receivedAt.getTime(), 1);
  equal(await port.getSnapshotAcknowledgement(hash, node), undefined, "ordered scalar keys");
  await absent(port);
}

async function realFailures(create: (db: Database) => ContinuityTransactionPort) {
  const error = Object.freeze({ prepare: "identity" });
  let prepares = 0;
  const db = { prepare() { prepares++; throw error; } } as unknown as Database;
  const port: ContinuityTransactionPort = create(db);
  equal(prepares, 0);
  const args: Record<keyof ContinuityTransactionPort, unknown[]> = {
    getNodeState: [], saveNodeState: [state], createEnrollmentAttempt: [attempt],
    getEnrollmentAttempt: [attemptID], createEnrollmentGrant: [grant],
    getEnrollmentGrant: [attemptID], recordSyncAttempt: [sync],
    recordAppliedSnapshot: [snapshot], getAppliedSnapshot: [snapshotID],
    recordSnapshotAcknowledgement: [ack], getSnapshotAcknowledgement: [node, hash],
  };
  for (const [key] of bindings) {
    const before = prepares;
    const promise = invoke(port, key, args[key]);
    equal(promise instanceof Promise, true, `${key} never throws synchronously`);
    await rejection(promise, error);
    equal(prepares, before + 1);
  }
  const before = prepares;
  const invalid: [keyof ContinuityTransactionPort, unknown[]][] = [
    ["getEnrollmentAttempt", [undefined]], ["getEnrollmentGrant", [17]],
    ["getAppliedSnapshot", [null]], ["getSnapshotAcknowledgement", [node]],
    ["saveNodeState", [{ ...state, updatedAt: new Date(NaN) }]],
    ["createEnrollmentAttempt", [{ ...attempt, createdAt: new Date(NaN) }]],
    ["createEnrollmentGrant", [{ ...grant, createdAt: new Date(NaN) }]],
    ["recordSyncAttempt", [{ ...sync, createdAt: new Date(NaN) }]],
    ["recordAppliedSnapshot", [{ ...snapshot, appliedAt: new Date(NaN) }]],
    ["recordSnapshotAcknowledgement", [{ ...ack, appliedAt: new Date(NaN) }]],
  ];
  for (const [key, values] of invalid) {
    const promise = invoke(port, key, values);
    equal(promise instanceof Promise, true);
    await assert.rejects(promise, error => error instanceof TypeError || error instanceof RangeError);
    checks++;
  }
  equal(prepares, before, "pre-SQL validation stays asynchronous");
}

async function integration(path: string, reopen: boolean) {
  const { createSQLiteContinuityTransactionPort: create } = await import("./continuity-transaction-port.js");
  const canonical: (db: Database) => ContinuityTransactionPort = create;
  await realFailures(canonical);
  const db = new Database(path, { strict: true, safeIntegers: true });
  db.exec("PRAGMA foreign_keys=ON");
  const external = new Database(path, { strict: true, safeIntegers: true });
  const port: ContinuityTransactionPort = canonical(db);
  const externalPort = create(external);
  const cached = db.query("SELECT 41 AS value");
  try {
    equal(db.inTransaction, false);
    if (!reopen) {
      const initialRaw = raw(db);
      const initialProjection = await projection(port);
      // Caller transaction and savepoint: all eleven methods see pending rows,
      // while another connection cannot. No method owns commit or rollback.
      db.exec("BEGIN; SAVEPOINT caller");
      await writeBatch(port);
      equal(db.inTransaction, true);
      await checkProjection(port);
      deep(raw(external), initialRaw);
      deep(await projection(externalPort), initialProjection);
      db.exec("ROLLBACK TO caller; RELEASE caller");
      equal(db.inTransaction, true);
      deep(raw(db), initialRaw);
      deep(await projection(port), initialProjection);
      await writeBatch(port);
      equal(db.inTransaction, true);
      db.exec("ROLLBACK");
      equal(db.inTransaction, false);
      deep(raw(db), initialRaw);
      db.exec("BEGIN");
      await writeBatch(port);
      equal(db.inTransaction, true);
      deep(raw(external), initialRaw);
      db.exec("COMMIT");
      equal(db.inTransaction, false);
      deep(raw(external), raw(db));
      deep(await projection(externalPort), await projection(port));
      // Roll back inserts and updates against already durable records.
      const durable = raw(db);
      db.exec("BEGIN; SAVEPOINT updates");
      await port.saveNodeState({ ...state, consecutiveFailures: 99 });
      await port.recordSnapshotAcknowledgement({ ...ack, snapshotVersion: 99 });
      await port.createEnrollmentAttempt({ ...attempt, id: rollbackID, state: "cancelled" });
      await port.recordSyncAttempt({ ...sync, id: rollbackID });
      await port.recordAppliedSnapshot({ ...snapshot, id: rollbackID, contentHash: "rollback" });
      await port.recordSnapshotAcknowledgement({ ...ack, contentHash: "rollback" });
      equal((await port.getNodeState())!.consecutiveFailures, 99);
      equal((await port.getSnapshotAcknowledgement(node, hash))!.snapshotVersion, 99);
      deep(raw(external), durable);
      db.exec("ROLLBACK TO updates; RELEASE updates; ROLLBACK");
      deep(raw(db), durable);
    }
    await checkProjection(port);
    equal(rows(db, "SELECT id FROM main.sync_attempts WHERE id='66666666-6666-6666-6666-666666666666'").length, 0);
    equal(db.query("SELECT 41 AS value"), cached, "caller statement cache untouched");
    deep(cached.get(), { value: 41n });
    deep(raw(db), raw(external));
    deep(await projection(port), await projection(externalPort));
    equal(db.inTransaction, false);
    // Go compares these complete raw rows and public projections across a
    // separate process reopen, not merely a second connection in this process.
    console.log(`continuity-port durable=${JSON.stringify({ raw: raw(db), projection: await projection(port) }, (_key, value) => typeof value === "bigint" ? `${value}n` : value)}`);
  } finally {
    external.close();
    db.close();
  }
}

const mode = process.argv[3];
if (mode === "mock") await forwarding();
else await integration(process.argv[2]!, mode === "reopen");
console.log(`continuity-port checks=${checks} mode=${mode}`);
