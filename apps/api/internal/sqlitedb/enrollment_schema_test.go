package sqlitedb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// Independent migration-first column order: 0007 appends the grant digest.
func enrollmentColumns() map[string][]certificateColumn {
	text := func(n string) certificateColumn { return certificateColumn{name: n, kind: "TEXT", required: 1} }
	optional := func(n string) certificateColumn { c := text(n); c.required = 0; return c }
	integer := func(n string, nullable bool) certificateColumn {
		c := text(n)
		c.kind = "INTEGER"
		if nullable {
			c.required = 0
		}
		return c
	}
	literal := func(n, value string) certificateColumn {
		c := text(n)
		c.def = sql.NullString{String: value, Valid: true}
		return c
	}
	pk := func(n string, position int) certificateColumn { c := text(n); c.pk = position; return c }
	return map[string][]certificateColumn{
		"enrollment_attempts": {pk("id", 1), literal("state", "'draft'"), text("primary_url"), optional("expected_primary_id"), optional("verified_primary_id"), optional("verified_primary_node_id"), integer("verified_leadership_generation", true), optional("verified_primary_tls_spki_sha256"), optional("verified_primary_ca_fingerprint"), optional("preview_digest"), text("local_node_ip"), optional("archive_id"), text("ephemeral_private_key_wrapped"), optional("bootstrap_payload"), optional("node_credential_secret_id"), optional("cluster_key_id"), optional("initial_snapshot_hash"), optional("initial_snapshot_revision_id"), optional("initial_apply_job_id"), optional("failure_code"), optional("confirmed_at"), text("created_at"), text("updated_at")},
		"enrollment_tokens":   {pk("id", 1), text("token_selector"), text("token_hash"), text("hash_version"), text("created_by_user_id"), text("created_at"), text("expires_at"), optional("consumed_at"), optional("consumed_by_attempt_id"), optional("revoked_at")},
		"node_credentials":    {pk("id", 1), text("node_id"), text("credential_hash"), text("hash_version"), text("created_at"), optional("last_authenticated_at"), optional("revoked_at")},
		"enrolled_nodes":      {pk("node_id", 1), text("installation_id"), text("primary_id"), optional("display_name"), text("credential_id"), text("enrolled_at"), optional("revoked_at"), text("created_by_attempt_id"), optional("unlinked_at"), optional("last_seen_at"), optional("ingress_ipv4"), optional("ingress_ipv6")},
		"enrollment_grants":   {pk("attempt_id", 1), text("token_id"), text("installation_id"), text("node_id"), text("primary_id"), integer("primary_generation", false), text("node_ephemeral_public_key"), text("sealed_bootstrap_payload"), text("payload_hash"), text("created_at"), text("expires_at"), optional("verified_preview_digest")},
		"node_snapshot_acks":  {pk("node_id", 1), pk("content_hash", 2), integer("snapshot_version", false), integer("replication_version", false), text("revision_id"), integer("leadership_generation", false), text("applied_at"), text("received_at")},
		"sync_attempts":       {pk("id", 1), text("node_id"), text("trigger"), literal("status", "'queued'"), optional("source_primary_id"), integer("leadership_generation", true), integer("snapshot_version", true), integer("replication_version", true), optional("content_hash"), optional("revision_id"), optional("apply_job_id"), optional("result_code"), optional("started_at"), optional("finished_at"), text("created_at"), text("updated_at")},
	}
}

func TestEnrollmentMetadata(t *testing.T) {
	db, _ := migrationDB(t)
	writeOK(t, Migrate(context.Background(), db))
	total := 0
	for table, expected := range enrollmentColumns() {
		total += len(expected)
		rows, err := db.Query("SELECT name,type,\"notnull\",pk,dflt_value FROM pragma_table_info(?) ORDER BY cid", table)
		writeOK(t, err)
		var actual []certificateColumn
		for rows.Next() {
			var c certificateColumn
			writeOK(t, rows.Scan(&c.name, &c.kind, &c.required, &c.pk, &c.def))
			actual = append(actual, c)
		}
		writeOK(t, rows.Err())
		writeOK(t, rows.Close())
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("%s columns=%#v want %#v", table, actual, expected)
		}
	}
	if total != 88 {
		t.Fatal("canonical column count", total)
	}
	enrollmentMetadata(t, db)
}

var enrollmentTables = []string{"enrollment_attempts", "enrollment_tokens", "node_credentials", "enrolled_nodes", "enrollment_grants", "node_snapshot_acks", "sync_attempts"}
var enrollmentEdges = [][4]string{
	{"enrollment_attempts", "archive_id", "standalone_archives", "id"},
	{"enrollment_attempts", "cluster_key_id", "cluster_keys", "id"},
	{"enrollment_attempts", "initial_apply_job_id", "apply_jobs", "id"},
	{"enrollment_attempts", "initial_snapshot_revision_id", "config_revisions", "id"},
	{"enrollment_attempts", "node_credential_secret_id", "secrets", "id"},
	{"enrollment_tokens", "created_by_user_id", "users", "id"},
	{"enrolled_nodes", "credential_id", "node_credentials", "id"},
	{"enrollment_grants", "token_id", "enrollment_tokens", "id"},
	{"node_snapshot_acks", "node_id", "enrolled_nodes", "node_id"},
	{"node_snapshot_acks", "revision_id", "config_revisions", "id"},
	{"sync_attempts", "apply_job_id", "apply_jobs", "id"},
	{"sync_attempts", "revision_id", "config_revisions", "id"},
}
var enrollmentIndexes = map[string][]string{
	"enrollment_attempts": {"enrollment_attempts_one_active_idx:1:c:1:<expression>"},
	"enrollment_tokens":   {"enrollment_tokens_selector_idx:1:c:0:token_selector"},
	"node_credentials":    {"node_credentials_node_idx:1:c:0:node_id"},
	"enrolled_nodes":      {"enrolled_nodes_attempt_unique:1:c:0:created_by_attempt_id", "enrolled_nodes_credential_idx:1:c:0:credential_id", "enrolled_nodes_installation_idx:1:c:0:installation_id"},
	"enrollment_grants":   {"1:u:0:token_id"},
	"sync_attempts":       {"sync_attempts_node_created_idx:0:c:0:node_id,created_at"},
}
var enrollmentStates = map[string][]string{
	"enrollment_attempts.state": {"draft", "verified", "confirmed", "exchanged", "archived", "initial-apply-pending", "committed", "cancelled", "recoverable", "failed"},
	"sync_attempts.status":      {"queued", "running", "current", "applied", "failed", "pending-ack"},
	"sync_attempts.trigger":     {"enrollment", "periodic", "manual", "restart"},
}

func enrollmentMetadata(t *testing.T, db *sql.DB) {
	t.Helper()
	for table, cols := range enrollmentColumns() {
		var ddl string
		writeOK(t, db.QueryRow("SELECT sql FROM sqlite_schema WHERE name=?", table).Scan(&ddl))
		if !strings.HasSuffix(ddl, " STRICT") {
			t.Fatal("not STRICT", table)
		}
		for _, col := range cols {
			// unlinked_at arrives with migration 008; fixtures that stop at the
			// enrollment migration (v6) legitimately lack it.
			if (col.name == "unlinked_at" || col.name == "last_seen_at") && !strings.Contains(ddl, col.name) {
				continue
			}
			if strings.HasSuffix(col.name, "_at") && !strings.Contains(ddl, col.name+" TEXT COLLATE BINARY") {
				t.Fatal("clock collation", table, col.name)
			}
		}
		rows, err := db.Query("SELECT \"table\",\"from\",\"to\",on_delete,on_update FROM pragma_foreign_key_list(?) ORDER BY \"from\"", table)
		writeOK(t, err)
		var actual, expected []string
		for rows.Next() {
			var parent, from, to, del, upd string
			writeOK(t, rows.Scan(&parent, &from, &to, &del, &upd))
			actual = append(actual, from+":"+parent+":"+to+":"+del+":"+upd)
		}
		writeOK(t, rows.Err())
		writeOK(t, rows.Close())
		for _, edge := range enrollmentEdges {
			if edge[0] == table {
				expected = append(expected, edge[1]+":"+edge[2]+":"+edge[3]+":NO ACTION:NO ACTION")
			}
		}
		sort.Strings(expected)
		if !reflect.DeepEqual(actual, expected) {
			t.Fatal("exact FKs", table, actual, expected)
		}
		rows, err = db.Query("SELECT name,\"unique\",origin,partial FROM pragma_index_list(?) ORDER BY name", table)
		writeOK(t, err)
		type index struct {
			name, origin    string
			unique, partial int
		}
		var indexes []index
		for rows.Next() {
			var i index
			writeOK(t, rows.Scan(&i.name, &i.unique, &i.origin, &i.partial))
			indexes = append(indexes, i)
		}
		writeOK(t, rows.Err())
		writeOK(t, rows.Close())
		actual = nil
		for _, i := range indexes {
			r, err := db.Query("SELECT name,coll,desc,cid FROM pragma_index_xinfo(?) WHERE key=1 ORDER BY seqno", i.name)
			writeOK(t, err)
			var names []string
			for r.Next() {
				var name sql.NullString
				var coll string
				var desc, cid int
				writeOK(t, r.Scan(&name, &coll, &desc, &cid))
				if desc != 0 || coll != "BINARY" {
					t.Fatal("index order/collation", i.name)
				}
				if !name.Valid {
					if cid != -2 {
						t.Fatal("expression cid", cid)
					}
					names = append(names, "<expression>")
				} else {
					names = append(names, name.String)
				}
			}
			writeOK(t, r.Err())
			writeOK(t, r.Close())
			if i.origin == "pk" {
				var pk []string
				for _, c := range cols {
					if c.pk != 0 {
						pk = append(pk, c.name)
					}
				}
				if i.unique != 1 || i.partial != 0 || !reflect.DeepEqual(names, pk) {
					t.Fatal("PK shape", table, names, pk)
				}
				continue
			}
			prefix := ""
			if i.origin == "c" {
				prefix = i.name + ":"
			}
			actual = append(actual, fmt.Sprintf("%s%d:%s:%d:%s", prefix, i.unique, i.origin, i.partial, strings.Join(names, ",")))
		}
		sort.Strings(actual)
		if !reflect.DeepEqual(actual, enrollmentIndexes[table]) {
			t.Fatal("indexes", table, actual, enrollmentIndexes[table])
		}
	}
	var partial, sync string
	writeOK(t, db.QueryRow("SELECT sql FROM sqlite_schema WHERE name='enrollment_attempts_one_active_idx'").Scan(&partial))
	writeOK(t, db.QueryRow("SELECT sql FROM sqlite_schema WHERE name='sync_attempts_node_created_idx'").Scan(&sync))
	if partial != "CREATE UNIQUE INDEX enrollment_attempts_one_active_idx ON enrollment_attempts((1)) WHERE state NOT IN ('committed','cancelled','failed')" || sync != "CREATE INDEX sync_attempts_node_created_idx ON sync_attempts(node_id ASC,created_at ASC)" {
		t.Fatal("index predicates/direction", partial, sync)
	}
}

// Shared deterministic identities let the other engine verify every column,
// while deliberately differing natural IDs prove no invented topology checks.
func enrollmentKey(engine string) string {
	if engine == "bun" {
		return "00000000-0000-0000-0000-000000000062"
	}
	return "00000000-0000-0000-0000-000000000061"
}
func enrollmentUUID(col string) bool { return col == "id" || strings.HasSuffix(col, "_id") }
func enrollmentBig(col string) bool  { return strings.Contains(col, "generation") }
func enrollmentValues(table, engine string) []any {
	cols := enrollmentColumns()[table]
	values := make([]any, len(cols))
	for i, c := range cols {
		switch {
		case c.kind == "INTEGER":
			values[i] = int64(-2147483648)
			if enrollmentBig(c.name) {
				values[i] = int64(9007199254740993)
			}
		case enrollmentUUID(c.name):
			values[i] = enrollmentKey(engine)
		case strings.HasSuffix(c.name, "_at"):
			values[i] = certificateClock
		default:
			values[i] = certificateOpaque
		}
		switch c.name {
		case "state":
			values[i] = "committed"
		case "status":
			values[i] = "queued"
		case "trigger":
			values[i] = "manual"
		case "expires_at":
			values[i] = "2"
		case "token_selector", "content_hash":
			values[i] = engine
		case "primary_id", "installation_id", "expected_primary_id", "verified_primary_id", "verified_primary_node_id", "source_primary_id":
			values[i] = certificateMissing
		}
		if table == "enrolled_nodes" && c.name == "installation_id" {
			values[i] = "10000000" + enrollmentKey(engine)[8:]
		}
	}
	return values
}
func enrollmentInsert(t *testing.T, db *sql.DB, engine string) {
	t.Helper()
	key := enrollmentKey(engine)
	writeOK(t, execSQL(db, "INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES(?,?,?,'operator','0','2')", key, engine+"@enrollment.invalid", certificateOpaque))
	writeOK(t, execSQL(db, "INSERT INTO secrets VALUES(?,'enrollment',?,'0','2')", key, certificateOpaque))
	writeOK(t, execSQL(db, "INSERT INTO config_revisions(id,revision_number,checksum,snapshot,created_at) VALUES(?,?,?,'null','0')", key, map[string]int{"go": 61, "bun": 62}[engine], engine))
	writeOK(t, execSQL(db, "INSERT INTO apply_jobs(id,revision_id,target,correlation_id,created_at) VALUES(?,?,'combined',?,'2')", key, key, engine))
	writeOK(t, execSQL(db, "INSERT INTO standalone_archives(id,archive_blob,capture_reason,captured_at,created_at) VALUES(?,?,'enrollment','0','2')", key, certificateOpaque))
	writeOK(t, execSQL(db, "INSERT INTO cluster_keys(id,purpose,wrapped_kek,wrapping_key_version,created_at) VALUES(?,'enrollment',?,-1,'0')", key, certificateOpaque))
	// One transaction clock is reused for all seven product rows.
	calls := 0
	writeOK(t, NewWriter(db, func() FiniteTimestamp { calls++; return FiniteTimestamp{Seconds: -1, Nanos: 123} }).Write(context.Background(), func(v WriteValue) error {
		for _, table := range enrollmentTables {
			var cols, placeholders []string
			values := enrollmentValues(table, engine)
			present := len(values)
			if err := v.Conn().QueryRowContext(v.Context(), "SELECT count(*) FROM pragma_table_info(?)", table).Scan(&present); err != nil {
				return err
			}
			values = values[:present]
			for i, c := range enrollmentColumns()[table][:present] {
				cols = append(cols, `"`+c.name+`"`)
				placeholders = append(placeholders, "?")
				if strings.HasSuffix(c.name, "_at") && c.name != "expires_at" {
					values[i] = v.Timestamp()
				}
			}
			if _, err := v.Conn().ExecContext(v.Context(), "INSERT INTO "+table+"("+strings.Join(cols, ",")+") VALUES("+strings.Join(placeholders, ",")+")", values...); err != nil {
				return err
			}
		}
		return nil
	}))
	if calls != 1 {
		t.Fatal("writer clock calls", calls)
	}
}

// enrollmentPresentColumns counts the table's columns in db. Fixtures that stop
// at the enrollment migration (v6) lack the trailing unlinked_at column that
// migration 008 appends to enrolled_nodes.
func enrollmentPresentColumns(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	writeOK(t, db.QueryRow("SELECT count(*) FROM pragma_table_info(?)", table).Scan(&n))
	return n
}

func enrollmentRead(t *testing.T, db *sql.DB, engine string) {
	t.Helper()
	for _, table := range enrollmentTables {
		rows, err := db.Query("SELECT * FROM "+table+" WHERE "+enrollmentColumns()[table][0].name+"=?", enrollmentKey(engine))
		writeOK(t, err)
		if !rows.Next() {
			t.Fatal("missing cross-engine row", engine, table)
		}
		expected := enrollmentValues(table, engine)[:enrollmentPresentColumns(t, db, table)]
		actual := make([]any, len(expected))
		pointers := make([]any, len(expected))
		for i := range actual {
			pointers[i] = &actual[i]
		}
		writeOK(t, rows.Scan(pointers...))
		if !reflect.DeepEqual(actual, expected) {
			t.Fatal("raw cross-engine values", table, engine, actual, expected)
		}
		if rows.Next() {
			t.Fatal("duplicate row", table)
		}
		writeOK(t, rows.Err())
		writeOK(t, rows.Close())
	}
}
func enrollmentDomains(t *testing.T, db *sql.DB) {
	t.Helper()
	before := continuityState(t, db)
	for table, cols := range enrollmentColumns() {
		for _, c := range cols {
			t.Run(table+"/"+c.name, func(t *testing.T) {
				writeOK(t, execSQL(db, "SAVEPOINT domain"))
				defer func() { writeOK(t, execSQL(db, "ROLLBACK TO domain; RELEASE domain")) }()
				// Maximal interval permits independent timestamp-domain probes.
				if table == "enrollment_tokens" || table == "enrollment_grants" {
					writeOK(t, execSQL(db, "UPDATE "+table+" SET created_at='0',expires_at='2'"))
				}
				col := `"` + c.name + `"`
				var bad []string
				if c.required == 1 {
					bad = append(bad, "NULL")
				}
				if c.kind == "TEXT" {
					bad = append(bad, "CAST('x' AS BLOB)", "coalesce("+col+",'x')||char(0)||'tail'")
				} else {
					bad = append(bad, "1.5", "CAST('1' AS BLOB)")
					if enrollmentBig(c.name) {
						bad = append(bad, "9223372036854775808", "-9223372036854775809")
					} else {
						bad = append(bad, "2147483648", "-2147483649")
					}
				}
				if enrollmentUUID(c.name) {
					bad = append(bad, "'bad'", "'AAAAAAAA-0000-0000-0000-000000000000'", "'00000000000000000000000000000000000000'")
				}
				if strings.HasSuffix(c.name, "_at") {
					bad = append(bad, "'bad'", "'1'", "'3'", "'118446744073709551616.000000000'", "'100000000000000000000.00000000x'")
				}
				for _, value := range bad {
					if _, err := db.Exec("UPDATE " + table + " SET " + col + "=" + value); err == nil {
						t.Fatal("accepted", table, c.name, value)
					}
				}
				var good []any
				if c.kind == "INTEGER" {
					good = []any{int64(-2147483648), int64(2147483647), int64(-1), int64(0)}
					if enrollmentBig(c.name) {
						good = append(good, int64(-9223372036854775808), int64(9223372036854775807), int64(9007199254740993), int64(-9007199254740993))
					}
				} else if strings.HasSuffix(c.name, "_at") {
					good = []any{"0", "2", "100000000000000000000.000000000", "118446744073709551615.999999999", certificateClock}
				} else if !enrollmentUUID(c.name) {
					good = []any{"", certificateOpaque, "null", "{\"n\":922337203685477580812345}"}
				}
				if states := enrollmentStates[table+"."+c.name]; states != nil {
					good = nil
					for _, state := range states {
						good = append(good, state)
					}
					if _, err := db.Exec("UPDATE " + table + " SET " + col + "='other'"); err == nil {
						t.Fatal("enum accepted")
					}
				}
				for _, value := range good {
					if (table == "enrollment_tokens" || table == "enrollment_grants") && (c.name == "created_at" && value == "2" || c.name == "expires_at" && value == "0") {
						continue
					}
					writeOK(t, execSQL(db, "UPDATE "+table+" SET "+col+"=? WHERE rowid=(SELECT min(rowid) FROM "+table+")", value))
					var got any
					var kind string
					writeOK(t, db.QueryRow("SELECT "+col+",typeof("+col+") FROM "+table+" ORDER BY rowid LIMIT 1").Scan(&got, &kind))
					if !reflect.DeepEqual(got, value) || kind != strings.ToLower(c.kind) {
						t.Fatal("storage/value", got, value, kind)
					}
				}
				if c.required == 0 {
					writeOK(t, execSQL(db, "UPDATE "+table+" SET "+col+"=NULL"))
					var kind string
					writeOK(t, db.QueryRow("SELECT typeof("+col+") FROM "+table+" LIMIT 1").Scan(&kind))
					if kind != "null" {
						t.Fatal("SQL NULL storage", kind)
					}
				}
			})
		}
	}
	if !reflect.DeepEqual(before, continuityState(t, db)) {
		t.Fatal("domain probes changed state")
	}
}

func enrollmentBun(t *testing.T, path, mode string) {
	t.Helper()
	bun, err := exec.LookPath("bun")
	writeOK(t, err)
	bun, err = filepath.Abs(bun)
	writeOK(t, err)
	_, source, _, _ := runtime.Caller(0)
	script := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../..", "packages/db/src/sqlite-foundation/enrollment-schema.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", script, path, mode)
	cmd.Dir = filepath.Dir(path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Bun enrollment: %v\n%s", err, output)
	}
}
func TestEnrollmentSharedMigration(t *testing.T) {
	for _, first := range []string{"go", "bun"} {
		t.Run(first, func(t *testing.T) {
			db, path := migrationDB(t)
			if first == "bun" {
				enrollmentBun(t, path, "insert")
			}
			writeOK(t, Migrate(context.Background(), db))
			enrollmentMetadata(t, db)
			enrollmentInsert(t, db, "go")
			enrollmentRead(t, db, "go")
			enrollmentDomains(t, db)
			mode := "verify"
			if first == "go" {
				mode = "insert"
			}
			enrollmentBun(t, path, mode)
			enrollmentRead(t, db, "bun")
			enrollmentRead(t, db, "go")
			before := continuityState(t, db)
			enrollmentBun(t, path, "verify")
			writeOK(t, Migrate(context.Background(), db))
			if !reflect.DeepEqual(before, continuityState(t, db)) {
				t.Fatal("rerun changed schema/data/ledger")
			}
		})
	}
}

// Clone with independently chosen keys so secondary uniqueness probes cannot
// accidentally pass merely because the primary key collided first.
func enrollmentClone(db *sql.DB, table string, overrides map[string]string) error {
	var cols, values []string
	for _, c := range enrollmentColumns()[table] {
		name := `"` + c.name + `"`
		cols = append(cols, name)
		value := name
		if replacement, ok := overrides[c.name]; ok {
			value = replacement
		}
		values = append(values, value)
	}
	return execSQL(db, "INSERT INTO "+table+"("+strings.Join(cols, ",")+") SELECT "+strings.Join(values, ",")+" FROM "+table+" WHERE rowid=(SELECT min(rowid) FROM "+table+")")
}
func TestEnrollmentKeysAndReferences(t *testing.T) {
	db, _ := migrationDB(t)
	writeOK(t, Migrate(context.Background(), db))
	enrollmentInsert(t, db, "go")
	before := continuityState(t, db)
	for _, table := range enrollmentTables {
		t.Run("primary/"+table, func(t *testing.T) {
			if err := enrollmentClone(db, table, nil); err == nil {
				t.Fatal("duplicate PK accepted", table)
			}
		})
	}
	quote := func(s string) string { return "'" + s + "'" }
	other := quote(certificateMissing)
	for table, col := range map[string]string{"enrollment_tokens": "token_selector", "node_credentials": "node_id", "enrollment_grants": "token_id"} {
		if err := enrollmentClone(db, table, map[string]string{enrollmentColumns()[table][0].name: other}); err == nil || !strings.Contains(err.Error(), col) {
			t.Fatal("independent unique", table, col, err)
		}
	}
	writeOK(t, execSQL(db, "SAVEPOINT keys"))
	writeOK(t, execSQL(db, "INSERT INTO node_credentials(id,node_id,credential_hash,hash_version,created_at) VALUES(?,?,'opaque','any','0')", certificateMissing, certificateMissing))
	for _, col := range []string{"installation_id", "credential_id", "created_by_attempt_id"} {
		overrides := map[string]string{"node_id": other, "installation_id": other, "credential_id": other, "created_by_attempt_id": other}
		delete(overrides, col)
		if err := enrollmentClone(db, "enrolled_nodes", overrides); err == nil || !strings.Contains(err.Error(), col) {
			t.Fatal("independent enrolled unique", col, err)
		}
	}
	writeOK(t, execSQL(db, "ROLLBACK TO keys; RELEASE keys"))
	// Both halves of the ack key are required for uniqueness, not either alone.
	writeOK(t, execSQL(db, "SAVEPOINT acks"))
	writeOK(t, execSQL(db, "INSERT INTO node_credentials(id,node_id,credential_hash,hash_version,created_at) VALUES(?,?,'opaque','any','0')", certificateMissing, certificateMissing))
	writeOK(t, execSQL(db, "INSERT INTO enrolled_nodes(node_id,installation_id,primary_id,credential_id,enrolled_at,created_by_attempt_id) VALUES(?,?,?,?,'0',?)", certificateMissing, certificateMissing, certificateMissing, certificateMissing, certificateMissing))
	writeOK(t, enrollmentClone(db, "node_snapshot_acks", map[string]string{"content_hash": "'second hash'"}))
	writeOK(t, enrollmentClone(db, "node_snapshot_acks", map[string]string{"node_id": other}))
	writeOK(t, execSQL(db, "ROLLBACK TO acks; RELEASE acks"))
	for _, state := range enrollmentStates["enrollment_attempts.state"] {
		t.Run("active/"+state, func(t *testing.T) {
			writeOK(t, execSQL(db, "SAVEPOINT active"))
			defer func() { writeOK(t, execSQL(db, "ROLLBACK TO active; RELEASE active")) }()
			writeOK(t, execSQL(db, "UPDATE enrollment_attempts SET state=?", state))
			err := enrollmentClone(db, "enrollment_attempts", map[string]string{"id": other, "state": quote(state)})
			terminal := state == "committed" || state == "cancelled" || state == "failed"
			if terminal {
				writeOK(t, err)
			} else if err == nil || !strings.Contains(err.Error(), "enrollment_attempts_one_active_idx") {
				t.Fatal("active uniqueness", state, err)
			}
		})
	}
	for _, edge := range enrollmentEdges {
		t.Run("reference/"+edge[0]+"/"+edge[1], func(t *testing.T) {
			for _, q := range []string{
				"UPDATE " + edge[0] + " SET " + edge[1] + "=" + other,
				"DELETE FROM " + edge[2] + " WHERE " + edge[3] + "=" + quote(enrollmentKey("go")),
				"UPDATE " + edge[2] + " SET " + edge[3] + "=" + other + " WHERE " + edge[3] + "=" + quote(enrollmentKey("go")),
			} {
				if _, err := db.Exec(q); err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
					t.Fatal("NO ACTION FK", q, err)
				}
			}
		})
	}
	writeOK(t, execSQL(db, "SAVEPOINT natural"))
	for _, q := range []string{
		"UPDATE enrollment_tokens SET consumed_by_attempt_id=" + other,
		"UPDATE enrolled_nodes SET created_by_attempt_id=" + other,
		"UPDATE enrollment_grants SET attempt_id=" + other + ",node_id=" + other,
		"UPDATE node_credentials SET node_id=" + other,
		"UPDATE sync_attempts SET node_id=" + other,
	} {
		writeOK(t, execSQL(db, q))
	}
	writeOK(t, execSQL(db, "ROLLBACK TO natural; RELEASE natural"))
	// Defaults are real defaults, not inferred from the explicitly filled seed.
	for _, probe := range []struct{ table, col, want string }{{"enrollment_attempts", "state", "draft"}, {"sync_attempts", "status", "queued"}} {
		writeOK(t, execSQL(db, "SAVEPOINT defaults"))
		var cols, values []string
		for _, c := range enrollmentColumns()[probe.table] {
			if c.name == probe.col {
				continue
			}
			cols = append(cols, `"`+c.name+`"`)
			value := `"` + c.name + `"`
			if c.name == "id" {
				value = other
			}
			values = append(values, value)
		}
		writeOK(t, execSQL(db, "INSERT INTO "+probe.table+"("+strings.Join(cols, ",")+") SELECT "+strings.Join(values, ",")+" FROM "+probe.table))
		var actual string
		writeOK(t, db.QueryRow("SELECT "+probe.col+" FROM "+probe.table+" WHERE id=?", certificateMissing).Scan(&actual))
		if actual != probe.want {
			t.Fatal("default", probe, actual)
		}
		writeOK(t, execSQL(db, "ROLLBACK TO defaults; RELEASE defaults"))
	}
	if !reflect.DeepEqual(before, continuityState(t, db)) {
		t.Fatal("key probes changed state")
	}
}
func TestEnrollmentExpiryOrdering(t *testing.T) {
	db, _ := migrationDB(t)
	writeOK(t, Migrate(context.Background(), db))
	enrollmentInsert(t, db, "go")
	clocks := []string{"0", "100000000000000000000.000000000", certificateClock, "118446744073709551615.999999999", "2"}
	for _, table := range []string{"enrollment_tokens", "enrollment_grants"} {
		for i, created := range clocks {
			for j, expires := range clocks {
				t.Run(fmt.Sprintf("%s/%d/%d", table, i, j), func(t *testing.T) {
					writeOK(t, execSQL(db, "SAVEPOINT expiry"))
					defer func() { writeOK(t, execSQL(db, "ROLLBACK TO expiry; RELEASE expiry")) }()
					err := execSQL(db, "UPDATE "+table+" SET created_at=?,expires_at=?", created, expires)
					if j > i {
						writeOK(t, err)
					} else if err == nil || !strings.Contains(err.Error(), table+"_expiry_check") {
						t.Fatal("expiry ordering", created, expires, err)
					}
				})
			}
		}
	}
}

func enrollmentFixture(t *testing.T) migrationFixture {
	t.Helper()
	raw, err := migrationAssets.ReadFile("migrations/registry.json")
	writeOK(t, err)
	f := migrationFixture{files: map[string]string{}}
	writeOK(t, json.Unmarshal(raw, &f.entries))
	if len(f.entries) < 6 {
		t.Fatal("v6 authority", len(f.entries))
	}
	// These tests exercise the enrollment migration (v6) as the newest one.
	f.entries = f.entries[:6]
	for _, entry := range f.entries {
		body, err := migrationAssets.ReadFile("migrations/" + entry.File)
		writeOK(t, err)
		f.files[entry.File] = string(body)
	}
	return f
}
func TestEnrollmentRealV5Prefix(t *testing.T) {
	for _, engine := range []string{"go", "bun"} {
		for _, failure := range []string{"none", "pending", "pending-successor", "successor"} {
			t.Run(engine+"/"+failure, func(t *testing.T) {
				full := enrollmentFixture(t)
				prefix := full
				prefix.entries = full.entries[:5]
				db, path := migrationDB(t)
				writeOK(t, runMigrationFixture(t, engine, db, path, prefix))
				authInsert(t, db)
				configurationInsert(t, db)
				certificateInsert(t, db)
				operationalInsert(t, db)
				continuityInsert(t, db)
				prior := append(append([]string{}, configurationTables...), "users", "sessions", "audit_events", "secrets", "certificates", "provider_connections", "internal_ca", "internal_ca_enrollment_state", "config_revisions", "apply_jobs", "health_observations", "operational_artifacts")
				prior = append(prior, continuityTables...)
				if len(prior) != 23 {
					t.Fatal("prior-table coverage", len(prior))
				}
				for _, table := range prior {
					var n int
					writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
					if n == 0 {
						t.Fatal("vacuous prior", table)
					}
				}
				ledger := certificateSnapshot(t, db, []string{"proxycore_schema_migrations"})
				priorState := continuityState(t, db, "proxycore_schema_migrations")
				if failure == "pending" {
					file := full.entries[5].File
					full.files[file] = "UPDATE secrets SET ciphertext='changed'; DELETE FROM health_observations; " + full.files[file] + " INVALID SQL;"
					full.entries[5].Checksum = fmt.Sprintf("%x", sha256.Sum256([]byte(full.files[file])))
				}
				if failure == "pending-successor" {
					body := "UPDATE secrets SET ciphertext='changed'; DELETE FROM applied_snapshots; INSERT INTO node_credentials(id,node_id,credential_hash,hash_version,created_at) VALUES('00000000-0000-0000-0000-000000000099','00000000-0000-0000-0000-000000000099','changed','any','0'); CREATE TABLE pending_enrollment(id INTEGER); INVALID SQL;"
					full.entries = append(full.entries, fixtureMigration{7, "private_failure", "seven.sql", fmt.Sprintf("%x", sha256.Sum256([]byte(body)))})
					full.files["seven.sql"] = body
				}
				before := continuityState(t, db)
				err := runMigrationFixture(t, engine, db, path, full)
				if failure == "pending" || failure == "pending-successor" {
					if err == nil {
						t.Fatal("bad pending accepted")
					}
					if !reflect.DeepEqual(before, continuityState(t, db)) {
						t.Fatal("pending rollback schema/data/ledger")
					}
					return
				}
				writeOK(t, err)
				enrollmentMetadata(t, db)
				excluded := append(append([]string{}, enrollmentTables...), "proxycore_schema_migrations")
				if !reflect.DeepEqual(priorState, continuityState(t, db, excluded...)) {
					t.Fatal("prior schema/raw bytes/storage changed")
				}
				next := certificateSnapshot(t, db, []string{"proxycore_schema_migrations"})
				if len(next) != 6 || !reflect.DeepEqual(ledger, next[:5]) {
					t.Fatal("prior ledger checksum/clock changed")
				}
				other := "bun"
				if engine == "bun" {
					other = "go"
				}
				for _, runner := range []string{other, engine} {
					writeOK(t, runMigrationFixture(t, runner, db, path, full))
				}
				if failure == "none" {
					return
				}
				enrollmentInsert(t, db, "go")
				body := "UPDATE enrollment_attempts SET bootstrap_payload='changed'; DELETE FROM sync_attempts; UPDATE secrets SET ciphertext='changed'; CREATE TABLE pending_enrollment(id INTEGER); INVALID SQL;"
				full.entries = append(full.entries, fixtureMigration{7, "private_failure", "seven.sql", fmt.Sprintf("%x", sha256.Sum256([]byte(body)))})
				full.files["seven.sql"] = body
				before = continuityState(t, db)
				for _, runner := range []string{engine, other} {
					if err := runMigrationFixture(t, runner, db, path, full); err == nil {
						t.Fatal("bad successor accepted", runner)
					}
					if !reflect.DeepEqual(before, continuityState(t, db)) {
						t.Fatal("successor rollback schema/data/ledger", runner)
					}
				}
			})
		}
	}
}
