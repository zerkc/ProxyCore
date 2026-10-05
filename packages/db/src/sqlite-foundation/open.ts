/// <reference types="bun-types/sqlite" />
import { Database } from "bun:sqlite";
import { resolve } from "node:path";

// Open a literal local filename. Callers own close(); parent directories must exist.
export function openSQLite(path: string): Database {
  if (!path || path.includes("\0")) {
    throw new Error("SQLite requires a nonempty filename without NUL");
  }
  // Absolute paths keep URI-looking filenames from becoming connection options.
  const db = new Database(resolve(path), { create: true, strict: true, safeIntegers: true });
  try {
    db.exec("PRAGMA busy_timeout = 5000; PRAGMA foreign_keys = ON;");
    const mode = db.query<{ journal_mode: string }, []>("PRAGMA journal_mode = WAL").get();
    if (mode?.journal_mode !== "wal") {
      throw new Error("SQLite could not enable WAL");
    }
    return db;
  } catch (error) {
    db.close();
    throw error;
  }
}
