import type { Database } from "bun:sqlite";
import { decryptSecret, encryptSecret } from "@proxycore/crypto";
import type { SecretStore } from "@proxycore/certificates";
import { SQLiteWriter } from "./write.js";

const canonical = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

/** Unwired SQLite SecretStore, byte-compatible with Go secrets.SQLiteStore.
 * Mirrors PgSecretStore: encryption happens before any write, so a bad master key
 * leaves no row; get returns undefined for a missing id and rejects malformed ids
 * like a PG uuid column (uppercase hex is accepted and lowercased).
 * The caller owns the connection; each put is one SQLiteWriter transaction, and
 * each get uses its own uncached statement, finalized before decryption.
 */
export function createSQLiteSecretStore(db: Database, masterKeyBase64: string): SecretStore {
  const writer = new SQLiteWriter(db);
  return {
    async put(purpose, plaintext) {
      const ciphertext = encryptSecret(plaintext, masterKeyBase64);
      return writer.write((v) => {
        const id = v.newUUID();
        v.db.query("INSERT INTO main.secrets (id, purpose, ciphertext, created_at, updated_at) VALUES (?, ?, ?, ?, ?)")
          .run(id, purpose, ciphertext, v.timestamp, v.timestamp);
        return id;
      });
    },
    async get(id) {
      if (!canonical.test(id)) throw new Error(`invalid input syntax for type uuid: ${JSON.stringify(id)}`);
      const statement = db.prepare("SELECT ciphertext FROM main.secrets WHERE id = ?");
      let rows: { ciphertext: string }[];
      try {
        rows = statement.all(id.toLowerCase()) as { ciphertext: string }[];
      } finally {
        statement.finalize();
      }
      return rows.length === 0 ? undefined : decryptSecret(rows[0]!.ciphertext, masterKeyBase64);
    },
  };
}
