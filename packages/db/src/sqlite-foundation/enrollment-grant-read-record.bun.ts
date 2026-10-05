import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import { getSQLiteEnrollmentGrantRecord } from "./enrollment-grant-read-record.js";
import type { SQLiteEnrollmentGrantRecord } from "./enrollment-grant-read-record.js";
import type { ContinuityTransactionPort, EnrollmentGrantRecord } from "../ports.js";

// Compile-time linkage to the canonical transaction port and record declaration.
const read: (db: Database, id: string) => ReturnType<ContinuityTransactionPort["getEnrollmentGrant"]>
  = getSQLiteEnrollmentGrantRecord;
type Equal<A, B> = (<T>() => T extends A ? 1 : 2) extends
  (<T>() => T extends B ? 1 : 2) ? true : false;
const linked: Equal<SQLiteEnrollmentGrantRecord, EnrollmentGrantRecord> = true;

const path = process.argv[2];
function open() {
  const connection = new Database(path, { strict: true, safeIntegers: true });
  connection.exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=0");
  return connection;
}
const db = open();
const id = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const reference = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
const token = "cccccccc-cccc-cccc-cccc-cccccccccccc";
const missing = "ffffffff-ffff-ffff-ffff-ffffffffffff";
const columns = [
  "attempt_id", "token_id", "installation_id", "node_id", "primary_id",
  "primary_generation", "verified_preview_digest", "node_ephemeral_public_key",
  "sealed_bootstrap_payload", "payload_hash", "created_at", "expires_at",
];
const fields = [
  "attemptId", "tokenId", "installationId", "nodeId", "primaryId",
  "primaryGeneration", "verifiedPreviewDigest", "nodeEphemeralPublicKey",
  "sealedBootstrapPayload", "payloadHash", "createdAt", "expiresAt",
];
const uuidColumns = columns.slice(0, 5);
const textColumns = [
  "verified_preview_digest", "node_ephemeral_public_key", "sealed_bootstrap_payload", "payload_hash",
];
const created = "109223372036854775807.999000000";
const expires = "109223372036854775808.001000000";
const expected: EnrollmentGrantRecord = {
  attemptId: id,
  tokenId: token,
  installationId: reference,
  nodeId: reference,
  primaryId: reference,
  primaryGeneration: Number(9007199254740993n),
  verifiedPreviewDigest: "  digest 雪 🦊  ",
  nodeEphemeralPublicKey: "  public-key 雪  ",
  sealedBootstrapPayload: "  sealed-payload 雪  ",
  payloadHash: "  payload-hash 雪  ",
  createdAt: new Date(-1),
  expiresAt: new Date(1),
};
function rawRow(): Record<string, unknown> {
  return {
    attempt_id: id,
    token_id: token,
    installation_id: reference,
    node_id: reference,
    primary_id: reference,
    primary_generation: 9007199254740993n,
    verified_preview_digest: "  digest 雪 🦊  ",
    node_ephemeral_public_key: "  public-key 雪  ",
    sealed_bootstrap_payload: "  sealed-payload 雪  ",
    payload_hash: "  payload-hash 雪  ",
    created_at: created,
    expires_at: expires,
  };
}
let checks = 0, scenarios = 0;
function equal(actual: unknown, wanted: unknown) {
  assert.deepEqual(actual, wanted);
  checks++;
}
function ok(value: unknown) {
  assert.ok(value);
  checks++;
}
async function rejects(body: () => Promise<unknown>, wanted?: Error | RegExp) {
  await assert.rejects(body, error => {
    if (wanted instanceof Error) assert.equal(error, wanted);
    else if (wanted) assert.match(String(error), wanted);
    return true;
  });
  checks++;
}
async function test(name: string, body: () => Promise<void>) {
  await body();
  scenarios++;
  console.log(`PASS ${name}`);
}
function values(connection = db): unknown[][] {
  const statement = connection.prepare(`SELECT ${columns.join(", ")} FROM main.enrollment_grants ORDER BY attempt_id`);
  try {
    return statement.values();
  } finally {
    statement.finalize();
  }
}
const initial = values();

// Per-owned-statement observations, never accumulated finalization counters.
type Owned = { gets: number; finalizes: number; args: unknown[]; closed: boolean };
type Faults = { prepare?: Error; get?: Error; finalize?: Error };
function facade(value?: unknown, faults: Faults = {}, native = false) {
  const owned: Owned[] = [];
  const sql: string[] = [];
  const connection = {
    prepare(query: string) {
      sql.push(query);
      if (faults.prepare) throw faults.prepare;
      const statement = native ? db.prepare(query) : undefined;
      const observation: Owned = { gets: 0, finalizes: 0, args: [], closed: false };
      owned.push(observation);
      return {
        get(...args: any[]) {
          observation.gets++;
          observation.args = args;
          if (faults.get) throw faults.get;
          return statement ? statement.get(...args) : value;
        },
        finalize() {
          observation.finalizes++;
          statement?.finalize();
          observation.closed = true;
          if (faults.finalize) throw faults.finalize;
        },
      };
    },
    query() { assert.fail("reader must not access caller statement cache"); },
    exec() { assert.fail("reader must not write or own transaction SQL"); },
    close() { assert.fail("reader must not close caller database"); },
    transaction() { assert.fail("reader must not own transactions"); },
    get inTransaction() { return assert.fail("reader must not guard caller transaction"); },
  } as unknown as Database;
  return { connection, owned, sql };
}
function finished(seam: ReturnType<typeof facade>, lookup = id) {
  equal(seam.owned.length, 1);
  equal(seam.owned[0], { gets: 1, finalizes: 1, args: [lookup], closed: true });
}
async function project(row: unknown) {
  const seam = facade(row);
  const result = await read(seam.connection, id);
  finished(seam);
  return result;
}
async function invalid(row: unknown, wanted?: RegExp) {
  const seam = facade(row);
  await rejects(() => read(seam.connection, id), wanted);
  finished(seam);
}
function timestamp(seconds: bigint, nanos = 0) {
  return `1${(seconds + (1n << 63n)).toString().padStart(20, "0")}.${nanos.toString().padStart(9, "0")}`;
}
async function nativeChange(key: string, value: unknown, body: () => Promise<void>) {
  ok(columns.includes(key));
  db.exec("SAVEPOINT reader_case");
  try {
    const statement = db.prepare(`UPDATE main.enrollment_grants SET ${key} = ? WHERE attempt_id = ?`);
    try {
      statement.run(value as any, id);
    } finally {
      statement.finalize();
    }
    await body();
  } finally {
    db.exec("ROLLBACK TO reader_case; RELEASE reader_case");
  }
  equal(values(), initial);
}

try {
  await test("all twelve canonical fields and type linkage", async () => {
    equal(linked, true);
    equal(initial.length, 1);
    const grant = await read(db, id);
    equal(grant, expected);
    equal(Object.keys(grant!).sort(), [...fields].sort());
    equal(values(), initial);
    const again = await read(db, id);
    equal(again, expected);
    ok(again !== grant);
    ok(again!.createdAt !== grant!.createdAt);
    ok(again!.expiresAt !== grant!.expiresAt);
  });
  await test("one explicit MAIN SELECT with literal attempt binding", async () => {
    const seam = facade(rawRow(), {}, true);
    equal(await read(seam.connection, id), expected);
    equal(seam.sql, [`SELECT ${columns.join(", ")} FROM main.enrollment_grants WHERE attempt_id = ? LIMIT 1`]);
    ok(!/SELECT \*|WHERE token_id|BEGIN|COMMIT|INSERT|UPDATE|DELETE|RETURNING/i.test(seam.sql[0]));
    finished(seam);
    equal(values(), initial);
  });
  await test("absence only and no token-ID lookup", async () => {
    for (const lookup of [missing, token, reference, "", "not-a-uuid"]) {
      equal(await read(db, lookup), undefined);
      const seam = facade(null, {}, true);
      equal(await read(seam.connection, lookup), undefined);
      finished(seam, lookup);
    }
  });
  await test("UUID alternate spellings remain literal, never normalized", async () => {
    const alternate = [id.toUpperCase(), `{${id}}`, id.replaceAll("-", ""), ` ${id}`, `${id} `, `${id}\n`, `${id}\0`];
    for (const lookup of alternate) {
      equal(await read(db, lookup), undefined);
      const seam = facade(null);
      equal(await read(seam.connection, lookup), undefined);
      finished(seam, lookup);
    }
    equal(await read(db, `${id}' OR 1=1 --`), undefined);
    equal(await read(db, id), expected);
  });
  await test("non-string lookup rejects before prepare", async () => {
    for (const lookup of [null, undefined, 1, 1n, {}, [], new String(id)]) {
      const seam = facade();
      await rejects(() => read(seam.connection, lookup as string), /must be a string/);
      equal(seam.sql, []);
      equal(seam.owned, []);
    }
  });
  await test("NULL preview publishes own undefined key", async () => {
    await nativeChange("verified_preview_digest", null, async () => {
      const grant = await read(db, id);
      equal(grant, { ...expected, verifiedPreviewDigest: undefined });
      ok(Object.hasOwn(grant!, "verifiedPreviewDigest"));
      equal(Object.keys(grant!).length, 12);
    });
    equal(await project({ ...rawRow(), verified_preview_digest: null }), {
      ...expected, verifiedPreviewDigest: undefined,
    });
  });
  await test("empty and Unicode preview are exact TEXT, never truthiness-filtered", async () => {
    for (const digest of ["", "雪 🦊", "  spaced  ", "e\u0301", "\n\r\t"]) {
      await nativeChange("verified_preview_digest", digest, async () => {
        equal((await read(db, id))!.verifiedPreviewDigest, digest);
      });
      equal((await project({ ...rawRow(), verified_preview_digest: digest }))!.verifiedPreviewDigest, digest);
    }
  });
  await test("non-UUID required TEXT allows empty and exact Unicode", async () => {
    for (const key of textColumns.slice(1)) {
      const field = fields[columns.indexOf(key)] as keyof EnrollmentGrantRecord;
      for (const text of ["", "  雪 🦊 e\u0301 \n  "]) {
        await nativeChange(key, text, async () => {
          equal((await read(db, id))![field], text);
        });
      }
    }
  });
  await test("signed int64 extrema, negatives, zero and intentional rounding", async () => {
    const generations = [
      -(1n << 63n), -(9007199254740993n), -1n, 0n, 1n,
      9007199254740991n, 9007199254740992n, 9007199254740993n,
      (1n << 63n) - 1n,
    ];
    for (const generation of generations) {
      await nativeChange("primary_generation", generation, async () => {
        equal((await read(db, id))!.primaryGeneration, Number(generation));
      });
      equal((await project({ ...rawRow(), primary_generation: generation }))!.primaryGeneration, Number(generation));
    }
    equal(Number(9007199254740993n), 9007199254740992);
    equal(Number((1n << 63n) - 1n), 9223372036854775808);
  });
  await test("infinities remain native keys but public Invalid Dates", async () => {
    await nativeChange("created_at", "0", async () => {
      ok(Number.isNaN((await read(db, id))!.createdAt.getTime()));
      equal(values()[0][10], "0");
    });
    await nativeChange("expires_at", "2", async () => {
      ok(Number.isNaN((await read(db, id))!.expiresAt.getTime()));
      equal(values()[0][11], "2");
    });
    for (const key of ["created_at", "expires_at"]) {
      for (const value of ["0", "2"]) {
        const grant = await project({ ...rawRow(), [key]: value });
        const date = key === "created_at" ? grant!.createdAt : grant!.expiresAt;
        ok(Number.isNaN(date.getTime()));
      }
    }
  });
  await test("canonical finite timestamp millisecond truncation and negative epoch", async () => {
    const cases: [bigint, number, number][] = [
      [-1n, 0, -1000], [-1n, 999999999, -1], [0n, 1, 0],
      [0n, 999999, 0], [0n, 1000000, 1], [0n, 999999999, 999],
      [1n, 123456789, 1123],
    ];
    for (const [seconds, nanos, milliseconds] of cases) {
      for (const key of ["created_at", "expires_at"]) {
        const grant = await project({ ...rawRow(), [key]: timestamp(seconds, nanos) });
        equal((key === "created_at" ? grant!.createdAt : grant!.expiresAt).getTime(), milliseconds);
      }
    }
  });
  await test("Date TimeClip bounds retain valid endpoints, reject neither canonical int64 extreme", async () => {
    const cases: [bigint, number, number | undefined][] = [
      [-8640000000000n, 0, -8640000000000000],
      [8640000000000n, 0, 8640000000000000],
      [-8640000000000n, 999999, -8640000000000000],
      [8640000000000n, 999999, 8640000000000000],
      [-8640000000001n, 999999999, undefined],
      [8640000000000n, 1000000, undefined],
      [-(1n << 63n), 0, undefined],
      [(1n << 63n) - 1n, 999999999, undefined],
    ];
    for (const [seconds, nanos, milliseconds] of cases) {
      for (const key of ["created_at", "expires_at"]) {
        const grant = await project({ ...rawRow(), [key]: timestamp(seconds, nanos) });
        const value = (key === "created_at" ? grant!.createdAt : grant!.expiresAt).getTime();
        if (milliseconds === undefined) ok(Number.isNaN(value));
        else equal(value, milliseconds);
      }
    }
    await nativeChange("created_at", timestamp(-(1n << 63n)), async () => {
      ok(Number.isNaN((await read(db, id))!.createdAt.getTime()));
    });
    await nativeChange("expires_at", timestamp((1n << 63n) - 1n, 999999999), async () => {
      ok(Number.isNaN((await read(db, id))!.expiresAt.getTime()));
    });
  });
  await test("stored UUID shape validation on every identifier", async () => {
    for (const key of uuidColumns) {
      for (const value of [null, undefined, 1n, 1, new Uint8Array([1]), "", reference.toUpperCase(),
        reference.replaceAll("-", ""), `{${reference}}`, `${reference}\n`, `${reference}\0`,
        "gggggggg-gggg-gggg-gggg-gggggggggggg", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb-"]) {
        await invalid({ ...rawRow(), [key]: value }, /Invalid (TEXT|stored UUID)/);
      }
      // No version/variant narrowing beyond the canonical stored shape.
      const zero = "00000000-0000-0000-0000-000000000000";
      const grant = await project({ ...rawRow(), [key]: zero });
      equal(grant![fields[columns.indexOf(key)] as keyof EnrollmentGrantRecord], zero);
    }
  });
  await test("required TEXT and nullable preview reject bad stored types and NUL", async () => {
    for (const key of textColumns) {
      for (const value of [undefined, 0, 0n, false, {}, [], new Uint8Array([65]), "before\0after"]) {
        await invalid({ ...rawRow(), [key]: value }, /Invalid TEXT/);
      }
      if (key !== "verified_preview_digest") await invalid({ ...rawRow(), [key]: null }, /Invalid TEXT/);
    }
  });
  await test("INTEGER validation does not accept number, float, TEXT or out-of-int64 bigint", async () => {
    for (const value of [null, undefined, 0, 1, -1, 1.5, NaN, Infinity, "1", {}, [],
      -(1n << 63n) - 1n, 1n << 63n]) {
      await invalid({ ...rawRow(), primary_generation: value }, /Invalid INTEGER/);
    }
  });
  await test("malformed canonical timestamp syntax rejects for both date fields", async () => {
    const malformed = [
      null, undefined, 0n, 0, new Date(0), "", "1970-01-01T00:00:00.000Z",
      "infinity", "-infinity", "1", `${created}\n`, `${created}\r`, `${created}\0`,
      created.replace(".", ","), created.slice(0, -1), `${created}0`,
      "118446744073709551616.000000000", "1-9223372036854775808.000000000",
      "109223372036854775808.00000000x",
    ];
    for (const key of ["created_at", "expires_at"]) {
      for (const value of malformed) await invalid({ ...rawRow(), [key]: value });
    }
  });
  await test("malformed rows are not absence, exact twelve columns are required", async () => {
    for (const row of [undefined, false, 0, "", [], [rawRow()], new Date(0), {},
      { ...rawRow(), unexpected: "extra" }]) {
      await invalid(row, /Invalid enrollment grant/);
    }
    for (const key of columns) {
      const row = rawRow();
      delete row[key];
      await invalid(row, /Invalid enrollment grant columns/);
    }
    const inherited = Object.create(rawRow());
    await invalid(inherited, /Invalid enrollment grant columns/);
  });
  await test("native constraints reject malformed fixtures before reader; no schema weakening", async () => {
    const malformed: [string, unknown][] = [
      ["installation_id", "not-uuid"], ["node_ephemeral_public_key", "NUL\0text"],
      ["verified_preview_digest", "NUL\0text"], ["primary_generation", 1.5],
      ["created_at", "malformed"], ["expires_at", "malformed"],
    ];
    for (const [key, value] of malformed) {
      const statement = db.prepare(`UPDATE main.enrollment_grants SET ${key} = ? WHERE attempt_id = ?`);
      try {
        assert.throws(() => statement.run(value as any, id), /CHECK|constraint|INTEGER/i);
        checks++;
      } finally {
        statement.finalize();
      }
      equal(await read(db, id), expected);
      equal(values(), initial);
    }
  });
  await test("permissive synthetic native table exercises reader corruption without disabling constraints", async () => {
    const synthetic = new Database(":memory:", { strict: true, safeIntegers: true });
    try {
      synthetic.exec(`CREATE TABLE enrollment_grants (${columns.map(key => `${key}`).join(", ")})`);
      const insert = synthetic.prepare(`INSERT INTO enrollment_grants (${columns.join(", ")}) VALUES (${columns.map(() => "?").join(", ")})`);
      try {
        insert.run(...columns.map(key => rawRow()[key]) as any[]);
      } finally {
        insert.finalize();
      }
      equal(await read(synthetic, id), expected);
      const cases: [string, unknown][] = [
        ["node_id", "bad UUID"], ["installation_id", 1n],
        ["primary_generation", "123"], ["primary_generation", 1.25],
        ["verified_preview_digest", new Uint8Array([65])],
        ["node_ephemeral_public_key", null], ["payload_hash", "NUL\0text"],
        ["created_at", "malformed"], ["expires_at", "malformed"],
      ];
      for (const [key, value] of cases) {
        synthetic.exec("SAVEPOINT malformed_reader");
        try {
          const update = synthetic.prepare(`UPDATE enrollment_grants SET ${key} = ?`);
          try { update.run(value as any); } finally { update.finalize(); }
          await rejects(() => read(synthetic, id));
        } finally {
          synthetic.exec("ROLLBACK TO malformed_reader; RELEASE malformed_reader");
        }
        equal(await read(synthetic, id), expected);
      }
    } finally {
      synthetic.close();
    }
  });
  await test("prepare error identity without pretending an owned statement exists", async () => {
    const error = new Error("synthetic prepare failure");
    const seam = facade(rawRow(), { prepare: error });
    await rejects(() => read(seam.connection, id), error);
    equal(seam.sql.length, 1);
    equal(seam.owned, []);
  });
  await test("query error identity and exactly one owned finalization", async () => {
    const error = new Error("synthetic query failure");
    const seam = facade(rawRow(), { get: error });
    await rejects(() => read(seam.connection, id), error);
    finished(seam);
    equal(await read(db, id), expected);
  });
  await test("finalizer failure identity for row and absence; priority when query also fails", async () => {
    const error = new Error("synthetic finalizer failure");
    for (const value of [rawRow(), null]) {
      const seam = facade(value, { finalize: error });
      await rejects(() => read(seam.connection, id), error);
      finished(seam);
    }
    const seam = facade(rawRow(), { get: new Error("synthetic query failure"), finalize: error });
    await rejects(() => read(seam.connection, id), error);
    finished(seam);
  });
  await test("native finalizer completes before injected rejection and caller can query again", async () => {
    const error = new Error("synthetic post-native-finalize failure");
    for (const lookup of [id, missing]) {
      const seam = facade(rawRow(), { finalize: error }, true);
      await rejects(() => read(seam.connection, lookup), error);
      finished(seam, lookup);
      equal(await read(db, id), expected);
      equal(values(), initial);
    }
  });
  await test("real native query failure in caller savepoint preserves transaction and finalizes", async () => {
    db.exec("BEGIN; SAVEPOINT caller_error");
    let error: unknown;
    let observation: Owned | undefined;
    const connection = {
      prepare() {
        const statement = db.prepare("SELECT ? AS failure");
        observation = { gets: 0, finalizes: 0, args: [], closed: false };
        return {
          get(value: string) {
            observation!.gets++;
            observation!.args = [value];
            try { return statement.get(value, value); }
            catch (caught) { error = caught; throw caught; }
          },
          finalize() {
            observation!.finalizes++;
            statement.finalize();
            observation!.closed = true;
          },
        };
      },
    } as unknown as Database;
    try {
      await assert.rejects(() => read(connection, id), caught => caught === error);
      checks++;
      ok(error instanceof Error);
      equal(observation, { gets: 1, finalizes: 1, args: [id], closed: true });
      equal(db.inTransaction, true);
      equal(await read(db, id), expected);
      db.exec("ROLLBACK TO caller_error; RELEASE caller_error; ROLLBACK");
    } finally {
      if (db.inTransaction) db.exec("ROLLBACK");
    }
  });
  await test("finalize precedes every projection property read and projection-error publication", async () => {
    let seam: ReturnType<typeof facade>;
    const row = new Proxy(rawRow(), {
      get(target, property, receiver) {
        equal(seam.owned[0].closed, true);
        equal(seam.owned[0].finalizes, 1);
        return Reflect.get(target, property, receiver);
      },
      ownKeys(target) {
        equal(seam.owned[0].closed, true);
        return Reflect.ownKeys(target);
      },
    });
    seam = facade(row);
    equal(await read(seam.connection, id), expected);
    finished(seam);
    const error = new Error("synthetic projection getter failure");
    const failing = rawRow();
    Object.defineProperty(failing, "created_at", {
      enumerable: true,
      get() {
        equal(seam.owned[0].closed, true);
        throw error;
      },
    });
    seam = facade(failing);
    await rejects(() => read(seam.connection, id), error);
    finished(seam);
  });
  await test("absence resolution occurs only after finalize", async () => {
    const seam = facade(null);
    const promise = read(seam.connection, missing);
    equal(seam.owned[0].closed, true);
    equal(await promise, undefined);
    finished(seam, missing);
  });
  await test("MAIN lookup ignores caller TEMP shadow", async () => {
    db.exec("CREATE TEMP TABLE enrollment_grants (caller_marker TEXT)");
    try {
      db.exec("INSERT INTO temp.enrollment_grants VALUES ('caller-only')");
      equal(await read(db, id), expected);
      const statement = db.prepare("SELECT caller_marker FROM temp.enrollment_grants");
      try { equal(statement.values(), [["caller-only"]]); }
      finally { statement.finalize(); }
    } finally {
      db.exec("DROP TABLE temp.enrollment_grants");
    }
  });
  await test("caller cached statements survive success, absence and projection failure", async () => {
    const cached = db.query("SELECT payload_hash FROM main.enrollment_grants WHERE attempt_id = ?");
    try {
      equal(cached.values(id), [[expected.payloadHash]]);
      equal(await read(db, id), expected);
      equal(await read(db, missing), undefined);
      await invalid({ ...rawRow(), payload_hash: null });
      equal(db.query("SELECT payload_hash FROM main.enrollment_grants WHERE attempt_id = ?"), cached);
      equal(cached.values(id), [[expected.payloadHash]]);
    } finally {
      cached.finalize();
    }
  });
  await test("pending caller UPDATE visible locally only, nested savepoint and outer rollback owned", async () => {
    const other = open();
    db.exec("BEGIN IMMEDIATE");
    try {
      db.exec("UPDATE main.enrollment_grants SET payload_hash = 'pending-outer'");
      equal((await read(db, id))!.payloadHash, "pending-outer");
      equal(await read(other, id), expected);
      equal(db.inTransaction, true);
      db.exec("SAVEPOINT caller_nested");
      db.exec("UPDATE main.enrollment_grants SET verified_preview_digest = ''");
      equal((await read(db, id))!.verifiedPreviewDigest, "");
      equal(await read(other, id), expected);
      const error = new Error("synthetic pending finalizer failure");
      const seam = facade(rawRow(), { finalize: error }, true);
      await rejects(() => read(seam.connection, id), error);
      finished(seam);
      equal(db.inTransaction, true);
      db.exec("ROLLBACK TO caller_nested; RELEASE caller_nested");
      equal((await read(db, id))!.verifiedPreviewDigest, expected.verifiedPreviewDigest);
      equal((await read(db, id))!.payloadHash, "pending-outer");
      db.exec("ROLLBACK");
      equal(await read(db, id), expected);
      equal(await read(other, id), expected);
    } finally {
      if (db.inTransaction) db.exec("ROLLBACK");
      other.close();
    }
    const reopened = open();
    try { equal(await read(reopened, id), expected); }
    finally { reopened.close(); }
  });
  await test("caller pending DELETE absence is local; rollback restores native grant", async () => {
    const other = open();
    db.exec("BEGIN IMMEDIATE; SAVEPOINT caller_delete");
    try {
      db.exec("DELETE FROM main.enrollment_grants");
      equal(await read(db, id), undefined);
      equal(await read(other, id), expected);
      equal(db.inTransaction, true);
      db.exec("ROLLBACK TO caller_delete; RELEASE caller_delete; ROLLBACK");
      equal(await read(db, id), expected);
    } finally {
      if (db.inTransaction) db.exec("ROLLBACK");
      other.close();
    }
  });
  await test("actual caller COMMIT and reopen, then committed fixture restoration", async () => {
    const other = open();
    db.exec("BEGIN IMMEDIATE; SAVEPOINT caller_commit");
    try {
      db.exec("UPDATE main.enrollment_grants SET payload_hash = 'committed-synthetic'");
      equal((await read(db, id))!.payloadHash, "committed-synthetic");
      equal(await read(other, id), expected);
      db.exec("RELEASE caller_commit");
      equal(db.inTransaction, true);
      db.exec("COMMIT");
      equal(db.inTransaction, false);
      equal((await read(other, id))!.payloadHash, "committed-synthetic");
      const reopened = open();
      try {
        equal((await read(reopened, id))!.payloadHash, "committed-synthetic");
        equal(values(reopened), values());
      } finally {
        reopened.close();
      }
    } finally {
      if (db.inTransaction) db.exec("ROLLBACK");
      const restore = db.prepare("UPDATE main.enrollment_grants SET payload_hash = ? WHERE attempt_id = ?");
      try { restore.run(expected.payloadHash, id); }
      finally { restore.finalize(); }
      other.close();
    }
    equal(values(), initial);
  });
  equal(values(), initial);
  const foreign = db.prepare("PRAGMA foreign_key_check");
  try { equal(foreign.values(), []); }
  finally { foreign.finalize(); }
  console.log(`enrollment-grant-read checks=${checks} scenarios=${scenarios}`);
} finally {
  if (db.inTransaction) db.exec("ROLLBACK");
  db.close();
}
