import assert from "node:assert/strict";
import { isAbsolute } from "node:path";
import { openSQLite } from "./open.js";
import { migrateSQLite } from "./migrate.js";
import { SQLiteWriter } from "./write.js";

const [path, mode] = process.argv.slice(2);
assert(path && isAbsolute(path));
const db = openSQLite(path);
const raw = '{"n":922337203685477580812345,"text":"雪🦊"}';
try {
  migrateSQLite(db);
  const ledger = db.query("SELECT * FROM proxycore_schema_migrations").all();
  assert.equal(ledger.length, 8);
  assert.equal((ledger[5] as { name: string }).name, "enrollment_product");
  // Full column order/type/NOT NULL/default/PK contract, including absent clocks.
  const columns = {
    users: [
      ["id", "TEXT", 1n, null, 1n], ["username", "TEXT", 1n, null, 0n],
      ["password_hash", "TEXT", 1n, null, 0n], ["role", "TEXT", 1n, null, 0n],
      ["active", "INTEGER", 1n, "1", 0n], ["password_change_required", "INTEGER", 1n, "0", 0n],
      ["created_at", "TEXT", 1n, null, 0n], ["updated_at", "TEXT", 1n, null, 0n],
    ],
    sessions: [
      ["id", "TEXT", 1n, null, 1n], ["user_id", "TEXT", 1n, null, 0n],
      ["token_hash", "TEXT", 1n, null, 0n], ["expires_at", "TEXT", 1n, null, 0n],
      ["revoked_at", "TEXT", 0n, null, 0n], ["last_seen_at", "TEXT", 0n, null, 0n],
      ["created_at", "TEXT", 1n, null, 0n],
    ],
    audit_events: [
      ["id", "TEXT", 1n, null, 1n], ["actor_user_id", "TEXT", 0n, null, 0n],
      ["action", "TEXT", 1n, null, 0n], ["resource_type", "TEXT", 1n, null, 0n],
      ["resource_id", "TEXT", 0n, null, 0n], ["before_value", "TEXT", 0n, null, 0n],
      ["after_value", "TEXT", 0n, null, 0n], ["correlation_id", "TEXT", 1n, null, 0n],
      ["result", "TEXT", 1n, null, 0n], ["created_at", "TEXT", 1n, null, 0n],
    ],
  };
  for (const [table, expected] of Object.entries(columns)) {
    const info = db.query<{ name: string; type: string; notnull: bigint; dflt_value: string | null; pk: bigint }, []>(`PRAGMA table_info(${table})`).all();
    assert.deepEqual(info.map((c) => [c.name, c.type, c.notnull, c.dflt_value, c.pk]), expected);
    const ddl = db.query<{ sql: string }, [string]>("SELECT sql FROM sqlite_schema WHERE name=?").get(table)!.sql;
    assert(!/DEFAULT.*(random|now)|IF NOT EXISTS/i.test(ddl));
    assert(ddl.endsWith(" STRICT"));
    for (const c of info.filter((c) => c.name.endsWith("_at"))) {
      assert(ddl.includes(`${c.name} TEXT COLLATE BINARY`));
    }
  }
  assert.deepEqual(db.query<{ name: string }, []>("SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name").all().map((r) => r.name), ["applied_snapshots", "apply_jobs", "audit_events", "certificates", "cluster_keys", "config_revisions", "dns_records", "enrolled_nodes", "enrollment_attempts", "enrollment_grants", "enrollment_tokens", "forwarding_rules", "health_observations", "installation_identity", "installation_settings", "internal_ca", "internal_ca_enrollment_state", "node_credentials", "node_snapshot_acks", "node_state", "operational_artifacts", "primary_trust_anchors", "provider_connections", "proxycore_schema_migrations", "resolver_pools", "secrets", "sessions", "standalone_archives", "stream_routes", "sync_attempts", "users", "zones"]);
  for (const [table, index, column] of [["users", "users_username_idx", "username"], ["sessions", "sessions_token_hash_idx", "token_hash"]]) {
    const indexes = db.query<{ name: string; unique: bigint; partial: bigint }, []>(`PRAGMA index_list(${table})`).all();
    assert(indexes.some((i) => i.name === index && i.unique === 1n && i.partial === 0n));
    const info = db.query<{ name: string; coll: string; key: bigint }, []>(`PRAGMA index_xinfo(${index})`).all().filter((i) => i.key === 1n);
    assert.deepEqual(info.map((i) => [i.name, i.coll]), [[column, "BINARY"]]);
  }
  for (const [table, from, deletion] of [["sessions", "user_id", "CASCADE"], ["audit_events", "actor_user_id", "NO ACTION"]]) {
    const keys = db.query<{ table: string; from: string; to: string; on_delete: string; on_update: string }, []>(`PRAGMA foreign_key_list(${table})`).all();
    assert.deepEqual(keys.map((k) => [k.table, k.from, k.to, k.on_delete, k.on_update]), [["users", from, "id", deletion, "NO ACTION"]]);
  }
  if (mode === "insert") {
    let calls = 0;
    const writer = new SQLiteWriter(db, () => {
      calls++;
      return { kind: "finite", seconds: -1n, nanos: 123 };
    });
    writer.write((v) => {
      const user = v.newUUID();
      v.db.query("INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES(?,'bun','$hash雪','operator',?,?)").run(user, v.timestamp, v.timestamp);
      v.db.query("INSERT INTO sessions(id,user_id,token_hash,expires_at,created_at) VALUES(?,?,'bun-token','2',?)").run(v.newUUID(), user, v.timestamp);
      v.db.query("INSERT INTO audit_events(id,actor_user_id,action,resource_type,before_value,after_value,correlation_id,result,created_at) VALUES(?,?,'write','user',NULL,?,'bun','ok',?)").run(v.newUUID(), user, raw, v.timestamp);
    });
    assert.equal(calls, 1);
  }
  for (const [table, expected] of Object.entries(columns)) {
    for (const [column] of expected) {
      if (column === "active" || column === "password_change_required") continue;
      for (const value of [`coalesce(${column},'0') || char(0) || 'suffix'`, `CAST(coalesce(${column},'null') AS BLOB)`]) {
        assert.throws(() => db.exec(`UPDATE ${table} SET ${column}=${value}`), /constraint|cannot store/i);
      }
    }
  }
  for (const sql of [
    "UPDATE users SET role='admin'", "UPDATE users SET active=2", "UPDATE users SET password_change_required=-1",
    "UPDATE users SET active=1.5",
    "UPDATE users SET id=NULL", "UPDATE users SET id='bad'", "UPDATE users SET created_at=NULL",
    "UPDATE users SET updated_at='yesterday'", "UPDATE sessions SET expires_at=NULL",
    "UPDATE sessions SET user_id='00000000-0000-0000-0000-000000000001'",
    "UPDATE audit_events SET before_value='{'", "DELETE FROM users",
    "INSERT INTO users SELECT '00000000-0000-0000-0000-000000000001',CAST(username AS BLOB),password_hash,role,active,password_change_required,created_at,updated_at FROM users",
    "INSERT INTO sessions SELECT '00000000-0000-0000-0000-000000000001',user_id,CAST(token_hash AS BLOB),expires_at,revoked_at,last_seen_at,created_at FROM sessions",
    "INSERT INTO users SELECT '00000000-0000-0000-0000-000000000001',username,password_hash,role,active,password_change_required,created_at,updated_at FROM users",
    "INSERT INTO sessions SELECT '00000000-0000-0000-0000-000000000001',user_id,token_hash,expires_at,revoked_at,last_seen_at,created_at FROM sessions",
  ]) assert.throws(() => db.exec(sql), /constraint|cannot store/i, sql);
  const rows = db.query<{ username: string; created_at: string; updated_at: string; password_hash: string; active: bigint; password_change_required: bigint }, []>("SELECT * FROM users").all();
  assert(rows.length > 0);
  for (const u of rows) {
    assert.equal(u.created_at, u.updated_at);
    assert.equal(u.password_hash, "$hash雪");
    assert.equal(u.active, 1n);
    assert.equal(u.password_change_required, 0n);
  }
  const matches = db.query<{ n: bigint }, [string]>("SELECT count(*) AS n FROM users u JOIN sessions s ON s.user_id=u.id JOIN audit_events a ON a.actor_user_id=u.id WHERE u.created_at=s.created_at AND u.created_at=a.created_at AND s.expires_at='2' AND s.revoked_at IS NULL AND s.last_seen_at IS NULL AND a.before_value IS NULL AND a.after_value=?").get(raw)!;
  assert.equal(matches.n, BigInt(rows.length));
  migrateSQLite(db);
  assert.deepEqual(db.query("SELECT * FROM proxycore_schema_migrations").all(), ledger);
  // Destructive constraint probes are rolled back; shared rows/ledger survive.
  db.exec("BEGIN");
  try {
    db.exec("UPDATE users SET created_at='0',updated_at='2'; UPDATE audit_events SET before_value='null'");
    assert.equal(db.query<{ v: string }, []>("SELECT before_value AS v FROM audit_events LIMIT 1").get()!.v, "null");
    db.exec("DELETE FROM audit_events; DELETE FROM users");
    assert.equal(db.query<{ n: bigint }, []>("SELECT count(*) AS n FROM sessions").get()!.n, 0n);
  } finally {
    db.exec("ROLLBACK");
  }
} finally {
  db.close();
}
