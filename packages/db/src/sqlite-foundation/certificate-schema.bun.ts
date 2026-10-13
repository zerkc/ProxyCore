import assert from "node:assert/strict";
import { isAbsolute } from "node:path";
import { openSQLite } from "./open.js";
import { migrateSQLite } from "./migrate.js";
import { SQLiteWriter } from "./write.js";

const [path, mode] = process.argv.slice(2);
assert(path && isAbsolute(path) && ["insert", "verify"].includes(mode!));
const db = openSQLite(path);
const opaque = '  opaque 雪🦊\n"quoted" non-JSON  ';
const raw = '{"n":922337203685477580812345,"text":"雪🦊"}';
const clock = "109223372036854775807.000000123";
type Column = [string, string, bigint, string | null, bigint];
const text = (name: string, required = 1n, pk = 0n, value: string | null = null): Column => [name, "TEXT", required, value, pk];
const id = text("id", 1n, 1n);
const clocks = [text("created_at"), text("updated_at")];
// Independent effective migration order, including columns appended by PG history.
const columns: Record<string, Column[]> = {
  secrets: [id, text("purpose"), text("ciphertext"), ...clocks],
  certificates: [id, text("hostnames"), text("issuer"), text("challenge"), text("environment", 1n, 0n, "'production'"), text("status", 1n, 0n, "'pending'"), text("expires_at", 0n), text("renew_after", 0n), text("key_secret_id", 0n), text("certificate_pem", 0n), ...clocks, text("failure_reason", 0n)],
  provider_connections: [id, text("provider"), text("name"), text("secret_id"), text("scope"), ["enabled", "INTEGER", 1n, "1", 0n], ...clocks],
  internal_ca: [id, text("certificate_pem"), text("key_secret_id"), ...clocks, text("enrollment_certificate_pem", 0n), text("enrollment_key_secret_id", 0n)],
  internal_ca_enrollment_state: [id, text("established_at")],
};
const query = (sql: string) => db.query<Record<string, string | bigint | null>, []>(sql).all();
const count = (table: string) => query(`SELECT count(*) AS n FROM ${table}`)[0]!.n;
const snapshot = () => Object.keys(columns).map((table) => query(`SELECT * FROM ${table} ORDER BY id`));
try {
  migrateSQLite(db);
  const ledger = query("SELECT * FROM proxycore_schema_migrations ORDER BY version");
  assert.equal(ledger.length, 6);
  assert.equal(ledger[5]!.name, "enrollment_product");
  assert.equal(ledger[3]!.name, "operational_product");
  assert.equal(ledger[2]!.name, "certificate_product");
  assert.deepEqual(query("SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name").map((r) => r.name), ["applied_snapshots", "apply_jobs", "audit_events", "certificates", "cluster_keys", "config_revisions", "dns_records", "enrolled_nodes", "enrollment_attempts", "enrollment_grants", "enrollment_tokens", "forwarding_rules", "health_observations", "installation_identity", "installation_settings", "internal_ca", "internal_ca_enrollment_state", "node_credentials", "node_snapshot_acks", "node_state", "operational_artifacts", "provider_connections", "proxycore_schema_migrations", "resolver_pools", "secrets", "sessions", "standalone_archives", "stream_routes", "sync_attempts", "users", "zones"]);
  for (const [table, expected] of Object.entries(columns)) {
    assert.deepEqual(query(`PRAGMA table_info(${table})`).map((c) => [c.name, c.type, c.notnull, c.dflt_value, c.pk]), expected);
    const ddl = query(`SELECT sql FROM sqlite_schema WHERE name='${table}'`)[0]!.sql as string;
    assert(ddl.endsWith(" STRICT"));
    for (const [name] of expected) if (name.endsWith("_at") || name === "renew_after") assert(ddl.includes(`${name} TEXT COLLATE BINARY`));
    assert.deepEqual(query(`PRAGMA index_list(${table})`).filter((i) => i.origin !== "pk"), []);
    const fks = expected.filter(([name]) => name.endsWith("secret_id")).map(([name]) => ["secrets", name, "id", "NO ACTION", "NO ACTION"]);
    assert.deepEqual(query(`PRAGMA foreign_key_list(${table})`).sort((a, b) => String(a.from).localeCompare(String(b.from))).map((k) => [k.table, k.from, k.to, k.on_delete, k.on_update]), fks.sort((a, b) => a[1]!.localeCompare(b[1]!)));
  }
  if (mode === "insert") {
    if (count("secrets") === 0n) for (const table of [...Object.keys(columns), "config_revisions", "apply_jobs", "health_observations", "operational_artifacts", "installation_identity", "cluster_keys", "node_state", "applied_snapshots", "standalone_archives"]) assert.equal(count(table), 0n);
    let calls = 0;
    new SQLiteWriter(db, () => { calls++; return { kind: "finite", seconds: -1n, nanos: 123 }; }).write((v) => {
      const key = v.newUUID();
      db.query("INSERT INTO secrets VALUES(?,'bun',?,?,?)").run(key, opaque, v.timestamp, v.timestamp);
      db.query("INSERT INTO certificates(id,hostnames,issuer,challenge,key_secret_id,created_at,updated_at) VALUES(?,?,'uploaded','none',?,?,?)").run(key, raw, key, v.timestamp, v.timestamp);
      db.query("INSERT INTO provider_connections(id,provider,name,secret_id,scope,created_at,updated_at) VALUES(?,'bun','雪',?,'any',?,?)").run(key, key, v.timestamp, v.timestamp);
      db.query("INSERT INTO internal_ca(id,certificate_pem,key_secret_id,created_at,updated_at,enrollment_key_secret_id) VALUES('bun','not parsed',?,?,?,?)").run(key, v.timestamp, v.timestamp, key);
      db.query("INSERT INTO internal_ca_enrollment_state VALUES('bun',?)").run(v.timestamp);
    });
    assert.equal(calls, 1);
  }
  for (const [table, expected] of Object.entries(columns)) {
    assert((count(table) as bigint) > 0n);
    for (const [name, kind, required] of expected) {
      const bad = kind === "TEXT" ? [`coalesce(${name},'x')||char(0)||'tail'`, "CAST('x' AS BLOB)"] : ["2", "-1", "1.5"];
      if (required) bad.push("NULL");
      if (name === "id" && !table.startsWith("internal_ca") || name.endsWith("secret_id")) bad.push("'bad'", "'AAAAAAAA-0000-0000-0000-000000000000'");
      if (name.endsWith("_at") || name === "renew_after") bad.push("'bad'", "'118446744073709551616.000000000'", "'100000000000000000000.00000000x'");
      for (const value of bad) assert.throws(() => db.exec(`UPDATE ${table} SET ${name}=${value}`), /constraint|cannot store/i);
      if (["id", "created_at", "updated_at", "established_at"].includes(name)) {
        const others = expected.filter(([n]) => n !== name).map(([n]) => n).join(",");
        assert.throws(() => db.exec(`INSERT INTO ${table}(${others}) SELECT ${others} FROM ${table} LIMIT 1`), /constraint/i);
      }
    }
    assert.throws(() => db.exec(`INSERT INTO ${table} SELECT * FROM ${table} LIMIT 1`), /constraint/i);
    const historical = table === "internal_ca_enrollment_state" ? "established_at" : "created_at";
    assert(query(`SELECT * FROM ${table}`).every((r) => r[historical] === clock && (table === "internal_ca_enrollment_state" || r.updated_at === clock)));
  }
  assert(query("SELECT ciphertext,typeof(ciphertext) AS kind FROM secrets").every((r) => r.ciphertext === opaque && r.kind === "text"));
  assert(query("SELECT * FROM certificates").every((r) => r.hostnames === raw && r.environment === "production" && r.status === "pending" && r.expires_at === null && r.renew_after === null && r.certificate_pem === null && r.failure_reason === null));
  assert(query("SELECT * FROM provider_connections").every((r) => r.enabled === 1n));
  for (const assignment of ["issuer='other'", "challenge='other'", "status='other'", "hostnames='{'", "hostnames=NULL"]) assert.throws(() => db.exec(`UPDATE certificates SET ${assignment}`), /constraint/i);
  const before = snapshot();
  db.exec("BEGIN");
  try {
    for (const [column, values] of Object.entries({ issuer: ["self-signed", "uploaded", "letsencrypt"], challenge: ["none", "http-01", "dns-01"], status: ["pending", "issued", "active", "expired", "failed"], hostnames: ["null", "true", "123", '"scalar"', "922337203685477580812345"], environment: ["", "arbitrary 雪"] })) {
      for (const value of values) {
        db.query(`UPDATE certificates SET ${column}=?`).run(value);
        assert(query(`SELECT ${column} AS v,typeof(${column}) AS kind FROM certificates`).every((r) => r.v === value && r.kind === "text"));
      }
    }
    for (const [table, expected] of Object.entries(columns)) for (const [name, , required] of expected) {
      if (name.endsWith("_at") || name === "renew_after") for (const value of ["0", "2", "100000000000000000000.000000000", "118446744073709551615.999999999"]) db.query(`UPDATE ${table} SET ${name}=?`).run(value);
      if (!required) db.exec(`UPDATE ${table} SET ${name}=NULL`);
    }
    db.exec("UPDATE provider_connections SET enabled=0");
    db.exec("INSERT INTO internal_ca SELECT 'free-form 雪',certificate_pem,key_secret_id,created_at,updated_at,NULL,NULL FROM internal_ca LIMIT 1; INSERT INTO internal_ca_enrollment_state VALUES('another 雪','0')");
  } finally { db.exec("ROLLBACK"); }
  assert.deepEqual(snapshot(), before);
  for (const [table, column] of [["certificates", "key_secret_id"], ["provider_connections", "secret_id"], ["internal_ca", "key_secret_id"], ["internal_ca", "enrollment_key_secret_id"]]) {
    db.exec("BEGIN");
    try {
      assert.throws(() => db.exec(`UPDATE ${table} SET ${column}='00000000-0000-0000-0000-000000000099'`), /constraint/i);
      for (const child of ["certificates", "provider_connections", "internal_ca"]) if (child !== table) db.exec(`DELETE FROM ${child}`);
      if (table === "internal_ca") {
        if (column === "key_secret_id") db.exec("UPDATE internal_ca SET enrollment_key_secret_id=NULL");
        else db.exec("INSERT INTO secrets VALUES('00000000-0000-0000-0000-000000000099','fixture','opaque','0','2'); UPDATE internal_ca SET key_secret_id='00000000-0000-0000-0000-000000000099'");
      }
      assert.throws(() => db.exec("DELETE FROM secrets WHERE purpose!='fixture'"), /constraint/i);
      assert.throws(() => db.exec("UPDATE secrets SET id='00000000-0000-0000-0000-000000000098' WHERE purpose!='fixture'"), /constraint/i);
      db.exec(`DELETE FROM ${table}; DELETE FROM secrets`);
      assert.equal(count("secrets"), 0n);
    } finally { db.exec("ROLLBACK"); }
    assert.deepEqual(snapshot(), before);
  }
  migrateSQLite(db);
  assert.deepEqual(query("SELECT * FROM proxycore_schema_migrations ORDER BY version"), ledger);
  db.exec("CREATE INDEX certificate_drift ON certificates(status)");
  assert.throws(() => migrateSQLite(db), /schema/i);
  assert.deepEqual(snapshot(), before);
  assert.deepEqual(query("SELECT * FROM proxycore_schema_migrations ORDER BY version"), ledger);
  db.exec("DROP INDEX certificate_drift");
} finally { db.close(); }
