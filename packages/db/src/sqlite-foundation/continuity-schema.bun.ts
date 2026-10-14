import assert from "node:assert/strict";
import { isAbsolute } from "node:path";
import { openSQLite } from "./open.js";
import { migrateSQLite } from "./migrate.js";
import { SQLiteWriter } from "./write.js";

const [path, mode] = process.argv.slice(2);
assert(path && isAbsolute(path) && ["insert", "verify"].includes(mode!));
const db = openSQLite(path);
const opaque = '  opaque 雪🦊\n"quoted" non-JSON  ';
const missing = "00000000-0000-0000-0000-000000000099";
const clock = "109223372036854775807.000000123";
type Column = [string, string, bigint, string | null, bigint];
const text = (n: string, required = 1n, pk = 0n, value: string | null = null): Column => [n, "TEXT", required, value, pk];
const optional = (n: string): Column => text(n, 0n);
const integer = (n: string, value: string | null = null): Column => [n, "INTEGER", 1n, value, 0n];
const id = text("id", 1n, 1n);
// Independent declaration: effective 0004 order followed by the 0008 bytea tail.
const columns: Record<string, Column[]> = {
  installation_identity: [id, text("installation_id"), text("node_id"), text("role", 1n, 0n, "'standalone-primary'"), integer("leadership_generation", "1"), integer("latest_known_generation", "1"), optional("cluster_key_id"), text("updated_at")],
  cluster_keys: [id, text("purpose"), text("wrapped_kek"), integer("wrapping_key_version"), text("created_at"), optional("retired_at")],
  node_state: [id, optional("enrolled_at"), optional("enrollment_token_hash"), optional("enrollment_primary_id"), optional("last_seen_at"), optional("last_applied_snapshot_id"), optional("enrollment_attempt_id"), optional("primary_url"), optional("primary_installation_id"), optional("primary_tls_spki_sha256"), optional("credential_id"), integer("sync_enabled", "0"), optional("last_attempt_at"), optional("last_success_at"), integer("consecutive_failures", "0"), optional("next_attempt_at"), optional("last_error_code"), text("updated_at")],
  applied_snapshots: [id, text("source_primary_id"), integer("leadership_generation"), integer("snapshot_version"), integer("replication_version"), text("content_hash"), optional("revision_id"), text("status", 1n, 0n, "'applied'"), optional("apply_job_id"), optional("failure_code"), text("applied_at"), optional("discarded_at"), ["snapshot_body", "BLOB", 0n, null, 0n]],
  standalone_archives: [id, optional("snapshot_content_hash"), optional("revision_id"), text("archive_blob"), text("capture_reason"), text("captured_at"), optional("retention_started_at"), optional("expires_at"), integer("restoration_eligible", "1"), text("created_at")],
};
const indexes: Record<string, [string, string, bigint][]> = {
  node_state: [["node_state_credential_idx", "credential_id", 1n], ["node_state_enrollment_attempt_idx", "enrollment_attempt_id", 1n]],
  applied_snapshots: [["applied_snapshots_content_hash_idx", "content_hash", 1n]],
  standalone_archives: [["standalone_archives_expiry_idx", "expires_at", 0n]],
};
const edges = [["applied_snapshots", "apply_job_id", "apply_jobs"], ["applied_snapshots", "revision_id", "config_revisions"], ["standalone_archives", "revision_id", "config_revisions"]] as const;
type Value = string | bigint | Uint8Array | null;
const query = (sql: string) => db.query<Record<string, Value>, []>(sql).all();
const count = (table: string) => query(`SELECT count(*) AS n FROM ${table}`)[0]!.n as bigint;
const uuid = (table: string, n: string) => n.endsWith("_id") || n === "id" && !["installation_identity", "node_state"].includes(table);
const bool = (n: string) => ["sync_enabled", "restoration_eligible"].includes(n);
const int64 = (n: string) => ["leadership_generation", "latest_known_generation"].includes(n);
const reject = (sql: string) => assert.throws(() => db.exec(sql), /constraint|cannot store/i);
const snapshot = () => Object.entries(columns).map(([table, cols]) => query(`SELECT *,${cols.map(([n]) => `typeof(${n}) AS type_${n}`).join(",")} FROM ${table} ORDER BY id`));
try {
  migrateSQLite(db);
  const ledger = query("SELECT * FROM proxycore_schema_migrations ORDER BY version");
  assert.equal(ledger.length, 7);
  assert.equal(ledger[5]!.name, "enrollment_product");
  assert.equal(Object.values(columns).reduce((n, cols) => n + cols.length, 0), 55);
  assert.deepEqual(query("SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name").map((r) => r.name), ["applied_snapshots", "apply_jobs", "audit_events", "certificates", "cluster_keys", "config_revisions", "dns_records", "enrolled_nodes", "enrollment_attempts", "enrollment_grants", "enrollment_tokens", "forwarding_rules", "health_observations", "installation_identity", "installation_settings", "internal_ca", "internal_ca_enrollment_state", "node_credentials", "node_snapshot_acks", "node_state", "operational_artifacts", "primary_trust_anchors", "provider_connections", "proxycore_schema_migrations", "resolver_pools", "secrets", "sessions", "standalone_archives", "stream_routes", "sync_attempts", "users", "zones"]);
  for (const [table, expected] of Object.entries(columns)) {
    assert.deepEqual(query(`PRAGMA table_info(${table})`).map((c) => [c.name, c.type, c.notnull, c.dflt_value, c.pk]), expected);
    const ddl = query(`SELECT sql FROM sqlite_schema WHERE name='${table}'`)[0]!.sql as string;
    assert(ddl.endsWith(" STRICT"));
    for (const [name] of expected) if (name.endsWith("_at")) assert(ddl.includes(`${name} TEXT COLLATE BINARY`));
    if (table === "applied_snapshots") assert(ddl.includes("CONSTRAINT applied_snapshots_status_check CHECK (status IN ('pending','applied','rejected','rolled-back','archived'))"));
    const secondary = query(`PRAGMA index_list(${table})`).filter((i) => i.origin !== "pk").sort((a, b) => String(a.name).localeCompare(String(b.name)));
    assert.deepEqual(secondary.map((i) => [i.name, i.unique, i.origin, i.partial]), (indexes[table] ?? []).map(([name, , unique]) => [name, unique, "c", 0n]));
    for (const index of query(`PRAGMA index_list(${table})`)) {
      const col = index.origin === "pk" ? "id" : indexes[table]!.find(([n]) => n === index.name)![1];
      assert.deepEqual(query(`PRAGMA index_xinfo('${index.name}')`).filter((x) => x.key === 1n).map((x) => [x.name, x.coll, x.desc]), [[col, "BINARY", 0n]]);
    }
    assert.deepEqual(query(`PRAGMA foreign_key_list(${table})`).sort((a, b) => String(a.from).localeCompare(String(b.from))).map((k) => [k.table, k.from, k.to, k.on_delete, k.on_update]), edges.filter(([t]) => t === table).map(([, col, parent]) => [parent, col, "id", "NO ACTION", "NO ACTION"]));
  }
  if (mode === "insert") {
    if (count("cluster_keys") === 0n) for (const table of Object.keys(columns)) assert.equal(count(table), 0n);
    let calls = 0;
    new SQLiteWriter(db, () => { calls++; return { kind: "finite", seconds: -1n, nanos: 123 }; }).write((v) => {
      const key = v.newUUID();
      db.query("INSERT INTO installation_identity(id,installation_id,node_id,cluster_key_id,updated_at) VALUES('bun',?,?,?,?)").run(key, missing, missing, v.timestamp);
      db.query("INSERT INTO cluster_keys(id,purpose,wrapped_kek,wrapping_key_version,created_at,retired_at) VALUES(?,'bun',?,-1,?,'2')").run(key, opaque, v.timestamp);
      db.query("INSERT INTO node_state(id,enrollment_primary_id,last_applied_snapshot_id,enrollment_attempt_id,primary_installation_id,credential_id,updated_at,enrolled_at) VALUES('bun',?,?,?,?,?,?,'0')").run(missing, missing, key, missing, key, v.timestamp);
      db.query("INSERT INTO applied_snapshots(id,source_primary_id,leadership_generation,snapshot_version,replication_version,content_hash,applied_at,snapshot_body) VALUES(?,?,?,0,-1,'bun',?,?)").run(key, missing, 9007199254740993n, v.timestamp, new Uint8Array([0, 255, 195, 40]));
      db.query("INSERT INTO standalone_archives(id,archive_blob,capture_reason,captured_at,created_at,expires_at) VALUES(?,?,'bun',?,?,'2')").run(key, opaque, v.timestamp, v.timestamp);
      for (const [suffix, body] of [["null", null], ["empty", new Uint8Array()], ["nul", new Uint8Array([0])]] as const) {
        db.query("INSERT INTO applied_snapshots(id,source_primary_id,leadership_generation,snapshot_version,replication_version,content_hash,applied_at,snapshot_body) VALUES(?,?,0,0,0,?,?,?)").run(v.newUUID(), missing, `bun-${suffix}`, v.timestamp, body);
      }
    });
    assert.equal(calls, 1);
  }
  for (const [table, expected] of Object.entries(columns)) {
    assert(count(table) > 0n, `nonvacuous ${table}`);
    for (const [name, kind, required] of expected) {
      const bad: string[] = [];
      if (required) bad.push("NULL");
      if (kind === "TEXT") bad.push(`coalesce(${name},'x')||char(0)||'tail'`, "CAST('x' AS BLOB)");
      if (kind === "INTEGER") bad.push("1.5", "CAST('1' AS BLOB)", ...(bool(name) ? ["-1", "2"] : int64(name) ? ["9223372036854775808", "-9223372036854775809"] : ["2147483648", "-2147483649"]));
      if (kind === "BLOB") bad.push("'text'", "1");
      if (uuid(table, name)) bad.push("'bad'", "'AAAAAAAA-0000-0000-0000-000000000000'", "'00000000000000000000000000000000000000'");
      if (name.endsWith("_at")) bad.push("'bad'", "'1'", "'3'", "'118446744073709551616.000000000'", "'100000000000000000000.00000000x'");
      for (const value of bad) reject(`UPDATE ${table} SET ${name}=${value}`);
    }
  }
  for (const engine of ["go", "bun"]) {
    const row = query(`SELECT * FROM installation_identity WHERE id='${engine}'`)[0];
    if (!row) continue;
    assert.equal(row.updated_at, clock);
    assert.equal(row.role, "standalone-primary");
    assert.equal(row.leadership_generation, 1n);
    assert.equal(row.latest_known_generation, 1n);
    assert.equal(query(`SELECT wrapped_kek FROM cluster_keys WHERE purpose='${engine}'`)[0]!.wrapped_kek, opaque);
    assert.equal(query(`SELECT archive_blob FROM standalone_archives WHERE capture_reason='${engine}'`)[0]!.archive_blob, opaque);
    const state = query(`SELECT enrolled_at,sync_enabled,consecutive_failures,updated_at FROM node_state WHERE id='${engine}'`)[0]!;
    assert.deepEqual(state, { enrolled_at: "0", sync_enabled: 0n, consecutive_failures: 0n, updated_at: clock });
    const archive = query(`SELECT restoration_eligible,expires_at,captured_at,created_at FROM standalone_archives WHERE capture_reason='${engine}'`)[0]!;
    assert.deepEqual(archive, { restoration_eligible: 1n, expires_at: "2", captured_at: clock, created_at: clock });
    assert.equal(query(`SELECT retired_at FROM cluster_keys WHERE purpose='${engine}'`)[0]!.retired_at, "2");
    assert.equal(query(`SELECT status FROM applied_snapshots WHERE content_hash='${engine}'`)[0]!.status, "applied");
    for (const [suffix, bytes] of [["", [0, 255, 195, 40]], ["-null", null], ["-empty", []], ["-nul", [0]]] as const) {
      const r = query(`SELECT snapshot_body AS body,typeof(snapshot_body) AS kind,applied_at FROM applied_snapshots WHERE content_hash='${engine}${suffix}'`)[0]!;
      assert(r);
      assert.equal(r.kind, bytes === null ? "null" : "blob");
      assert.deepEqual(r.body === null ? null : Array.from(r.body as Uint8Array), bytes);
      assert.equal(r.applied_at, clock);
    }
    assert.equal(query(`SELECT leadership_generation FROM applied_snapshots WHERE content_hash='${engine}'`)[0]!.leadership_generation, engine === "go" ? -9223372036854775808n : 9007199254740993n);
  }
  const before = snapshot();
  db.exec("BEGIN");
  try {
    for (const [table, expected] of Object.entries(columns)) for (const [name, kind, required] of expected) {
      let values: Value[] = [];
      if (name.endsWith("_at")) values = ["0", "2", "100000000000000000000.000000000", "118446744073709551615.999999999", clock];
      if (kind === "INTEGER") values = [0n, 1n, ...(!bool(name) ? [-2147483648n, 2147483647n, -1n, ...(int64(name) ? [-9223372036854775808n, 9223372036854775807n, 9007199254740993n, -9007199254740993n] : [])] : [])];
      if (kind === "BLOB") values = [new Uint8Array(), new Uint8Array([0]), new Uint8Array([255, 195, 40, 0])];
      if (kind === "TEXT" && !uuid(table, name) && !name.endsWith("_at") && !["role", "status"].includes(name)) values = ["", opaque];
      if (uuid(table, name) && !["id", "revision_id", "apply_job_id"].includes(name)) values = [missing, "ffffffff-ffff-ffff-ffff-ffffffffffff"];
      if (name === "role") { values = ["standalone-primary", "primary", "primary-with-nodes", "node", "stale-primary"]; reject(`UPDATE ${table} SET role='other'`); }
      if (name === "status") { values = ["pending", "applied", "rejected", "rolled-back", "archived"]; assert.throws(() => db.exec(`UPDATE ${table} SET status='other'`), /applied_snapshots_status_check/); }
      for (const value of values) {
        db.query(`UPDATE ${table} SET ${name}=? WHERE id=(SELECT id FROM ${table} ORDER BY id LIMIT 1)`).run(value);
        const actual = query(`SELECT ${name} AS v,typeof(${name}) AS kind FROM ${table} ORDER BY id LIMIT 1`)[0]!;
        assert.deepEqual(value instanceof Uint8Array ? Array.from(actual.v as Uint8Array) : actual.v, value instanceof Uint8Array ? Array.from(value) : value);
        assert.equal(actual.kind, kind.toLowerCase());
      }
      if (!required) {
        db.exec(`UPDATE ${table} SET ${name}=NULL`);
        assert(query(`SELECT typeof(${name}) AS kind FROM ${table}`).every((r) => r.kind === "null"));
      }
    }
  } finally { db.exec("ROLLBACK"); }
  assert.deepEqual(snapshot(), before);
  for (const col of ["credential_id", "enrollment_attempt_id"]) {
    const other = col === "credential_id" ? "enrollment_attempt_id" : "credential_id";
    reject(`INSERT INTO node_state(id,${col},${other},updated_at) SELECT 'unique-other',${col},NULL,updated_at FROM node_state LIMIT 1`);
  }
  reject(`INSERT INTO applied_snapshots(id,source_primary_id,leadership_generation,snapshot_version,replication_version,content_hash,applied_at) SELECT '${missing}',source_primary_id,0,0,0,content_hash,applied_at FROM applied_snapshots LIMIT 1`);
  db.exec("BEGIN");
  try {
    for (let i = 0; i < 4; i++) {
      db.query("INSERT INTO node_state(id,updated_at) VALUES(?,'0')").run(`free-form ${i} 雪`);
      db.query("INSERT INTO installation_identity(id,installation_id,node_id,updated_at) VALUES(?,?,?,'2')").run(`free-form ${i} 雪`, missing, missing);
    }
    db.exec(`INSERT INTO standalone_archives SELECT '${missing}',snapshot_content_hash,NULL,archive_blob,capture_reason,captured_at,retention_started_at,expires_at,restoration_eligible,created_at FROM standalone_archives LIMIT 1`);
  } finally { db.exec("ROLLBACK"); }
  for (const [table, col, parent] of edges) {
    db.exec("BEGIN");
    try {
      reject(`UPDATE ${table} SET ${col}='${missing}'`);
      db.exec(`INSERT INTO config_revisions(id,revision_number,checksum,snapshot,created_at) VALUES('${missing}',99,'edge','null','0')`);
      if (parent === "apply_jobs") db.exec(`INSERT INTO apply_jobs(id,revision_id,target,correlation_id,created_at) VALUES('${missing}','${missing}','combined','edge','2')`);
      db.exec(`UPDATE ${table} SET ${col}='${missing}'`);
      reject(`DELETE FROM ${parent} WHERE id='${missing}'`);
      reject(`UPDATE ${parent} SET id='ffffffff-ffff-ffff-ffff-ffffffffffff' WHERE id='${missing}'`);
      db.exec(`UPDATE ${table} SET ${col}=NULL; DELETE FROM ${table}; DELETE FROM ${parent} WHERE id='${missing}'`);
    } finally { db.exec("ROLLBACK"); }
    assert.deepEqual(snapshot(), before);
  }
  migrateSQLite(db);
  assert.deepEqual(query("SELECT * FROM proxycore_schema_migrations ORDER BY version"), ledger);
} finally { db.close(); }
