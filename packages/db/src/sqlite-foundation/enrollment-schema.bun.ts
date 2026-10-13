import assert from "node:assert/strict";
import { isAbsolute } from "node:path";
import { openSQLite } from "./open.js";
import { migrateSQLite } from "./migrate.js";
import { SQLiteWriter } from "./write.js";

const [path, mode] = process.argv.slice(2);
assert(path && isAbsolute(path) && ["insert", "verify"].includes(mode!));
const db = openSQLite(path);
const opaque = '  opaque 雪🦊\n"quoted" non-JSON  ';
const clock = "109223372036854775807.000000123";
const missing = "00000000-0000-0000-0000-000000000099";
const key = (engine: string) => `00000000-0000-0000-0000-0000000000${engine === "go" ? "61" : "62"}`;
type Value = string | bigint | null;
type Column = [string, string, bigint, string | null, bigint];
const text = (n: string, required = 1n, pk = 0n, value: string | null = null): Column => [n, "TEXT", required, value, pk];
const optional = (n: string): Column => text(n, 0n);
const integer = (n: string, nullable = false): Column => [n, "INTEGER", nullable ? 0n : 1n, null, 0n];
const id = text("id", 1n, 1n);
// Independent effective PG order. The grant digest is the 0007 tail.
const columns: Record<string, Column[]> = {
  enrollment_attempts: [id, text("state", 1n, 0n, "'draft'"), text("primary_url"), optional("expected_primary_id"), optional("verified_primary_id"), optional("verified_primary_node_id"), integer("verified_leadership_generation", true), optional("verified_primary_tls_spki_sha256"), optional("verified_primary_ca_fingerprint"), optional("preview_digest"), text("local_node_ip"), optional("archive_id"), text("ephemeral_private_key_wrapped"), optional("bootstrap_payload"), optional("node_credential_secret_id"), optional("cluster_key_id"), optional("initial_snapshot_hash"), optional("initial_snapshot_revision_id"), optional("initial_apply_job_id"), optional("failure_code"), optional("confirmed_at"), text("created_at"), text("updated_at")],
  enrollment_tokens: [id, text("token_selector"), text("token_hash"), text("hash_version"), text("created_by_user_id"), text("created_at"), text("expires_at"), optional("consumed_at"), optional("consumed_by_attempt_id"), optional("revoked_at")],
  node_credentials: [id, text("node_id"), text("credential_hash"), text("hash_version"), text("created_at"), optional("last_authenticated_at"), optional("revoked_at")],
  enrolled_nodes: [text("node_id", 1n, 1n), text("installation_id"), text("primary_id"), optional("display_name"), text("credential_id"), text("enrolled_at"), optional("revoked_at"), text("created_by_attempt_id")],
  enrollment_grants: [text("attempt_id", 1n, 1n), text("token_id"), text("installation_id"), text("node_id"), text("primary_id"), integer("primary_generation"), text("node_ephemeral_public_key"), text("sealed_bootstrap_payload"), text("payload_hash"), text("created_at"), text("expires_at"), optional("verified_preview_digest")],
  node_snapshot_acks: [text("node_id", 1n, 1n), text("content_hash", 1n, 2n), integer("snapshot_version"), integer("replication_version"), text("revision_id"), integer("leadership_generation"), text("applied_at"), text("received_at")],
  sync_attempts: [id, text("node_id"), text("trigger"), text("status", 1n, 0n, "'queued'"), optional("source_primary_id"), integer("leadership_generation", true), integer("snapshot_version", true), integer("replication_version", true), optional("content_hash"), optional("revision_id"), optional("apply_job_id"), optional("result_code"), optional("started_at"), optional("finished_at"), text("created_at"), text("updated_at")],
};
const states: Record<string, string[]> = {
  "enrollment_attempts.state": ["draft", "verified", "confirmed", "exchanged", "archived", "initial-apply-pending", "committed", "cancelled", "recoverable", "failed"],
  "sync_attempts.status": ["queued", "running", "current", "applied", "failed", "pending-ack"],
  "sync_attempts.trigger": ["enrollment", "periodic", "manual", "restart"],
};
const edges = [
  ["enrollment_attempts", "archive_id", "standalone_archives", "id"],
  ["enrollment_attempts", "cluster_key_id", "cluster_keys", "id"],
  ["enrollment_attempts", "initial_apply_job_id", "apply_jobs", "id"],
  ["enrollment_attempts", "initial_snapshot_revision_id", "config_revisions", "id"],
  ["enrollment_attempts", "node_credential_secret_id", "secrets", "id"],
  ["enrollment_tokens", "created_by_user_id", "users", "id"],
  ["enrolled_nodes", "credential_id", "node_credentials", "id"],
  ["enrollment_grants", "token_id", "enrollment_tokens", "id"],
  ["node_snapshot_acks", "node_id", "enrolled_nodes", "node_id"],
  ["node_snapshot_acks", "revision_id", "config_revisions", "id"],
  ["sync_attempts", "apply_job_id", "apply_jobs", "id"],
  ["sync_attempts", "revision_id", "config_revisions", "id"],
];
const indexes: Record<string, [string, bigint, string, bigint, (string | null)[]][]> = {
  enrollment_attempts: [["enrollment_attempts_one_active_idx", 1n, "c", 1n, [null]]],
  enrollment_tokens: [["enrollment_tokens_selector_idx", 1n, "c", 0n, ["token_selector"]]],
  node_credentials: [["node_credentials_node_idx", 1n, "c", 0n, ["node_id"]]],
  enrolled_nodes: [["enrolled_nodes_attempt_unique", 1n, "c", 0n, ["created_by_attempt_id"]], ["enrolled_nodes_credential_idx", 1n, "c", 0n, ["credential_id"]], ["enrolled_nodes_installation_idx", 1n, "c", 0n, ["installation_id"]]],
  enrollment_grants: [["", 1n, "u", 0n, ["token_id"]]],
  sync_attempts: [["sync_attempts_node_created_idx", 0n, "c", 0n, ["node_id", "created_at"]]],
};
const query = (sql: string) => db.query<Record<string, Value>, []>(sql).all();
const uuid = (n: string) => n === "id" || n.endsWith("_id");
const big = (n: string) => n.includes("generation");
const reject = (sql: string) => assert.throws(() => db.exec(sql), /constraint|cannot store/i);
function values(table: string, engine: string): (string | bigint)[] {
  return columns[table]!.map(([n, kind]) => {
    if (n === "state") return "committed";
    if (n === "status") return "queued";
    if (n === "trigger") return "manual";
    if (n === "expires_at") return "2";
    if (["token_selector", "content_hash"].includes(n)) return engine;
    if (table === "enrolled_nodes" && n === "installation_id") return `10000000${key(engine).slice(8)}`;
    if (["primary_id", "installation_id", "expected_primary_id", "verified_primary_id", "verified_primary_node_id", "source_primary_id"].includes(n)) return missing;
    if (kind === "INTEGER") return big(n) ? 9007199254740993n : -2147483648n;
    if (uuid(n)) return key(engine);
    if (n.endsWith("_at")) return clock;
    return opaque;
  });
}
const snapshot = () => Object.keys(columns).map((table) => query(`SELECT *,${columns[table]!.map(([n]) => `typeof("${n}") AS type_${n}`).join(",")} FROM ${table} ORDER BY rowid`));
try {
  migrateSQLite(db);
  const ledger = query("SELECT * FROM proxycore_schema_migrations ORDER BY version");
  assert.equal(ledger.length, 6);
  assert.equal(ledger[5]!.name, "enrollment_product");
  assert.equal(Object.values(columns).reduce((n, cols) => n + cols.length, 0), 84);
  assert.equal(query("SELECT count(*) AS n FROM sqlite_schema WHERE type='table'")[0]!.n, 31n);
  for (const [table, expected] of Object.entries(columns)) {
    assert.deepEqual(query(`PRAGMA table_info(${table})`).map((c) => [c.name, c.type, c.notnull, c.dflt_value, c.pk]), expected);
    const ddl = query(`SELECT sql FROM sqlite_schema WHERE name='${table}'`)[0]!.sql as string;
    assert(ddl.endsWith(" STRICT"));
    for (const [n] of expected) if (n.endsWith("_at")) assert(ddl.includes(`${n} TEXT COLLATE BINARY`));
    assert.deepEqual(query(`PRAGMA foreign_key_list(${table})`).sort((a, b) => String(a.from).localeCompare(String(b.from))).map((e) => [e.from, e.table, e.to, e.on_delete, e.on_update]), edges.filter(([t]) => t === table).map(([, n, p, to]) => [n, p, to, "NO ACTION", "NO ACTION"]));
    const secondary = query(`PRAGMA index_list(${table})`).filter((i) => i.origin !== "pk").sort((a, b) => String(a.name).localeCompare(String(b.name)));
    assert.deepEqual(secondary.map((i) => [i.origin === "c" ? i.name : "", i.unique, i.origin, i.partial]), (indexes[table] ?? []).map(([n, u, o, p]) => [n, u, o, p]));
    for (const i of query(`PRAGMA index_list(${table})`)) {
      const names = i.origin === "pk" ? expected.filter((c) => c[4] !== 0n).map((c) => c[0]) : (indexes[table] ?? []).find(([n, , o]) => o === i.origin && (o !== "c" || n === i.name))![4];
      assert.deepEqual(query(`PRAGMA index_xinfo('${i.name}')`).filter((x) => x.key === 1n).map((x) => [x.name, x.coll, x.desc]), names.map((n) => [n, "BINARY", 0n]));
    }
  }
  assert.equal(query("SELECT sql FROM sqlite_schema WHERE name='enrollment_attempts_one_active_idx'")[0]!.sql, "CREATE UNIQUE INDEX enrollment_attempts_one_active_idx ON enrollment_attempts((1)) WHERE state NOT IN ('committed','cancelled','failed')");
  assert.equal(query("SELECT sql FROM sqlite_schema WHERE name='sync_attempts_node_created_idx'")[0]!.sql, "CREATE INDEX sync_attempts_node_created_idx ON sync_attempts(node_id ASC,created_at ASC)");
  if (mode === "insert") {
    const k = key("bun");
    db.query("INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES(?,'bun@enrollment.invalid',?,'operator','0','2')").run(k, opaque);
    db.query("INSERT INTO secrets VALUES(?,'enrollment',?,'0','2')").run(k, opaque);
    db.query("INSERT INTO config_revisions(id,revision_number,checksum,snapshot,created_at) VALUES(?,62,'bun','null','0')").run(k);
    db.query("INSERT INTO apply_jobs(id,revision_id,target,correlation_id,created_at) VALUES(?,?,'combined','bun','2')").run(k, k);
    db.query("INSERT INTO standalone_archives(id,archive_blob,capture_reason,captured_at,created_at) VALUES(?,?,'enrollment','0','2')").run(k, opaque);
    db.query("INSERT INTO cluster_keys(id,purpose,wrapped_kek,wrapping_key_version,created_at) VALUES(?,'enrollment',?,-1,'0')").run(k, opaque);
    let calls = 0;
    new SQLiteWriter(db, () => { calls++; return { kind: "finite", seconds: -1n, nanos: 123 }; }).write((v) => {
      for (const [table, cols] of Object.entries(columns)) {
        const row = values(table, "bun");
        for (const [i, [n]] of cols.entries()) if (n.endsWith("_at") && n !== "expires_at") row[i] = v.timestamp;
        db.query(`INSERT INTO ${table}(${cols.map(([n]) => `"${n}"`).join(",")}) VALUES(${cols.map(() => "?").join(",")})`).run(...row);
      }
    });
    assert.equal(calls, 1);
  }
  for (const engine of ["go", "bun"]) {
    if (query(`SELECT id FROM enrollment_attempts WHERE id='${key(engine)}'`).length === 0) continue;
    for (const [table, cols] of Object.entries(columns)) {
      const row = query(`SELECT * FROM ${table} WHERE ${cols[0]![0]}='${key(engine)}'`)[0];
      assert(row, `${table}/${engine}`);
      assert.deepEqual(cols.map(([n]) => row[n]), values(table, engine));
    }
  }
  const before = snapshot();
  for (const [table, cols] of Object.entries(columns)) for (const [n, kind, required] of cols) {
    assert(query(`SELECT count(*) AS n FROM ${table}`)[0]!.n as bigint > 0n);
    db.exec("SAVEPOINT domain");
    try {
      if (["enrollment_tokens", "enrollment_grants"].includes(table)) db.exec(`UPDATE ${table} SET created_at='0',expires_at='2'`);
      const bad = required ? ["NULL"] : [];
      if (kind === "TEXT") bad.push("CAST('x' AS BLOB)", `coalesce("${n}",'x')||char(0)||'tail'`);
      else bad.push("1.5", "CAST('1' AS BLOB)", ...(big(n) ? ["9223372036854775808", "-9223372036854775809"] : ["2147483648", "-2147483649"]));
      if (uuid(n)) bad.push("'bad'", "'AAAAAAAA-0000-0000-0000-000000000000'", "'00000000000000000000000000000000000000'");
      if (n.endsWith("_at")) bad.push("'bad'", "'1'", "'3'", "'118446744073709551616.000000000'", "'100000000000000000000.00000000x'");
      for (const v of bad) reject(`UPDATE ${table} SET "${n}"=${v}`);
      let good: Value[] = [];
      if (kind === "INTEGER") good = [-2147483648n, 2147483647n, -1n, 0n, ...(big(n) ? [-9223372036854775808n, 9223372036854775807n, 9007199254740993n, -9007199254740993n] : [])];
      else if (n.endsWith("_at")) good = ["0", "2", "100000000000000000000.000000000", "118446744073709551615.999999999", clock];
      else if (!uuid(n)) good = ["", opaque, "null", '{"n":922337203685477580812345}'];
      if (states[`${table}.${n}`]) { good = states[`${table}.${n}`]!; reject(`UPDATE ${table} SET "${n}"='other'`); }
      for (const v of good) {
        if (["enrollment_tokens", "enrollment_grants"].includes(table) && (n === "created_at" && v === "2" || n === "expires_at" && v === "0")) continue;
        db.query(`UPDATE ${table} SET "${n}"=? WHERE rowid=(SELECT min(rowid) FROM ${table})`).run(v);
        assert.deepEqual(query(`SELECT "${n}" AS v,typeof("${n}") AS kind FROM ${table} ORDER BY rowid LIMIT 1`)[0], { v, kind: kind.toLowerCase() });
      }
      if (!required) {
        db.exec(`UPDATE ${table} SET "${n}"=NULL`);
        assert(query(`SELECT typeof("${n}") AS kind FROM ${table}`).every((r) => r.kind === "null"));
      }
    } finally { db.exec("ROLLBACK TO domain; RELEASE domain"); }
  }
  assert.deepEqual(snapshot(), before);
  for (const [table, col, parent, to] of edges) {
    reject(`UPDATE ${table} SET ${col}='${missing}'`);
    reject(`DELETE FROM ${parent} WHERE ${to}='${key("bun")}'`);
    reject(`UPDATE ${parent} SET ${to}='${missing}' WHERE ${to}='${key("bun")}'`);
  }
  assert.deepEqual(snapshot(), before);
  const clone = (table: string, overrides: Record<string, string> = {}, omit?: string) => {
    const names = columns[table]!.map(([n]) => n).filter((n) => n !== omit);
    db.exec(`INSERT INTO ${table}(${names.map((n) => `"${n}"`).join(",")}) SELECT ${names.map((n) => overrides[n] ?? `"${n}"`).join(",")} FROM ${table} WHERE rowid=(SELECT min(rowid) FROM ${table})`);
  };
  const other = `'${missing}'`;
  for (const table of Object.keys(columns)) assert.throws(() => clone(table), /UNIQUE constraint/i);
  for (const [table, col] of [["enrollment_tokens", "token_selector"], ["node_credentials", "node_id"], ["enrollment_grants", "token_id"]]) {
    assert.throws(() => clone(table!, { [columns[table!]![0]![0]]: other }), new RegExp(`UNIQUE constraint.*${col}`));
  }
  db.exec("SAVEPOINT keys");
  try {
    db.query("INSERT INTO node_credentials(id,node_id,credential_hash,hash_version,created_at) VALUES(?,?,'opaque','any','0')").run(missing, missing);
    for (const col of ["installation_id", "credential_id", "created_by_attempt_id"]) {
      const replacements: Record<string, string> = { node_id: other, installation_id: other, credential_id: other, created_by_attempt_id: other };
      delete replacements[col];
      assert.throws(() => clone("enrolled_nodes", replacements), new RegExp(`UNIQUE constraint.*${col}`));
    }
    db.query("INSERT INTO enrolled_nodes(node_id,installation_id,primary_id,credential_id,enrolled_at,created_by_attempt_id) VALUES(?,?,?,?,'0',?)").run(missing, missing, missing, missing, missing);
    clone("node_snapshot_acks", { content_hash: "'second hash'" });
    clone("node_snapshot_acks", { node_id: other });
  } finally { db.exec("ROLLBACK TO keys; RELEASE keys"); }
  for (const state of states["enrollment_attempts.state"]!) {
    db.exec("SAVEPOINT active");
    try {
      // Only one seed is made active; both engines can otherwise coexist.
      db.exec(`UPDATE enrollment_attempts SET state='${state}' WHERE rowid=(SELECT min(rowid) FROM enrollment_attempts)`);
      const action = () => clone("enrollment_attempts", { id: other, state: `'${state}'` });
      if (["committed", "cancelled", "failed"].includes(state)) action();
      else assert.throws(action, /enrollment_attempts_one_active_idx/);
    } finally { db.exec("ROLLBACK TO active; RELEASE active"); }
  }
  db.exec("SAVEPOINT natural");
  try {
    // These references were dropped in 0007 or were never foreign keys.
    for (const [table, col] of [["enrollment_tokens", "consumed_by_attempt_id"], ["enrolled_nodes", "created_by_attempt_id"], ["enrollment_grants", "attempt_id"], ["node_credentials", "node_id"], ["sync_attempts", "node_id"]]) {
      db.exec(`UPDATE ${table} SET ${col}=${other} WHERE rowid=(SELECT min(rowid) FROM ${table})`);
    }
  } finally { db.exec("ROLLBACK TO natural; RELEASE natural"); }
  for (const [table, col, value] of [["enrollment_attempts", "state", "draft"], ["sync_attempts", "status", "queued"]]) {
    db.exec("SAVEPOINT defaults");
    try {
      clone(table!, { id: other }, col);
      assert.equal(query(`SELECT ${col} AS v FROM ${table} WHERE id=${other}`)[0]!.v, value);
    } finally { db.exec("ROLLBACK TO defaults; RELEASE defaults"); }
  }
  const clocks = ["0", "100000000000000000000.000000000", clock, "118446744073709551615.999999999", "2"];
  for (const table of ["enrollment_tokens", "enrollment_grants"]) for (const [i, created] of clocks.entries()) for (const [j, expires] of clocks.entries()) {
    db.exec("SAVEPOINT expiry");
    try {
      const update = () => db.query(`UPDATE ${table} SET created_at=?,expires_at=?`).run(created, expires);
      if (j > i) update(); else assert.throws(update, new RegExp(`${table}_expiry_check`));
    } finally { db.exec("ROLLBACK TO expiry; RELEASE expiry"); }
  }
  assert.deepEqual(snapshot(), before);
  migrateSQLite(db);
  assert.deepEqual(query("SELECT * FROM proxycore_schema_migrations ORDER BY version"), ledger);
} finally { db.close(); }
