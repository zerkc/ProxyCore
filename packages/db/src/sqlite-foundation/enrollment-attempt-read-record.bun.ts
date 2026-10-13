import { strict as assert } from "node:assert";
import { Database } from "bun:sqlite";
import type { ContinuityTransactionPort, EnrollmentAttemptRecord } from "../ports.js";
import type { SQLiteEnrollmentAttemptRecord } from "./enrollment-attempt-read-record.js";
import { getSQLiteEnrollmentAttemptRecord } from "./enrollment-attempt-read-record.js";

// Compile-time linkage to the canonical port, including its Promise and alias.
const reader: (db: Database, id: string) => ReturnType<ContinuityTransactionPort["getEnrollmentAttempt"]>
  = getSQLiteEnrollmentAttemptRecord;
const canonical: (value: SQLiteEnrollmentAttemptRecord) => EnrollmentAttemptRecord = value => value;
const alias: (value: EnrollmentAttemptRecord) => SQLiteEnrollmentAttemptRecord = value => value;
void canonical;
void alias;

const path = process.argv[2];
if (!path) throw new Error("Required disposable fixture path");
function open() {
  const connection = new Database(path, { strict: true, safeIntegers: true });
  connection.exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=0");
  return connection;
}
const db = open();
const id = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const reference = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
const rawText = "  Raw 雪 ? '  ";
const fields = [
  "id", "state", "primaryUrl", "expectedPrimaryId", "verifiedPrimaryId", "verifiedPrimaryNodeId",
  "verifiedLeadershipGeneration", "verifiedPrimaryTlsSpkiSha256", "verifiedPrimaryCaFingerprint",
  "previewDigest", "localNodeIp", "archiveId", "ephemeralPrivateKeyWrapped", "bootstrapPayload",
  "nodeCredentialSecretId", "clusterKeyId", "initialSnapshotHash", "initialSnapshotRevisionId",
  "initialApplyJobId", "failureCode", "confirmedAt", "createdAt", "updatedAt",
];
const columns = [
  "id", "state", "primary_url", "expected_primary_id", "verified_primary_id", "verified_primary_node_id",
  "verified_leadership_generation", "verified_primary_tls_spki_sha256", "verified_primary_ca_fingerprint",
  "preview_digest", "local_node_ip", "archive_id", "ephemeral_private_key_wrapped", "bootstrap_payload",
  "node_credential_secret_id", "cluster_key_id", "initial_snapshot_hash", "initial_snapshot_revision_id",
  "initial_apply_job_id", "failure_code", "confirmed_at", "created_at", "updated_at",
];
const required = ["id", "state", "primaryUrl", "localNodeIp", "ephemeralPrivateKeyWrapped", "createdAt", "updatedAt"];
const optional = fields.filter(field => !required.includes(field));
const uuidColumns = ["id", "expected_primary_id", "verified_primary_id", "verified_primary_node_id",
  "archive_id", "node_credential_secret_id", "cluster_key_id", "initial_snapshot_revision_id", "initial_apply_job_id"];
const dates = ["confirmed_at", "created_at", "updated_at"];
const textColumns = columns.filter(column => column !== "verified_leadership_generation");
const states = ["draft", "verified", "confirmed", "exchanged", "archived", "initial-apply-pending",
  "committed", "cancelled", "recoverable", "failed"] as const;
const sql = `SELECT ${columns.join(", ")} FROM main.enrollment_attempts WHERE id = ? LIMIT 1`;
let checks = 0;
let scenarios = 0;
function ok(value: unknown, message?: string): asserts value {
  checks++;
  assert.ok(value, message ?? "expected truthy value");
}
function equal(actual: unknown, expected: unknown) {
  checks++;
  assert.deepStrictEqual(actual, expected);
}
async function test(name: string, body: () => void | Promise<void>) {
  await body();
  scenarios++;
  console.log(`PASS ${name}`);
}
async function rejects(body: () => unknown, expected?: unknown) {
  let error: unknown;
  try {
    await body();
  } catch (caught) {
    error = caught;
  }
  ok(error !== undefined, "expected rejection");
  if (expected !== undefined) equal(error, expected);
  return error;
}
function timestamp(seconds: bigint, nanos = 0) {
  return `1${(seconds + (1n << 63n)).toString().padStart(20, "0")}.${String(nanos).padStart(9, "0")}`;
}
function fullRow(): Record<string, unknown> {
  return {
    id, state: "failed", primary_url: rawText,
    expected_primary_id: reference, verified_primary_id: reference, verified_primary_node_id: reference,
    verified_leadership_generation: 9223372036854775807n,
    verified_primary_tls_spki_sha256: rawText, verified_primary_ca_fingerprint: rawText,
    preview_digest: rawText, local_node_ip: "not-an-ip", archive_id: reference,
    ephemeral_private_key_wrapped: "synthetic-not-key-material", bootstrap_payload: "not JSON: 雪 🦊",
    node_credential_secret_id: reference, cluster_key_id: reference,
    initial_snapshot_hash: rawText, initial_snapshot_revision_id: reference, initial_apply_job_id: reference,
    failure_code: "", confirmed_at: timestamp(0n, 1),
    created_at: timestamp(-1n, 999999999), updated_at: timestamp(1n, 123456789),
  };
}
function expectedRecord(): EnrollmentAttemptRecord {
  return {
    id, state: "failed", primaryUrl: rawText,
    expectedPrimaryId: reference, verifiedPrimaryId: reference, verifiedPrimaryNodeId: reference,
    verifiedLeadershipGeneration: Number(9223372036854775807n),
    verifiedPrimaryTlsSpkiSha256: rawText, verifiedPrimaryCaFingerprint: rawText,
    previewDigest: rawText, localNodeIp: "not-an-ip", archiveId: reference,
    ephemeralPrivateKeyWrapped: "synthetic-not-key-material",
    bootstrapPayload: new TextEncoder().encode("not JSON: 雪 🦊"),
    nodeCredentialSecretId: reference, clusterKeyId: reference,
    initialSnapshotHash: rawText, initialSnapshotRevisionId: reference, initialApplyJobId: reference,
    failureCode: "", confirmedAt: new Date(0), createdAt: new Date(-1), updatedAt: new Date(1123),
  };
}

// A narrow statement seam rejects any accidental connection or transaction operation.
function seam(result: unknown, queryError?: unknown, finalizeError?: unknown) {
  let finalized = 0;
  let prepared = 0;
  let queried = 0;
  let bound: unknown[] = [];
  const connection = {
    prepare(text: string) {
      prepared++;
      let statementFinalized = false;
      equal(text, sql);
      return {
        all(...bindings: unknown[]) {
          queried++;
          bound = bindings;
          equal(statementFinalized, false);
          if (queryError !== undefined) throw queryError;
          return result;
        },
        finalize() {
          equal(statementFinalized, false);
          statementFinalized = true;
          finalized++;
          if (finalizeError !== undefined) throw finalizeError;
        },
      };
    },
  } as unknown as Database;
  return { connection, finalized: () => finalized, prepared: () => prepared,
    queried: () => queried, bound: () => bound };
}
function nativeAll(connection = db): Record<string, unknown>[] {
  const statement = connection.prepare(`SELECT ${columns.join(", ")} FROM main.enrollment_attempts ORDER BY id`);
  try {
    return statement.all() as Record<string, unknown>[];
  } finally {
    statement.finalize();
  }
}
function update(column: string, value: string | bigint | null, connection = db) {
  ok(columns.includes(column));
  const statement = connection.prepare(`UPDATE main.enrollment_attempts SET ${column} = ? WHERE id = ?`);
  try {
    statement.run(value, id);
  } finally {
    statement.finalize();
  }
}
async function rollback(body: () => Promise<void>) {
  db.exec("SAVEPOINT reader_fixture");
  try {
    await body();
  } finally {
    db.exec("ROLLBACK TO reader_fixture; RELEASE reader_fixture");
  }
}

const initial = nativeAll();
try {
  await test("Go-migrated NULL-FK native fixture and millisecond floor", async () => {
    const result = await reader(db, id);
    ok(result);
    equal(result.primaryUrl, " literal 雪 ");
    equal(result.localNodeIp, "not-an-ip");
    equal(result.ephemeralPrivateKeyWrapped, "synthetic-wrapped-text");
    equal(result.verifiedLeadershipGeneration, Number(9223372036854775807n));
    equal(result.bootstrapPayload, new TextEncoder().encode("not JSON: 雪 🦊"));
    equal(result.createdAt.getTime(), -1);
    equal(result.updatedAt.getTime(), 0);
    equal(Object.keys(result), fields);
    for (const field of optional.filter(field => !["verifiedLeadershipGeneration", "bootstrapPayload"].includes(field))) {
      ok(Object.hasOwn(result, field));
      equal(result[field as keyof EnrollmentAttemptRecord], undefined);
    }
  });

  await test("exact 23-field canonical helper projection and literal parameter", async () => {
    const fake = seam([fullRow()]);
    const pending = reader(fake.connection, " arbitrary ' ? ");
    ok(pending instanceof Promise);
    equal(await pending, expectedRecord());
    equal(fake.bound(), [" arbitrary ' ? "]);
    equal(fake.prepared(), 1);
    equal(fake.queried(), 1);
    equal(fake.finalized(), 1);
  });

  await test("all nullable keys are present undefined, required TEXT permits empty", async () => {
    const row = fullRow();
    for (const field of optional) row[columns[fields.indexOf(field)]!] = null;
    row.primary_url = row.local_node_ip = row.ephemeral_private_key_wrapped = "";
    const result = await reader(seam([row]).connection, "literal");
    ok(result);
    equal(Object.keys(result), fields);
    equal(result.primaryUrl, "");
    equal(result.localNodeIp, "");
    equal(result.ephemeralPrivateKeyWrapped, "");
    for (const field of optional) {
      ok(Object.hasOwn(result, field));
      equal(result[field as keyof EnrollmentAttemptRecord], undefined);
    }
  });

  await test("empty and NULL bootstrap are undefined; Unicode bytes and Dates are fresh", async () => {
    for (const value of [null, "", "0", "false", "{}", " 雪 🦊 \n "]) {
      const row = fullRow();
      row.bootstrap_payload = value;
      const fake = seam([row]);
      const first = await reader(fake.connection, id);
      const second = await reader(fake.connection, id);
      ok(first && second);
      equal(first.bootstrapPayload, value ? new TextEncoder().encode(value) : undefined);
      equal(second.bootstrapPayload, first.bootstrapPayload);
      ok(first !== second);
      ok(first.createdAt !== second.createdAt);
      ok(first.updatedAt !== second.updatedAt);
      ok(first.confirmedAt !== second.confirmedAt);
      first.createdAt.setTime(999);
      equal(second.createdAt.getTime(), -1);
      if (first.bootstrapPayload && second.bootstrapPayload) {
        ok(first.bootstrapPayload !== second.bootstrapPayload);
        first.bootstrapPayload[0] = 255;
        equal(second.bootstrapPayload, new TextEncoder().encode(value!));
      }
    }
  });

  await test("all ten states project without extra product narrowing", async () => {
    for (const state of states) {
      const row = fullRow();
      row.state = state;
      equal((await reader(seam([row]).connection, id))?.state, state);
      await rollback(async () => {
        update("state", state);
        equal((await reader(db, id))?.state, state);
      });
    }
  });

  await test("signed int64 extremes and represented rounding follow read policy", async () => {
    for (const value of [-(1n << 63n), -9007199254740993n, -1n, 0n, 1n,
      9007199254740993n, (1n << 63n) - 1n]) {
      const row = fullRow();
      row.verified_leadership_generation = value;
      equal((await reader(seam([row]).connection, id))?.verifiedLeadershipGeneration, Number(value));
      await rollback(async () => {
        update("verified_leadership_generation", value);
        equal((await reader(db, id))?.verifiedLeadershipGeneration, Number(value));
      });
    }
    await rollback(async () => {
      update("verified_leadership_generation", null);
      const record = await reader(db, id);
      ok(record && Object.hasOwn(record, "verifiedLeadershipGeneration"));
      equal(record.verifiedLeadershipGeneration, undefined);
    });
  });

  await test("canonical finite timestamp floor and Date range endpoints", async () => {
    const cases: [string, number][] = [
      [timestamp(0n, 999999), 0], [timestamp(0n, 1000000), 1],
      [timestamp(-1n, 999999999), -1], [timestamp(-1n, 1), -1000],
      [timestamp(8640000000000n), 8640000000000000],
      [timestamp(-8640000000000n), -8640000000000000],
    ];
    for (const [key, expected] of cases) {
      for (const column of dates) {
        const row = fullRow();
        row[column] = key;
        const record = await reader(seam([row]).connection, id);
        ok(record);
        const projected = record[fields[columns.indexOf(column)] as keyof EnrollmentAttemptRecord];
        ok(projected instanceof Date);
        equal(projected.getTime(), expected);
      }
    }
  });

  await test("infinities and canonical out-of-Date-range keys yield Invalid Date", async () => {
    for (const key of ["0", "2", timestamp(8640000000000n, 1000000),
      timestamp(-8640000000001n, 999999999), timestamp((1n << 63n) - 1n), timestamp(-(1n << 63n))]) {
      for (const column of dates) {
        const row = fullRow();
        row[column] = key;
        const result = await reader(seam([row]).connection, id);
        ok(result);
        const projected = result[fields[columns.indexOf(column)] as keyof EnrollmentAttemptRecord];
        ok(projected instanceof Date && Number.isNaN(projected.getTime()));
        await rollback(async () => {
          update(column, key);
          const native = await reader(db, id);
          ok(native);
          const date = native[fields[columns.indexOf(column)] as keyof EnrollmentAttemptRecord];
          ok(date instanceof Date && Number.isNaN(date.getTime()));
        });
      }
    }
  });

  await test("malformed timestamp keys reject rather than signal absence", async () => {
    for (const key of ["", "infinity", "-infinity", "1", "2026-01-01", " 0", "2\n",
      "118446744073709551616.000000000", "109223372036854775808.00000000x",
      "109223372036854775808.000000000\n", "109223372036854775808.00000000",
      "109223372036854775808.0000000000", "109223372036854775808,000000000"]) {
      for (const column of dates) {
        const row = fullRow();
        row[column] = key;
        const fake = seam([row]);
        await rejects(() => reader(fake.connection, id));
        equal(fake.finalized(), 1);
      }
    }
    // The migrated schema rejects malformed keys before a reader can observe them.
    await rollback(async () => {
      checks++;
      assert.throws(() => update("created_at", "malformed"), /CHECK constraint failed/);
      equal((await reader(db, id))?.createdAt.getTime(), -1);
    });
  });

  await test("literal TEXT lookup never normalizes UUID or interpolates SQL", async () => {
    for (const lookup of ["missing", "", id.toUpperCase(), `{${id}}`, id.replaceAll("-", ""),
      `${id}\n`, ` ${id}`, `${id} `, "' OR 1=1 --", "\0", "not-a-uuid"]) {
      equal(await reader(db, lookup), undefined);
      const fake = seam([]);
      equal(await reader(fake.connection, lookup), undefined);
      equal(fake.bound(), [lookup]);
      equal(fake.finalized(), 1);
    }
    equal((await reader(db, id))?.id, id);
    for (const input of [null, undefined, 42, 1n, {}, [], new Uint8Array()]) {
      const fake = seam([]);
      const error = await rejects(() => reader(fake.connection, input as unknown as string));
      ok(error instanceof TypeError);
      equal(fake.prepared(), 0);
    }
  });

  await test("required stored TEXT and nullable TEXT types are validated", async () => {
    for (const column of textColumns) {
      for (const invalid of [undefined, 4, 4n, false, {}, [], new Uint8Array([65]), "bad\0text"]) {
        const row = fullRow();
        row[column] = invalid;
        const fake = seam([row]);
        await rejects(() => reader(fake.connection, id));
        equal(fake.finalized(), 1);
      }
    }
    for (const column of ["id", "state", "primary_url", "local_node_ip", "ephemeral_private_key_wrapped",
      "created_at", "updated_at"]) {
      const row = fullRow();
      row[column] = null;
      await rejects(() => reader(seam([row]).connection, id));
    }
    for (const state of ["", "Failed", "unknown", " failed", "failed\n"]) {
      const row = fullRow();
      row.state = state;
      await rejects(() => reader(seam([row]).connection, id));
    }
  });

  await test("UUID shape validation without version or variant narrowing", async () => {
    for (const column of uuidColumns) {
      for (const invalid of ["", reference.toUpperCase(), reference.replaceAll("-", ""),
        `{${reference}}`, `${reference}\n`, reference.replace("b", "g"), "0".repeat(36)]) {
        const row = fullRow();
        row[column] = invalid;
        await rejects(() => reader(seam([row]).connection, id));
      }
      for (const valid of ["00000000-0000-0000-0000-000000000000", "ffffffff-ffff-ffff-ffff-ffffffffffff"]) {
        const row = fullRow();
        row[column] = valid;
        const result = await reader(seam([row]).connection, id);
        ok(result);
        equal(result[fields[columns.indexOf(column)] as keyof EnrollmentAttemptRecord], valid);
      }
    }
  });

  await test("integer type and int64 bounds reject malformed stored values", async () => {
    for (const invalid of [undefined, 1, 1.5, NaN, Infinity, "1", false,
      new Uint8Array([1]), -(1n << 63n) - 1n, 1n << 63n]) {
      const row = fullRow();
      row.verified_leadership_generation = invalid;
      await rejects(() => reader(seam([row]).connection, id));
    }
  });

  await test("only an empty row array is absence; malformed results reject", async () => {
    for (const result of [null, undefined, {}, false, "", [null], [undefined], [[]], ["row"],
      [fullRow(), fullRow()], [{ ...fullRow(), extra: null }]]) {
      const fake = seam(result);
      await rejects(() => reader(fake.connection, id));
      equal(fake.finalized(), 1);
    }
    for (const column of columns) {
      const row = fullRow();
      delete row[column];
      const fake = seam([row]);
      await rejects(() => reader(fake.connection, id));
      equal(fake.finalized(), 1);
    }
  });

  await test("prepare query and finalize failures preserve error identity", async () => {
    const prepareError = new Error("synthetic prepare failure");
    const prepareFault = { prepare() { throw prepareError; } } as unknown as Database;
    await rejects(() => reader(prepareFault, id), prepareError);
    const queryError = new Error("synthetic query failure");
    const queryFault = seam([], queryError);
    await rejects(() => reader(queryFault.connection, id), queryError);
    equal(queryFault.finalized(), 1);
    const finalizeError = new Error("synthetic finalize failure");
    for (const result of [[], [fullRow()]]) {
      const finalizeFault = seam(result, undefined, finalizeError);
      await rejects(() => reader(finalizeFault.connection, id), finalizeError);
      equal(finalizeFault.finalized(), 1);
    }
    const doubleFault = seam([], queryError, finalizeError);
    await rejects(() => reader(doubleFault.connection, id), finalizeError);
    equal(doubleFault.finalized(), 1);
  });

  await test("statement finalization happens before any row projection or publication", async () => {
    const getterError = new Error("synthetic projection getter failure");
    let touched = 0;
    const row = fullRow();
    const fake = seam([row]);
    Object.defineProperty(row, "state", { enumerable: true, get() {
      touched++;
      equal(fake.finalized(), 1);
      throw getterError;
    } });
    await rejects(() => reader(fake.connection, id), getterError);
    equal(touched, 1);
    equal(fake.finalized(), 1);
    const untouched = fullRow();
    Object.defineProperty(untouched, "state", { enumerable: true, get() {
      throw new Error("projection must not run after failed finalize");
    } });
    const finalizerError = new Error("synthetic finalize before projection");
    await rejects(() => reader(seam([untouched], undefined, finalizerError).connection, id), finalizerError);
  });

  await test("real native statements finalize on success absence and thrown query", async () => {
    const cached = db.query("SELECT primary_url FROM main.enrollment_attempts WHERE id = ?");
    for (const mode of ["success", "absence", "query-fault", "finalize-fault", "projection-fault"] as const) {
      let finalized = 0;
      const fault = new Error(`synthetic native ${mode}`);
      let owned: ReturnType<Database["prepare"]> | undefined;
      const wrapped = {
        prepare(text: string) {
          equal(text, sql);
          const native = db.prepare(text);
          owned = native;
          return {
            all(value: string) {
              const rows = native.all(value);
              if (mode === "query-fault") throw fault;
              if (mode === "projection-fault") {
                const row = rows[0] as Record<string, unknown>;
                Object.defineProperty(row, "state", { enumerable: true, get() {
                  equal(finalized, 1);
                  throw fault;
                } });
              }
              return rows;
            },
            finalize() {
              native.finalize();
              finalized++;
              if (mode === "finalize-fault") throw fault;
            },
          };
        },
      } as unknown as Database;
      if (mode.endsWith("fault")) {
        await rejects(() => reader(wrapped, id), fault);
      } else {
        const result = await reader(wrapped, mode === "absence" ? "missing" : id);
        equal(result?.id, mode === "absence" ? undefined : id);
      }
      equal(finalized, 1);
      ok(owned);
      checks++;
      assert.throws(() => owned!.all(id));
      equal(cached.get(id), { primary_url: " literal 雪 " });
      equal((await reader(db, id))?.id, id);
    }
    // Only the test owner finalizes the caller's cached statement.
    cached.finalize();
  });

  await test("MAIN qualification ignores a same-named TEMP table", async () => {
    db.exec("CREATE TEMP TABLE enrollment_attempts(id TEXT, primary_url TEXT)");
    db.exec("INSERT INTO temp.enrollment_attempts VALUES('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa','shadow')");
    equal((await reader(db, id))?.primaryUrl, " literal 雪 ");
  });

  await test("caller transaction cache and savepoints survive reads and errors", async () => {
    const external = open();
    const cached = db.query("SELECT primary_url FROM main.enrollment_attempts WHERE id = ?");
    db.exec("BEGIN");
    try {
      update("primary_url", "pending caller value");
      ok(db.inTransaction);
      equal((await reader(db, id))?.primaryUrl, "pending caller value");
      equal((await reader(external, id))?.primaryUrl, " literal 雪 ");
      db.exec("SAVEPOINT caller_owned");
      update("primary_url", "savepoint value");
      equal((await reader(db, id))?.primaryUrl, "savepoint value");
      checks++;
      assert.throws(() => update("state", "unknown"), /enrollment_attempts_state_check/);
      const queryError = new Error("synthetic query failure inside caller savepoint");
      let finalized = 0;
      const fault = {
        prepare(text: string) {
          equal(text, sql);
          const statement = db.prepare(text);
          return {
            all(value: string) {
              statement.all(value);
              throw queryError;
            },
            finalize() {
              statement.finalize();
              finalized++;
            },
          };
        },
      } as unknown as Database;
      await rejects(() => reader(fault, id), queryError);
      equal(finalized, 1);
      ok(db.inTransaction);
      equal(cached.get(id), { primary_url: "savepoint value" });
      db.exec("ROLLBACK TO caller_owned; RELEASE caller_owned");
      equal((await reader(db, id))?.primaryUrl, "pending caller value");
      equal((await reader(db, id))?.state, "failed");
      equal(await reader(db, "missing"), undefined);
      ok(db.inTransaction);
      db.exec("ROLLBACK");
      equal((await reader(db, id))?.primaryUrl, " literal 雪 ");
      equal((await reader(external, id))?.primaryUrl, " literal 雪 ");
      equal(cached.get(id), { primary_url: " literal 雪 " });
    } finally {
      if (db.inTransaction) db.exec("ROLLBACK");
      cached.finalize();
      external.close();
    }
  });

  await test("external visibility follows caller COMMIT; reopen preserves all native fields", async () => {
    const external = open();
    try {
      db.exec("BEGIN");
      update("primary_url", "caller committed");
      equal((await reader(external, id))?.primaryUrl, " literal 雪 ");
      await reader(db, id);
      ok(db.inTransaction);
      db.exec("COMMIT");
      equal((await reader(external, id))?.primaryUrl, "caller committed");
      const reopened = open();
      try {
        equal(nativeAll(reopened), nativeAll());
        equal((await reader(reopened, id))?.primaryUrl, "caller committed");
      } finally {
        reopened.close();
      }
      // Restore the synthetic fixture with caller-owned SQL, not adapter writes.
      db.exec("BEGIN");
      update("primary_url", " literal 雪 ");
      db.exec("COMMIT");
    } finally {
      if (db.inTransaction) db.exec("ROLLBACK");
      external.close();
    }
  });

  await test("native TEXT INTEGER and BLOB corruption rejected on permissive synthetic table", async () => {
    const corrupt = new Database(":memory:", { strict: true, safeIntegers: true });
    try {
      corrupt.exec(`CREATE TABLE enrollment_attempts (${columns.map(column => `${column}`).join(", ")})`);
      const insert = corrupt.prepare(`INSERT INTO enrollment_attempts (${columns.join(", ")}) VALUES (${columns.map(() => "?").join(", ")})`);
      try {
        const row = fullRow();
        insert.run(...columns.map(column => row[column] as string | bigint));
      } finally {
        insert.finalize();
      }
      equal(await reader(corrupt, id), expectedRecord());
      const cases: [string, string | bigint | Uint8Array][] = [
        ["primary_url", 42n], ["bootstrap_payload", new Uint8Array([65])],
        ["verified_leadership_generation", "42"], ["state", "unknown"],
        ["expected_primary_id", "bad-uuid"], ["updated_at", "malformed"],
        ["created_at", "malformed"], ["confirmed_at", "malformed"],
      ];
      for (const [column, value] of cases) {
        corrupt.exec("SAVEPOINT corruption_case");
        const statement = corrupt.prepare(`UPDATE enrollment_attempts SET ${column} = ?`);
        try {
          statement.run(value);
          await rejects(() => reader(corrupt, id));
        } finally {
          statement.finalize();
          corrupt.exec("ROLLBACK TO corruption_case; RELEASE corruption_case");
        }
        equal(await reader(corrupt, id), expectedRecord());
      }
    } finally {
      corrupt.close();
    }
  });

  await test("native empty bootstrap NULLs and detached mutations never change stored rows", async () => {
    for (const value of [null, "", " 雪 🦊 "]) {
      await rollback(async () => {
        update("bootstrap_payload", value);
        const before = nativeAll();
        const first = await reader(db, id);
        const second = await reader(db, id);
        ok(first && second);
        equal(first.bootstrapPayload, value ? new TextEncoder().encode(value) : undefined);
        first.primaryUrl = "local mutation";
        first.createdAt.setTime(123);
        first.updatedAt.setTime(456);
        if (first.bootstrapPayload) first.bootstrapPayload.fill(0);
        equal(second.primaryUrl, " literal 雪 ");
        equal(second.createdAt.getTime(), -1);
        equal(second.updatedAt.getTime(), 0);
        equal(second.bootstrapPayload, value ? new TextEncoder().encode(value) : undefined);
        equal(nativeAll(), before);
      });
    }
    equal(nativeAll(), initial);
    const reopened = open();
    try {
      equal(nativeAll(reopened), initial);
      equal((await reader(reopened, id))?.primaryUrl, " literal 雪 ");
    } finally {
      reopened.close();
    }
    ok(!db.inTransaction);
  });

  console.log(`enrollment-attempt-read checks=${checks} scenarios=${scenarios}`);
} finally {
  if (db.inTransaction) db.exec("ROLLBACK");
  db.close();
}
