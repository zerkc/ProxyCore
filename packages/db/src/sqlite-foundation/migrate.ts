import { Database } from "bun:sqlite";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { decodeTimestamp, encodeTimestamp } from "./timestamp.js";

type Entry = { version: number; name: string; file: string; checksum: string; sql: string };
type SchemaObject = { name: string; type: string; sql: string };
const ledgerDDL = "CREATE TABLE proxycore_schema_migrations(version INTEGER NOT NULL PRIMARY KEY,name TEXT NOT NULL,checksum TEXT NOT NULL,applied_at TEXT COLLATE BINARY NOT NULL)";
const authority = fileURLToPath(new URL("../../../../apps/api/internal/sqlitedb/migrations/", import.meta.url));
const controlTokens = new Set(["BEGIN", "END", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE", "ATTACH", "DETACH", "PRAGMA"]);

// JSON.parse validates syntax first. This shallow token walk preserves member
// occurrences lost by JSON.parse, treating escaped strings as indivisible tokens.
function validateRegistryMembers(raw: string): void {
  const tokens = raw.match(/"(?:\\.|[^"\\])*"|[{}\[\]:,]|[^\s{}\[\]:,]+/g) ?? [];
  let depth = 0;
  let keys = new Set<string>();
  for (const [i, token] of tokens.entries()) {
    if (token === "{" || token === "[") {
      depth++;
      if (depth === 2) keys = new Set<string>();
    } else if (token === "}" || token === "]") {
      depth--;
    } else if (depth === 2 && tokens[i + 1] === ":") {
      const key: string = JSON.parse(token);
      if (!["version", "name", "file", "checksum"].includes(key) || keys.has(key)) {
        throw new Error(`Unknown or duplicate registry member ${key}`);
      }
      if (key === "version" && !/^[1-9][0-9]*$/.test(tokens[i + 2] ?? "")) {
        throw new Error("Version must use positive integer JSON syntax");
      }
      keys.add(key);
    }
  }
}

function loadAuthority(directory: string): Entry[] {
  const raw = readFileSync(join(directory, "registry.json"), "utf8");
  const entries: unknown = JSON.parse(raw);
  if (!Array.isArray(entries)) throw new Error("Registry must be a JSON array");
  validateRegistryMembers(raw);
  const seen = new Set<string>();
  return entries.map((e, i) => {
    if (!e || typeof e !== "object" || Object.keys(e).sort().join(",") !== "checksum,file,name,version"
      || !Number.isSafeInteger(e.version) || e.version !== i + 1
      || typeof e.name !== "string" || !/^[A-Za-z0-9][A-Za-z0-9 _.-]{0,127}$/.test(e.name) || e.name.trim() !== e.name
      || typeof e.file !== "string" || !/^[a-z0-9][a-z0-9_-]*\.sql$/.test(e.file) || e.file.trim() !== e.file
      || typeof e.checksum !== "string" || e.checksum.length !== 64 || !/^[0-9a-f]{64}$/.test(e.checksum)
      || seen.has(`name:${e.name}`) || seen.has(`file:${e.file}`)) {
      throw new Error(`Invalid registry entry ${i + 1}`);
    }
    seen.add(`name:${e.name}`);
    seen.add(`file:${e.file}`);
    const bytes = readFileSync(join(directory, e.file));
    if (createHash("sha256").update(bytes).digest("hex") !== e.checksum) {
      throw new Error(`SQL checksum mismatch for ${e.file}`);
    }
    const sql = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes);
    if (sql.includes("\0")) throw new Error("SQL must not contain NUL");
    // Scripts must not manage transactions/connection state; BEGIN/END triggers
    // are conservatively unsupported, while comments and quoted text are ignored.
    const tokens = sql.match(/'([^']|'')*'|"([^"]|"")*"|\[[^\]]*\]|--[^\r\n]*|\/\*.*?\*\/|`[^`]*`|[A-Za-z_][A-Za-z0-9_]*/gs) ?? [];
    if (tokens.some((token) => controlTokens.has(token.toUpperCase()))) {
      throw new Error("Migration SQL contains unsupported control tokens");
    }
    return { ...e, sql };
  });
}

function productSchema(db: Database): SchemaObject[] {
  return db.query<SchemaObject, []>(
    "SELECT name,type,sql FROM main.sqlite_schema WHERE substr(name,1,7) != 'sqlite_' AND name != 'proxycore_schema_migrations' ORDER BY name",
  ).all();
}

function migrateAuthority(db: Database, entries: Entry[]): void {
  db.transaction(() => {
    const temporary = db.query<{ count: bigint }, []>(
      "SELECT count(*) AS count FROM temp.sqlite_schema WHERE substr(name,1,7) != 'sqlite_'",
    ).get();
    if (temporary?.count !== 0n) throw new Error("Unrecognized temporary schema");
    const ledger = db.query<{ type: string; sql: string }, []>(
      "SELECT type,sql FROM main.sqlite_schema WHERE name = 'proxycore_schema_migrations'",
    ).get();
    if (ledger && (ledger.type !== "table" || ledger.sql !== ledgerDDL)) {
      throw new Error("Unrecognized migration ledger schema");
    }
    if (!ledger) db.exec(ledgerDDL);
    const invalidStorage = db.query<{ count: bigint }, []>(
      "SELECT count(*) AS count FROM main.proxycore_schema_migrations WHERE typeof(version) != 'integer' OR typeof(name) != 'text' OR typeof(checksum) != 'text' OR typeof(applied_at) != 'text'",
    ).get();
    if (invalidStorage?.count !== 0n) throw new Error("Invalid ledger storage classes");
    const applied = db.query<{ version: bigint; name: string; checksum: string; applied_at: string }, []>(
      "SELECT version,name,checksum,applied_at FROM main.proxycore_schema_migrations ORDER BY version",
    ).all();
    for (const [i, row] of applied.entries()) {
      const entry = entries[i];
      if (!entry || row.version !== BigInt(i + 1) || row.name !== entry.name || row.checksum !== entry.checksum
        || decodeTimestamp(row.applied_at).kind !== "finite") {
        throw new Error(`Unrecognized applied migration ${row.version}`);
      }
    }
    const scratch = new Database(":memory:", { strict: true, safeIntegers: true });
    try {
      scratch.exec("PRAGMA foreign_keys = ON");
      for (const entry of entries.slice(0, applied.length)) scratch.exec(entry.sql);
      if (JSON.stringify(productSchema(db)) !== JSON.stringify(productSchema(scratch))) {
        throw new Error("Unrecognized product schema");
      }
    } finally {
      scratch.close();
    }
    // This is only the ledger clock, not a future product-default transaction clock.
    const millis = BigInt(Date.now());
    const remainder = ((millis % 1000n) + 1000n) % 1000n;
    const clock = encodeTimestamp({ kind: "finite", seconds: (millis - remainder) / 1000n, nanos: Number(remainder) * 1000000 });
    for (const entry of entries.slice(applied.length)) {
      db.exec(entry.sql);
      db.query("INSERT INTO main.proxycore_schema_migrations VALUES (?, ?, ?, ?)")
        .run(entry.version, entry.name, entry.checksum, clock);
    }
  }).immediate();
}

// Production always reads the same authority directory that Go embeds.
export function migrateSQLite(db: Database): void {
  migrateAuthority(db, loadAuthority(authority));
}

// Internal, harness-only fixture seam. It is guarded against normal application
// imports and retains all production validation; there is no unchecked SQL API.
export function migrationFixtureForTest(db: Database, directory: string): void {
  const harness = fileURLToPath(new URL("./migrate.bun.ts", import.meta.url));
  if (resolve(process.argv[1] ?? "") !== harness) throw new Error("Test harness only");
  migrateAuthority(db, loadAuthority(directory));
}
