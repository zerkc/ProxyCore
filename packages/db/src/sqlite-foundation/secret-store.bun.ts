import assert from "node:assert/strict";
import { Database } from "bun:sqlite";
import { openSQLite } from "./open.js";
import { decodeTimestamp } from "./timestamp.js";
import { createSQLiteSecretStore } from "./secret-store.js";
import type { SecretStore } from "@proxycore/certificates";

type Identical<A, B> =
  (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2)
    ? (<T>() => T extends B ? 1 : 2) extends (<T>() => T extends A ? 1 : 2) ? true : false
    : false;
type Expect<T extends true> = T;
type CanonicalReturn = Expect<Identical<ReturnType<typeof createSQLiteSecretStore>, SecretStore>>;

const [path, key, goId, goPlain] = process.argv.slice(2) as [string, string, string, string];
const db = openSQLite(path);
const store = createSQLiteSecretStore(db, key);
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const count = () => db.query<{ n: bigint }, []>("SELECT count(*) AS n FROM main.secrets").get()!.n;
let scenarios = 0;
async function test(name: string, body: () => Promise<void>) {
  await body(); scenarios++; console.log(`PASS ${name}`);
}

try {
  await test("Go-written secret is readable by TS, uppercase id accepted", async () => {
    assert.equal(await store.get(goId), goPlain);
    assert.equal(await store.get(goId.toUpperCase()), goPlain);
  });
  await test("put/get round trip with canonical id and no plaintext in row", async () => {
    const plain = "synthetic-ts-plaintext 雪";
    const id = await store.put("ts-purpose", plain);
    assert.match(id, uuid);
    assert.equal(await store.get(id), plain);
    const row = db.query<Record<string, unknown>, [string]>("SELECT * FROM main.secrets WHERE id=?").get(id)!;
    assert.deepEqual(Object.keys(row).sort(), ["ciphertext", "created_at", "id", "purpose", "updated_at"]);
    assert.equal(row.purpose, "ts-purpose");
    assert.match(row.ciphertext as string, /^v1:[\w-]+:[\w-]+:[\w-]+$/);
    assert.ok(!JSON.stringify(row).includes("synthetic-ts-plaintext"));
    assert.equal(row.created_at, row.updated_at);
    assert.equal(decodeTimestamp(row.created_at as string).kind, "finite");
    console.log(`TSWRITTEN ${id} ${plain}`);
  });
  await test("missing id is undefined; malformed id rejects like a PG uuid column", async () => {
    assert.equal(await store.get("00000000-0000-4000-8000-000000000000"), undefined);
    for (const bad of ["", "nope", "00000000-0000-4000-8000-00000000000g", `{${goId}}`, `${goId} `]) {
      await assert.rejects(store.get(bad), /invalid input syntax for type uuid/);
    }
  });
  await test("empty purpose accepted; empty plaintext stores but cannot decrypt", async () => {
    assert.equal(await store.get(await store.put("", "synthetic-empty-purpose")), "synthetic-empty-purpose");
    const id = await store.put("p", "");
    await assert.rejects(store.get(id), /Unsupported encrypted secret format/);
  });
  await test("NUL purpose rejected without a row", async () => {
    const before = count();
    await assert.rejects(store.put("a\0b", "x"));
    assert.equal(count(), before);
  });
  await test("empty or invalid master key rejects put and get without a row", async () => {
    for (const bad of ["", "AAAA"]) {
      const broken = createSQLiteSecretStore(db, bad), before = count();
      await assert.rejects(broken.put("p", "x"), /must decode to 32 bytes/);
      assert.equal(count(), before);
      await assert.rejects(broken.get(goId), /must decode to 32 bytes/);
    }
  });
  await test("wrong master key fails authentication", async () => {
    const other = createSQLiteSecretStore(db, Buffer.alloc(32, 7).toString("base64"));
    await assert.rejects(other.get(goId));
  });
  await test("caller-owned connection stays open and outside a transaction", async () => {
    assert.equal(db.inTransaction, false);
    assert.equal(await store.get(goId), goPlain);
  });
  console.log(`COMPLETE ${scenarios} scenarios`);
} finally { db.close(); }
