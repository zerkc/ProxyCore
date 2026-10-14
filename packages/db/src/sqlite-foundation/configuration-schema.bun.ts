import assert from "node:assert/strict";
import { isAbsolute } from "node:path";
import { openSQLite } from "./open.js";
import { migrateSQLite } from "./migrate.js";
import { SQLiteWriter } from "./write.js";

const [path, mode] = process.argv.slice(2);
assert(path && isAbsolute(path));
const db = openSQLite(path);
const raw = '{"n":922337203685477580812345,"text":"雪🦊"}';
// Independent contract descriptors: name, storage type, required, default, PK.
type Column = [string, string, bigint, string | null, bigint];
const text = (name: string, required = 1n, pk = 0n): Column => [name, "TEXT", required, null, pk];
const integer = (name: string, value: string | null): Column => [name, "INTEGER", 1n, value, 0n];
const clocks = [text("created_at"), text("updated_at")];
const columns: Record<string, Column[]> = {
  installation_settings: [text("id", 1n, 1n), ...["ingress_ipv4", "ingress_ipv6", "default_resolver_pool", "forwarding_rules", "enrollment_hostnames"].map((n) => text(n, 0n)), integer("retention_max_age_days", "7"), integer("retention_max_size_mb", "50"), text("current_desired_revision_id", 0n), text("current_applied_revision_id", 0n), ...clocks],
  zones: [text("id", 1n, 1n), text("name"), integer("enabled", "1"), ...clocks],
  dns_records: [text("id", 1n, 1n), text("zone_id"), text("name"), text("type"), text("value"), integer("ttl", "300"), integer("enabled", "1"), integer("proxied", "0"), text("proxy_settings", 0n), text("comment", 0n), ...clocks],
  resolver_pools: [text("id", 1n, 1n), text("name"), text("endpoints"), integer("is_default", "0"), ...clocks],
  forwarding_rules: [text("id", 1n, 1n), text("suffix"), text("pool_id"), ...clocks],
  stream_routes: [text("id", 1n, 1n), integer("enabled", "1"), text("protocol"), text("listen_address"), integer("listen_port", null), text("upstream"), ...clocks],
};
const indexes: Record<string, [string, string[]]> = {
  zones: ["zones_name_idx", ["name"]], dns_records: ["dns_records_zone_name_id_idx", ["zone_id", "name", "id"]],
  resolver_pools: ["resolver_pools_name_idx", ["name"]], forwarding_rules: ["forwarding_rules_suffix_idx", ["suffix"]],
  stream_routes: ["stream_routes_listener_idx", ["protocol", "listen_address", "listen_port"]],
};
const query = (sql: string) => db.query<Record<string, string | bigint | null>, []>(sql).all();
const count = (table: string) => query(`SELECT count(*) AS n FROM ${table}`)[0]!.n;
try {
  migrateSQLite(db);
  const ledger = query("SELECT * FROM proxycore_schema_migrations ORDER BY version");
  assert.equal(ledger.length, 7);
  assert.equal(ledger[5]!.name, "enrollment_product");
  assert.equal(ledger[3]!.name, "operational_product");
  assert.equal(ledger[2]!.name, "certificate_product");
  assert.equal(ledger[1]!.name, "configuration_core");
  assert.deepEqual(query("SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name").map((r) => r.name), ["applied_snapshots", "apply_jobs", "audit_events", "certificates", "cluster_keys", "config_revisions", "dns_records", "enrolled_nodes", "enrollment_attempts", "enrollment_grants", "enrollment_tokens", "forwarding_rules", "health_observations", "installation_identity", "installation_settings", "internal_ca", "internal_ca_enrollment_state", "node_credentials", "node_snapshot_acks", "node_state", "operational_artifacts", "primary_trust_anchors", "provider_connections", "proxycore_schema_migrations", "resolver_pools", "secrets", "sessions", "standalone_archives", "stream_routes", "sync_attempts", "users", "zones"]);
  for (const [table, expected] of Object.entries(columns)) {
    assert.deepEqual(query(`PRAGMA table_info(${table})`).map((c) => [c.name, c.type, c.notnull, c.dflt_value, c.pk]), expected);
    const ddl = query(`SELECT sql FROM sqlite_schema WHERE name='${table}'`)[0]!.sql as string;
    assert(ddl.endsWith(" STRICT"));
    assert(!/IF NOT EXISTS|DEFAULT.*(random|now)/i.test(ddl));
    for (const [clock] of clocks) assert(ddl.includes(`${clock} TEXT COLLATE BINARY`));
    const actual = query(`PRAGMA index_list(${table})`).filter((i) => i.origin !== "pk");
    const index = indexes[table];
    assert.equal(actual.length, index ? 1 : 0);
    if (index) {
      assert.deepEqual(actual.map((i) => [i.name, i.unique, i.partial]), [[index[0], 1n, 0n]]);
      assert.deepEqual(query(`PRAGMA index_xinfo(${index[0]})`).filter((i) => i.key === 1n).map((i) => [i.name, i.coll]), index[1].map((c) => [c, "BINARY"]));
    }
    const fk = table === "dns_records" ? ["zones", "zone_id"] : table === "forwarding_rules" ? ["resolver_pools", "pool_id"] : null;
    assert.deepEqual(query(`PRAGMA foreign_key_list(${table})`).map((k) => [k.table, k.from, k.to, k.on_delete, k.on_update]), fk ? [[...fk, "id", "CASCADE", "NO ACTION"]] : []);
    if (mode === "empty") assert.equal(count(table), 0n);
  }
  if (mode === "empty") process.exitCode = 0;
  else {
    if (mode === "insert") {
      if (count("installation_settings") === 0n) for (const table of [...Object.keys(columns), "users", "sessions", "audit_events", "secrets", "certificates", "provider_connections", "internal_ca", "internal_ca_enrollment_state", "config_revisions", "apply_jobs", "health_observations", "operational_artifacts", "installation_identity", "cluster_keys", "node_state", "applied_snapshots", "standalone_archives"]) assert.equal(count(table), 0n);
      let calls = 0;
      new SQLiteWriter(db, () => { calls++; return { kind: "finite", seconds: -1n, nanos: 123 }; }).write((v) => {
        const zone = v.newUUID(), pool = v.newUUID();
        db.query("INSERT INTO installation_settings(id,default_resolver_pool,forwarding_rules,enrollment_hostnames,created_at,updated_at) VALUES('bun',NULL,'null',?,?,?)").run(raw, v.timestamp, v.timestamp);
        db.query("INSERT INTO zones(id,name,created_at,updated_at) VALUES(?,'bun雪',?,?)").run(zone, v.timestamp, v.timestamp);
        db.query("INSERT INTO resolver_pools(id,name,endpoints,created_at,updated_at) VALUES(?,'bun雪',?,?,?)").run(pool, raw, v.timestamp, v.timestamp);
        db.query("INSERT INTO forwarding_rules(id,suffix,pool_id,created_at,updated_at) VALUES(?,'bun雪',?,?,?)").run(zone, pool, v.timestamp, v.timestamp);
        db.query("INSERT INTO stream_routes(id,protocol,listen_address,listen_port,upstream,created_at,updated_at) VALUES(?,'udp','bun雪',-2147483648,?,?,?)").run(zone, raw, v.timestamp, v.timestamp);
        db.query("INSERT INTO dns_records(id,zone_id,name,type,value,created_at,updated_at) VALUES(?,?,'雪','A',?,?,?)").run(zone, zone, raw, v.timestamp, v.timestamp);
        db.query("INSERT INTO dns_records(id,zone_id,name,type,value,created_at,updated_at) VALUES(?,?,'雪','TXT','null',?,?)").run(pool, zone, v.timestamp, v.timestamp);
      });
      assert.equal(calls, 1);
    }
    for (const [table, expected] of Object.entries(columns)) {
      assert((count(table) as bigint) > 0n);
      for (const [column, kind, required] of expected) {
        const bad = kind === "TEXT" ? [`coalesce(${column},'null')||char(0)||'suffix'`, `CAST(coalesce(${column},'null') AS BLOB)`] : ["1.5"];
        if (required) bad.push("NULL");
        if (column === "id" && table !== "installation_settings" || column === "zone_id" || column === "pool_id") bad.push("'bad'", "'AAAAAAAA-0000-0000-0000-000000000000'");
        if (column.endsWith("_at")) bad.push("'bad'", "'118446744073709551616.000000000'", "'100000000000000000000.00000000x'");
        if (["enabled", "proxied", "is_default"].includes(column)) bad.push("2", "-1");
        if (["ttl", "listen_port", "retention_max_age_days", "retention_max_size_mb"].includes(column)) bad.push("2147483648", "-2147483649");
        if (["value", "proxy_settings", "endpoints", "upstream", "default_resolver_pool", "forwarding_rules", "enrollment_hostnames"].includes(column)) bad.push("'{' ");
        for (const value of bad) assert.throws(() => db.exec(`UPDATE ${table} SET ${column}=${value}`), /constraint|cannot store/i, `${table}.${column}: ${value}`);
      }
      assert.equal(count(table), query(`SELECT count(*) AS n FROM ${table} WHERE created_at=updated_at AND created_at='109223372036854775807.000000123'`)[0]!.n);
    }
    for (const sql of ["UPDATE dns_records SET type='NS'", "UPDATE stream_routes SET protocol='TCP'", "UPDATE zones SET name='duplicate'", "UPDATE resolver_pools SET name='duplicate'", "UPDATE forwarding_rules SET suffix='duplicate'", "UPDATE stream_routes SET protocol='tcp',listen_address='duplicate',listen_port=0", "UPDATE dns_records SET zone_id='00000000-0000-0000-0000-000000000000'", "UPDATE forwarding_rules SET pool_id='00000000-0000-0000-0000-000000000000'"]) {
      // Uniqueness probes require both engines' rows; other constraints always apply.
      if (!sql.includes("duplicate") || count("zones") === 2n) assert.throws(() => db.exec(sql), /constraint/i, sql);
    }
    for (const table of ["installation_settings", "dns_records", "resolver_pools", "stream_routes"]) {
      const column = { installation_settings: "enrollment_hostnames", dns_records: "value", resolver_pools: "endpoints", stream_routes: "upstream" }[table]!;
      assert(query(`SELECT ${column} AS v,typeof(${column}) AS kind FROM ${table}`).some((r) => r.v === raw && r.kind === "text"));
    }
    assert(query("SELECT * FROM installation_settings").every((r) => r.default_resolver_pool === null && r.forwarding_rules === "null" && r.retention_max_age_days === 7n && r.retention_max_size_mb === 50n));
    assert(query("SELECT * FROM dns_records").every((r) => r.ttl === 300n && r.enabled === 1n && r.proxied === 0n && r.proxy_settings === null && r.comment === null));
    assert(query("SELECT * FROM resolver_pools").every((r) => r.is_default === 0n));
    for (const table of ["zones", "stream_routes"]) assert(query(`SELECT * FROM ${table}`).every((r) => r.enabled === 1n));
    const before = Object.keys(columns).map((table) => query(`SELECT * FROM ${table} ORDER BY id`));
    db.exec("BEGIN");
    try {
      db.exec("UPDATE resolver_pools SET is_default=1; UPDATE installation_settings SET retention_max_age_days=-2147483648,retention_max_size_mb=0,current_desired_revision_id='not-a-uuid',current_applied_revision_id='雪',default_resolver_pool='null'; UPDATE stream_routes SET listen_port=0; UPDATE dns_records SET ttl=-2147483648,proxy_settings='null'");
      assert.equal(count("resolver_pools"), query("SELECT count(*) AS n FROM resolver_pools WHERE is_default=1")[0]!.n);
      db.exec("UPDATE resolver_pools SET endpoints='922337203685477580812345'");
      assert(query("SELECT endpoints,typeof(endpoints) AS kind FROM resolver_pools").every((r) => r.endpoints === "922337203685477580812345" && r.kind === "text"));
      for (const type of ["A", "AAAA", "CNAME", "TXT", "MX", "SRV"]) db.exec(`UPDATE dns_records SET type='${type}'`);
      for (const sql of ["UPDATE zones SET id='00000000-0000-0000-0000-000000000000'", "UPDATE resolver_pools SET id='00000000-0000-0000-0000-000000000000'"]) assert.throws(() => db.exec(sql), /constraint/i);
      for (const table of Object.keys(columns)) db.exec(`UPDATE ${table} SET created_at='0',updated_at='2'`);
      db.exec("DELETE FROM zones; DELETE FROM resolver_pools");
      assert.equal(count("dns_records"), 0n);
      assert.equal(count("forwarding_rules"), 0n);
    } finally { db.exec("ROLLBACK"); }
    assert.deepEqual(Object.keys(columns).map((table) => query(`SELECT * FROM ${table} ORDER BY id`)), before);
    migrateSQLite(db);
    assert.deepEqual(query("SELECT * FROM proxycore_schema_migrations ORDER BY version"), ledger);
    db.exec("DROP INDEX stream_routes_listener_idx");
    assert.throws(() => migrateSQLite(db), /schema/i);
    db.exec("CREATE UNIQUE INDEX stream_routes_listener_idx ON stream_routes(protocol,listen_address,listen_port)");
  }
} finally { db.close(); }
