import assert from "node:assert/strict";
import { isAbsolute } from "node:path";
import { openSQLite } from "./open.js";
import { migrateSQLite } from "./migrate.js";
import { SQLiteWriter } from "./write.js";

const [path, mode] = process.argv.slice(2);
assert(path && isAbsolute(path) && ["insert", "verify"].includes(mode!));
const db = openSQLite(path);
const raw = ' \n{"n":922337203685477580812345,"text":"雪🦊"} \t';
const missing = "00000000-0000-0000-0000-000000000099";
const clock = "109223372036854775807.000000123";
type Column = [string, string, bigint, string | null, bigint];
const text = (n: string, required = 1n, pk = 0n, value: string | null = null): Column => [n, "TEXT", required, value, pk];
const integer = (n: string, required = 0n): Column => [n, "INTEGER", required, null, 0n];
const id = text("id", 1n, 1n);
const tail = [text("source", 1n, 0n, "'ordinary'"), text("source_primary_id", 0n), text("source_node_id", 0n), text("source_revision_id", 0n), text("snapshot_content_hash", 0n), integer("snapshot_version"), integer("replication_version"), integer("leadership_generation")];
// Independent effective PG migration order, not parsed from the shared SQL.
const columns: Record<string, Column[]> = {
  config_revisions: [id, integer("revision_number", 1n), text("checksum"), text("snapshot"), text("actor_user_id", 0n), text("created_at"), text("applied_at", 0n), ...tail],
  apply_jobs: [id, text("revision_id"), text("actor_user_id", 0n), text("target"), text("status", 1n, 0n, "'queued'"), text("correlation_id"), text("validation_output", 0n), text("apply_output", 0n), text("health_output", 0n), text("error_message", 0n), text("claimed_at", 0n), text("started_at", 0n), text("finished_at", 0n), text("created_at"), ...tail],
  health_observations: [id, text("component"), text("status"), text("details", 0n), text("created_at")],
  operational_artifacts: [id, text("kind"), text("path"), integer("size_bytes", 1n), text("revision_id", 0n), text("created_at")],
};
const query = (sql: string) => db.query<Record<string, string | bigint | null>, []>(sql).all();
const count = (table: string) => query(`SELECT count(*) AS n FROM ${table}`)[0]!.n;
const snapshot = () => Object.keys(columns).map((table) => query(`SELECT * FROM ${table} ORDER BY id`));
const json = (n: string) => n === "snapshot" || n === "details" || n.endsWith("_output");
const reject = (sql: string) => assert.throws(() => db.exec(sql), /constraint|cannot store/i);
try {
  migrateSQLite(db);
  const ledger = query("SELECT * FROM proxycore_schema_migrations ORDER BY version");
  assert.equal(ledger.length, 6);
  assert.equal(ledger[5]!.name, "enrollment_product");
  assert.equal(ledger[3]!.name, "operational_product");
  assert.deepEqual(query("SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name").map((r) => r.name), ["applied_snapshots", "apply_jobs", "audit_events", "certificates", "cluster_keys", "config_revisions", "dns_records", "enrolled_nodes", "enrollment_attempts", "enrollment_grants", "enrollment_tokens", "forwarding_rules", "health_observations", "installation_identity", "installation_settings", "internal_ca", "internal_ca_enrollment_state", "node_credentials", "node_snapshot_acks", "node_state", "operational_artifacts", "provider_connections", "proxycore_schema_migrations", "resolver_pools", "secrets", "sessions", "standalone_archives", "stream_routes", "sync_attempts", "users", "zones"]);
  for (const [table, expected] of Object.entries(columns)) {
    assert.deepEqual(query(`PRAGMA table_info(${table})`).map((c) => [c.name, c.type, c.notnull, c.dflt_value, c.pk]), expected);
    const ddl = query(`SELECT sql FROM sqlite_schema WHERE name='${table}'`)[0]!.sql as string;
    assert(ddl.endsWith(" STRICT"));
    for (const [name] of expected) if (name.endsWith("_at")) assert(ddl.includes(`${name} TEXT COLLATE BINARY`));
    if (["config_revisions", "apply_jobs"].includes(table)) assert(ddl.includes(`CONSTRAINT ${table}_source_check CHECK (source IN ('ordinary','import','sync'))`));
    const indexes = query(`PRAGMA index_list(${table})`).filter((i) => i.origin !== "pk").sort((a, b) => String(a.name).localeCompare(String(b.name)));
    assert.deepEqual(indexes.map((i) => [i.name, i.unique, i.origin, i.partial]), table === "config_revisions" ? [["config_revisions_checksum_idx", 1n, "c", 0n], ["config_revisions_number_idx", 1n, "c", 0n]] : []);
    for (const index of query(`PRAGMA index_list(${table})`)) {
      const expectedColumn = index.origin === "pk" ? "id" : index.name === "config_revisions_number_idx" ? "revision_number" : "checksum";
      assert.deepEqual(query(`PRAGMA index_xinfo('${index.name}')`).filter((x) => x.key === 1n).map((x) => [x.name, x.coll, x.desc]), [[expectedColumn, "BINARY", 0n]]);
    }
    const fks: string[][] = [];
    if (["config_revisions", "apply_jobs"].includes(table)) fks.push(["users", "actor_user_id", "id", "NO ACTION", "NO ACTION"]);
    if (["apply_jobs", "operational_artifacts"].includes(table)) fks.push(["config_revisions", "revision_id", "id", "NO ACTION", "NO ACTION"]);
    assert.deepEqual(query(`PRAGMA foreign_key_list(${table})`).sort((a, b) => String(a.from).localeCompare(String(b.from))).map((k) => [k.table, k.from, k.to, k.on_delete, k.on_update]), fks);
  }
  if (mode === "insert") {
    if (count("config_revisions") === 0n) for (const table of Object.keys(columns)) assert.equal(count(table), 0n);
    let calls = 0;
    new SQLiteWriter(db, () => { calls++; return { kind: "finite", seconds: -1n, nanos: 123 }; }).write((v) => {
      const key = v.newUUID();
      db.query("INSERT INTO config_revisions(id,revision_number,checksum,snapshot,created_at,leadership_generation) VALUES(?,-1,'bun',?,?,?)").run(key, raw, v.timestamp, 9007199254740993n);
      db.query("INSERT INTO apply_jobs(id,revision_id,target,correlation_id,validation_output,created_at,leadership_generation) VALUES(?,?,'combined','bun',?,?,?)").run(key, key, raw, v.timestamp, -9223372036854775808n);
      db.query("INSERT INTO health_observations VALUES(?,'bun','arbitrary',?,?)").run(key, raw, v.timestamp);
      db.query("INSERT INTO operational_artifacts VALUES(?,'bun','arbitrary 雪',0,?,?)").run(key, key, v.timestamp);
    });
    assert.equal(calls, 1);
  }
  for (const [table, expected] of Object.entries(columns)) {
    assert((count(table) as bigint) > 0n);
    for (const [name, kind, required] of expected) {
      const bad = kind === "TEXT" ? [`coalesce(${name},'x')||char(0)||'tail'`, "CAST('x' AS BLOB)"] : ["1.5", "CAST('1' AS BLOB)", ...(name === "leadership_generation" ? ["9223372036854775808", "-9223372036854775809"] : ["2147483648", "-2147483649"])];
      if (required) bad.push("NULL");
      if (name === "id" || name.endsWith("_id") && name !== "correlation_id") bad.push("'bad'", "'AAAAAAAA-0000-0000-0000-000000000000'", "'00000000000000000000000000000000000000'");
      if (name.endsWith("_at")) bad.push("'bad'", "'1'", "'3'", "'118446744073709551616.000000000'", "'100000000000000000000.00000000x'");
      if (json(name)) bad.push("'{'", "'undefined'");
      for (const value of bad) reject(`UPDATE ${table} SET ${name}=${value}`);
      if (["id", "created_at"].includes(name)) {
        const others = expected.filter(([n]) => n !== name).map(([n]) => n).join(",");
        reject(`INSERT INTO ${table}(${others}) SELECT ${others} FROM ${table} LIMIT 1`);
      }
    }
    reject(`INSERT INTO ${table} SELECT * FROM ${table} LIMIT 1`);
    assert(query(`SELECT * FROM ${table}`).every((r) => r.created_at === clock));
  }
  assert(query("SELECT * FROM config_revisions").every((r) => r.snapshot === raw && r.source === "ordinary" && r.applied_at === null));
  assert(query("SELECT * FROM apply_jobs").every((r) => r.validation_output === raw && r.source === "ordinary" && r.status === "queued" && r.apply_output === null));
  assert(query("SELECT * FROM health_observations").every((r) => r.details === raw));
  const goRevision = query("SELECT leadership_generation FROM config_revisions WHERE checksum!='bun'");
  const goJob = query("SELECT leadership_generation FROM apply_jobs WHERE correlation_id='go'");
  if (goRevision.length) assert.equal(goRevision[0]!.leadership_generation, -9223372036854775808n);
  if (goJob.length) assert.equal(goJob[0]!.leadership_generation, 9223372036854775807n);
  for (const assignment of ["target='other'", "status='other'"]) reject(`UPDATE apply_jobs SET ${assignment}`);
  for (const table of ["config_revisions", "apply_jobs"]) assert.throws(() => db.exec(`UPDATE ${table} SET source='other'`), new RegExp(`${table}_source_check`));
  const before = snapshot();
  db.exec("BEGIN");
  try {
    db.exec("UPDATE config_revisions SET revision_number=42 WHERE id!=(SELECT id FROM config_revisions ORDER BY id LIMIT 1)");
    for (const [table, expected] of Object.entries(columns)) for (const [name, kind, required] of expected) {
      let values: (string | bigint)[] = [];
      if (json(name)) values = [raw, "null", "true", "123", '"scalar"', "922337203685477580812345"];
      if (name.endsWith("_at")) values = ["0", "2", "100000000000000000000.000000000", "118446744073709551615.999999999", clock];
      if (kind === "INTEGER") values = [-2147483648n, 2147483647n, 0n, -1n, ...(name === "leadership_generation" ? [-9223372036854775808n, 9223372036854775807n, 9007199254740993n] : [])];
      if (name.startsWith("source_")) values = [missing];
      if (["snapshot_content_hash", "component", "kind", "path"].includes(name) || table === "health_observations" && name === "status") values = ["", "arbitrary 雪"];
      for (const value of values) {
        db.query(`UPDATE ${table} SET ${name}=? WHERE id=(SELECT id FROM ${table} ORDER BY id LIMIT 1)`).run(value);
        const actual = query(`SELECT ${name} AS v,typeof(${name}) AS kind FROM ${table} ORDER BY id LIMIT 1`)[0]!;
        assert.equal(actual.v, value);
        assert.equal(actual.kind, kind === "TEXT" ? "text" : "integer");
      }
      if (!required) {
        db.exec(`UPDATE ${table} SET ${name}=NULL`);
        assert(query(`SELECT ${name} AS v,typeof(${name}) AS kind FROM ${table}`).every((r) => r.v === null && r.kind === "null"));
      }
    }
    for (const [table, domains] of Object.entries({ config_revisions: { source: ["ordinary", "import", "sync"] }, apply_jobs: { source: ["ordinary", "import", "sync"], target: ["coredns", "nginx", "combined", "certificate"], status: ["queued", "validating", "applying", "applied", "failed", "rolled-back"] } })) {
      for (const [column, values] of Object.entries(domains)) for (const value of values) db.query(`UPDATE ${table} SET ${column}=?`).run(value);
    }
  } finally { db.exec("ROLLBACK"); }
  assert.deepEqual(snapshot(), before);
  reject(`INSERT INTO config_revisions(id,revision_number,checksum,snapshot,created_at) SELECT '${missing}',revision_number,'unique-other',snapshot,created_at FROM config_revisions LIMIT 1`);
  reject(`INSERT INTO config_revisions(id,revision_number,checksum,snapshot,created_at) SELECT '${missing}',123,checksum,snapshot,created_at FROM config_revisions LIMIT 1`);
  for (const [table, column, parent] of [["config_revisions", "actor_user_id", "users"], ["apply_jobs", "actor_user_id", "users"], ["apply_jobs", "revision_id", "config_revisions"], ["operational_artifacts", "revision_id", "config_revisions"]]) {
    db.exec("BEGIN");
    try {
      reject(`UPDATE ${table} SET ${column}='${missing}'`);
      if (parent === "users") {
        if (table === "config_revisions") db.exec("DELETE FROM apply_jobs; DELETE FROM operational_artifacts");
        db.exec(`INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES('${missing}','edge','opaque','owner','0','2')`);
        db.exec(`UPDATE ${table} SET ${column}='${missing}'`);
        reject(`DELETE FROM users WHERE id='${missing}'`);
        reject(`UPDATE users SET id='00000000-0000-0000-0000-000000000098' WHERE id='${missing}'`);
      } else {
        db.exec(`DELETE FROM ${table === "apply_jobs" ? "operational_artifacts" : "apply_jobs"}`);
        reject("DELETE FROM config_revisions");
        reject(`UPDATE config_revisions SET id='${missing}'`);
      }
      db.exec(`DELETE FROM ${table}; DELETE FROM ${parent}`);
    } finally { db.exec("ROLLBACK"); }
    assert.deepEqual(snapshot(), before);
  }
  migrateSQLite(db);
  assert.deepEqual(query("SELECT * FROM proxycore_schema_migrations ORDER BY version"), ledger);
  db.exec("CREATE INDEX operational_drift ON apply_jobs(status)");
  assert.throws(() => migrateSQLite(db), /schema/i);
  assert.deepEqual(snapshot(), before);
  assert.deepEqual(query("SELECT * FROM proxycore_schema_migrations ORDER BY version"), ledger);
  db.exec("DROP INDEX operational_drift");
} finally { db.close(); }
