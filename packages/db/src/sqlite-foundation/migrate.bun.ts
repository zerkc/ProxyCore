import { strict as assert } from "node:assert";
import { isAbsolute } from "node:path";
import { migrateSQLite, migrationFixtureForTest } from "./migrate.js";
import { openSQLite } from "./open.js";

const [path, directory, mode] = process.argv.slice(2);
assert.ok(path && isAbsolute(path));
assert.ok(directory && (directory === "-" || isAbsolute(directory)));
const db = openSQLite(path);
try {
  if (mode === "ready") process.stdout.write("ready\n");
  if (mode === "temporary") db.exec("CREATE TEMP TABLE foreign_table(id INTEGER)");
  if (mode === "default" || mode === "temporary") {
    migrateSQLite(db);
  } else {
    migrationFixtureForTest(db, directory);
  }
  const rows = db.query<{ applied_at: string }, []>(
    "SELECT applied_at FROM proxycore_schema_migrations ORDER BY version",
  ).all();
  assert.ok(rows.every((row) => row.applied_at.length === 31));
} finally {
  db.close();
}
