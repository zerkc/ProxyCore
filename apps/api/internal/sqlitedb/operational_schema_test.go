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
	"strings"
	"testing"
	"time"
)

const operationalRaw = " \n{\"n\":922337203685477580812345,\"text\":\"雪🦊\"} \t"
const operationalID = "00000000-0000-0000-0000-000000000001"

// Independent migration-first contract: source attributes follow original clocks.
func operationalColumns() map[string][]certificateColumn {
	text := func(n string) certificateColumn { return certificateColumn{name: n, kind: "TEXT", required: 1} }
	optional := func(n string) certificateColumn { c := text(n); c.required = 0; return c }
	integer := func(n string, required int) certificateColumn {
		return certificateColumn{name: n, kind: "INTEGER", required: required}
	}
	id := text("id")
	id.pk = 1
	source := text("source")
	source.def = sql.NullString{String: "'ordinary'", Valid: true}
	status := text("status")
	status.def = sql.NullString{String: "'queued'", Valid: true}
	tail := []certificateColumn{source, optional("source_primary_id"), optional("source_node_id"), optional("source_revision_id"), optional("snapshot_content_hash"), integer("snapshot_version", 0), integer("replication_version", 0), integer("leadership_generation", 0)}
	return map[string][]certificateColumn{
		"config_revisions":      append([]certificateColumn{id, integer("revision_number", 1), text("checksum"), text("snapshot"), optional("actor_user_id"), text("created_at"), optional("applied_at")}, tail...),
		"apply_jobs":            append([]certificateColumn{id, text("revision_id"), optional("actor_user_id"), text("target"), status, text("correlation_id"), optional("validation_output"), optional("apply_output"), optional("health_output"), optional("error_message"), optional("claimed_at"), optional("started_at"), optional("finished_at"), text("created_at")}, tail...),
		"health_observations":   {id, text("component"), text("status"), optional("details"), text("created_at")},
		"operational_artifacts": {id, text("kind"), text("path"), integer("size_bytes", 1), optional("revision_id"), text("created_at")},
	}
}

func operationalBun(t *testing.T, path, mode string) {
	t.Helper()
	bun, err := exec.LookPath("bun")
	writeOK(t, err)
	bun, err = filepath.Abs(bun)
	writeOK(t, err)
	_, source, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(source), "../../../..", "packages/db/src/sqlite-foundation/operational-schema.bun.ts")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", script, path, mode)
	cmd.Dir = filepath.Dir(path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Bun operational: %v\n%s", err, output)
	}
}

func operationalMetadata(t *testing.T, db *sql.DB) {
	t.Helper()
	for table, expected := range operationalColumns() {
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
		var ddl string
		writeOK(t, db.QueryRow("SELECT sql FROM sqlite_schema WHERE name=?", table).Scan(&ddl))
		if !strings.HasSuffix(ddl, " STRICT") {
			t.Fatal("non-STRICT", table)
		}
		for _, c := range expected {
			if strings.HasSuffix(c.name, "_at") && !strings.Contains(ddl, c.name+" TEXT COLLATE BINARY") {
				t.Fatal("clock collation", c.name)
			}
		}
		if table == "apply_jobs" || table == "config_revisions" {
			if !strings.Contains(ddl, "CONSTRAINT "+table+"_source_check CHECK (source IN ('ordinary','import','sync'))") {
				t.Fatal("source constraint", ddl)
			}
		}
		keys, err := db.Query("SELECT \"table\",\"from\",\"to\",on_delete,on_update FROM pragma_foreign_key_list(?) ORDER BY \"from\"", table)
		writeOK(t, err)
		var got []string
		for keys.Next() {
			var target, from, to, del, upd string
			writeOK(t, keys.Scan(&target, &from, &to, &del, &upd))
			got = append(got, target+":"+from+":"+to+":"+del+":"+upd)
		}
		writeOK(t, keys.Err())
		writeOK(t, keys.Close())
		var want []string
		if table == "config_revisions" || table == "apply_jobs" {
			want = append(want, "users:actor_user_id:id:NO ACTION:NO ACTION")
		}
		if table == "apply_jobs" || table == "operational_artifacts" {
			want = append(want, "config_revisions:revision_id:id:NO ACTION:NO ACTION")
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s FKs=%v want %v", table, got, want)
		}
		indexes, err := db.Query("SELECT name,\"unique\",origin,partial FROM pragma_index_list(?) WHERE origin!='pk' ORDER BY name", table)
		writeOK(t, err)
		var names []string
		for indexes.Next() {
			var name, origin string
			var unique, partial int
			writeOK(t, indexes.Scan(&name, &unique, &origin, &partial))
			if unique != 1 || partial != 0 || origin != "c" {
				t.Fatal("index", name)
			}
			names = append(names, name)
		}
		writeOK(t, indexes.Err())
		writeOK(t, indexes.Close())
		var expectedNames []string
		if table == "config_revisions" {
			expectedNames = []string{"config_revisions_checksum_idx", "config_revisions_number_idx"}
		}
		if !reflect.DeepEqual(names, expectedNames) {
			t.Fatal("indexes", names)
		}
		for _, pair := range [][2]string{{"sqlite_autoindex_" + table + "_1", "id"}} {
			var n, total int
			writeOK(t, db.QueryRow("SELECT count(CASE WHEN name=? AND coll='BINARY' AND desc=0 THEN 1 END),count(*) FROM pragma_index_xinfo(?) WHERE key=1", pair[1], pair[0]).Scan(&n, &total))
			if n != 1 || total != 1 {
				t.Fatal("PK collation", table)
			}
		}
		if table == "config_revisions" {
			for _, pair := range [][2]string{{"config_revisions_number_idx", "revision_number"}, {"config_revisions_checksum_idx", "checksum"}} {
				var n, total int
				writeOK(t, db.QueryRow("SELECT count(CASE WHEN name=? AND coll='BINARY' AND desc=0 THEN 1 END),count(*) FROM pragma_index_xinfo(?) WHERE key=1", pair[1], pair[0]).Scan(&n, &total))
				if n != 1 || total != 1 {
					t.Fatal("index column", pair)
				}
			}
		}
	}
}

func operationalInsert(t *testing.T, db *sql.DB) {
	t.Helper()
	calls := 0
	writeOK(t, NewWriter(db, func() FiniteTimestamp { calls++; return FiniteTimestamp{Seconds: -1, Nanos: 123} }).Write(context.Background(), func(v WriteValue) error {
		id, err := v.NewUUID()
		if err != nil {
			return err
		}
		rows := []struct {
			query string
			args  []any
		}{
			{"INSERT INTO config_revisions(id,revision_number,checksum,snapshot,created_at,leadership_generation) VALUES(?,0,?,?,?,?)", []any{id, id, operationalRaw, v.Timestamp(), int64(-9223372036854775808)}},
			{"INSERT INTO apply_jobs(id,revision_id,target,correlation_id,validation_output,created_at,leadership_generation) VALUES(?,?,'combined','go',?,?,?)", []any{id, id, operationalRaw, v.Timestamp(), int64(9223372036854775807)}},
			{"INSERT INTO health_observations VALUES(?,'go','unrestricted',?,?)", []any{id, operationalRaw, v.Timestamp()}},
			{"INSERT INTO operational_artifacts VALUES(?,'go','arbitrary 雪',-1,?,?)", []any{id, id, v.Timestamp()}},
		}
		for _, r := range rows {
			if _, err := v.Conn().ExecContext(v.Context(), r.query, r.args...); err != nil {
				return err
			}
		}
		return nil
	}))
	if calls != 1 {
		t.Fatal("writer clock calls", calls)
	}
}

func operationalJSON(n string) bool {
	return n == "snapshot" || n == "details" || strings.HasSuffix(n, "_output")
}

func operationalConstraints(t *testing.T, db *sql.DB) {
	t.Helper()
	reject := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err == nil {
			t.Fatal("accepted", q)
		}
	}
	for table, cols := range operationalColumns() {
		var n int
		writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
		if n == 0 {
			t.Fatal("vacuous", table)
		}
		for _, c := range cols {
			bad := []string{}
			if c.required == 1 {
				bad = append(bad, "NULL")
			}
			if c.kind == "TEXT" {
				bad = append(bad, "coalesce("+c.name+",'x')||char(0)||'tail'", "CAST('x' AS BLOB)")
			} else {
				bad = append(bad, "1.5", "CAST('1' AS BLOB)")
				if c.name == "leadership_generation" {
					bad = append(bad, "9223372036854775808", "-9223372036854775809")
				} else {
					bad = append(bad, "2147483648", "-2147483649")
				}
			}
			if c.name == "id" || strings.HasSuffix(c.name, "_id") && c.name != "correlation_id" {
				bad = append(bad, "'bad'", "'AAAAAAAA-0000-0000-0000-000000000000'", "'00000000000000000000000000000000000000'")
			}
			if strings.HasSuffix(c.name, "_at") {
				bad = append(bad, "'bad'", "'1'", "'3'", "'118446744073709551616.000000000'", "'100000000000000000000.00000000x'")
			}
			if operationalJSON(c.name) {
				bad = append(bad, "'{'", "'undefined'")
			}
			for _, value := range bad {
				reject("UPDATE " + table + " SET " + c.name + "=" + value)
			}
			if c.name == "id" || c.name == "created_at" {
				var others []string
				for _, o := range cols {
					if o.name != c.name {
						others = append(others, o.name)
					}
				}
				joined := strings.Join(others, ",")
				reject("INSERT INTO " + table + "(" + joined + ") SELECT " + joined + " FROM " + table + " LIMIT 1")
			}
		}
		reject("INSERT INTO " + table + " SELECT * FROM " + table + " LIMIT 1")
	}
	for _, q := range []string{"UPDATE apply_jobs SET target='other'", "UPDATE apply_jobs SET status='other'"} {
		reject(q)
	}
	for _, table := range []string{"config_revisions", "apply_jobs"} {
		_, err := db.Exec("UPDATE " + table + " SET source='other'")
		if err == nil || !strings.Contains(err.Error(), table+"_source_check") {
			t.Fatal("independent source membership constraint", table, err)
		}
	}
	tables := []string{"config_revisions", "apply_jobs", "health_observations", "operational_artifacts"}
	before := certificateSnapshot(t, db, tables)
	writeOK(t, execSQL(db, "BEGIN"))
	defer db.Exec("ROLLBACK")
	// Keep updates on one row: uniqueness must not obscure integer/domain probes.
	writeOK(t, execSQL(db, "UPDATE config_revisions SET revision_number=42 WHERE id!=(SELECT id FROM config_revisions ORDER BY id LIMIT 1)"))
	for table, cols := range operationalColumns() {
		for _, c := range cols {
			var values []any
			if operationalJSON(c.name) {
				values = []any{operationalRaw, "null", "true", "123", "\"scalar\"", "922337203685477580812345"}
			}
			if strings.HasSuffix(c.name, "_at") {
				values = []any{"0", "2", "100000000000000000000.000000000", "118446744073709551615.999999999", certificateClock}
			}
			if c.kind == "INTEGER" {
				values = []any{int64(-2147483648), int64(2147483647), int64(0), int64(-1)}
				if c.name == "leadership_generation" {
					values = append(values, int64(-9223372036854775808), int64(9223372036854775807), int64(9007199254740993))
				}
			}
			if strings.HasPrefix(c.name, "source_") && c.name != "source" {
				values = []any{certificateMissing}
			}
			if c.name == "snapshot_content_hash" || c.name == "component" || c.name == "kind" || c.name == "path" || table == "health_observations" && c.name == "status" {
				values = []any{"", "arbitrary 雪"}
			}
			for _, value := range values {
				writeOK(t, execSQL(db, "UPDATE "+table+" SET "+c.name+"=? WHERE id=(SELECT id FROM "+table+" ORDER BY id LIMIT 1)", value))
				var actual any
				var kind string
				writeOK(t, db.QueryRow("SELECT "+c.name+",typeof("+c.name+") FROM "+table+" ORDER BY id LIMIT 1").Scan(&actual, &kind))
				if !reflect.DeepEqual(actual, value) || c.kind == "TEXT" && kind != "text" || c.kind == "INTEGER" && kind != "integer" {
					t.Fatalf("%s.%s raw=%v want=%v type=%s", table, c.name, actual, value, kind)
				}
			}
			if c.required == 0 {
				writeOK(t, execSQL(db, "UPDATE "+table+" SET "+c.name+"=NULL"))
				var kind string
				writeOK(t, db.QueryRow("SELECT typeof("+c.name+") FROM "+table+" LIMIT 1").Scan(&kind))
				if kind != "null" {
					t.Fatal("SQL NULL differs from JSON null", table, c.name, kind)
				}
			}
		}
	}
	for table, domains := range map[string]map[string][]string{
		"config_revisions": {"source": {"ordinary", "import", "sync"}},
		"apply_jobs":       {"source": {"ordinary", "import", "sync"}, "target": {"coredns", "nginx", "combined", "certificate"}, "status": {"queued", "validating", "applying", "applied", "failed", "rolled-back"}},
	} {
		for col, values := range domains {
			for _, v := range values {
				writeOK(t, execSQL(db, "UPDATE "+table+" SET "+col+"=?", v))
			}
		}
	}
	writeOK(t, execSQL(db, "ROLLBACK"))
	if !reflect.DeepEqual(before, certificateSnapshot(t, db, tables)) {
		t.Fatal("probe rollback changed bytes")
	}
	// Probe each unique key independently with a new PK and a distinct other key.
	reject("INSERT INTO config_revisions(id,revision_number,checksum,snapshot,created_at) SELECT '" + certificateMissing + "',revision_number,'unique-other',snapshot,created_at FROM config_revisions LIMIT 1")
	reject("INSERT INTO config_revisions(id,revision_number,checksum,snapshot,created_at) SELECT '" + certificateMissing + "',123,checksum,snapshot,created_at FROM config_revisions LIMIT 1")
	for _, edge := range [][3]string{{"config_revisions", "actor_user_id", "users"}, {"apply_jobs", "actor_user_id", "users"}, {"apply_jobs", "revision_id", "config_revisions"}, {"operational_artifacts", "revision_id", "config_revisions"}} {
		writeOK(t, execSQL(db, "BEGIN"))
		reject("UPDATE " + edge[0] + " SET " + edge[1] + "='" + certificateMissing + "'")
		if edge[2] == "users" {
			if edge[0] == "config_revisions" {
				writeOK(t, execSQL(db, "DELETE FROM apply_jobs; DELETE FROM operational_artifacts"))
			}
			writeOK(t, execSQL(db, "INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES('"+operationalID+"','edge','opaque','owner','0','2')"))
			writeOK(t, execSQL(db, "UPDATE "+edge[0]+" SET "+edge[1]+"='"+operationalID+"'"))
			reject("DELETE FROM users WHERE id='" + operationalID + "'")
			reject("UPDATE users SET id='" + certificateMissing + "' WHERE id='" + operationalID + "'")
		} else {
			other := "apply_jobs"
			if edge[0] == other {
				other = "operational_artifacts"
			}
			writeOK(t, execSQL(db, "DELETE FROM "+other))
			reject("DELETE FROM config_revisions")
			reject("UPDATE config_revisions SET id='" + certificateMissing + "'")
		}
		writeOK(t, execSQL(db, "DELETE FROM "+edge[0]))
		writeOK(t, execSQL(db, "DELETE FROM "+edge[2]+" WHERE id IN (SELECT id FROM "+edge[2]+")"))
		writeOK(t, execSQL(db, "ROLLBACK"))
	}
}

func execSQL(db *sql.DB, query string, args ...any) error {
	_, err := db.Exec(query, args...)
	return err
}

func TestOperationalSharedMigration(t *testing.T) {
	for _, first := range []string{"go", "bun"} {
		t.Run(first, func(t *testing.T) {
			db, path := migrationDB(t)
			if first == "bun" {
				operationalBun(t, path, "insert")
			}
			writeOK(t, Migrate(context.Background(), db))
			operationalMetadata(t, db)
			if first == "go" {
				for table := range operationalColumns() {
					var n int
					writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
					if n != 0 {
						t.Fatal("fresh table populated", table)
					}
				}
			}
			operationalInsert(t, db)
			operationalConstraints(t, db)
			writeOK(t, db.Close())
			mode := "verify"
			if first == "go" {
				mode = "insert"
			}
			operationalBun(t, path, mode)
			db, err := Open(path)
			writeOK(t, err)
			defer db.Close()
			writeOK(t, Migrate(context.Background(), db))
			operationalMetadata(t, db)
			for table := range operationalColumns() {
				var n int
				writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
				if n != 2 {
					t.Fatal(table, n)
				}
			}
			var raw string
			var generation int64
			writeOK(t, db.QueryRow("SELECT snapshot,leadership_generation FROM config_revisions WHERE checksum='bun'").Scan(&raw, &generation))
			if raw != operationalRaw || generation != 9007199254740993 {
				t.Fatal("Bun bytes/bigint", raw, generation)
			}
			writeOK(t, db.QueryRow("SELECT leadership_generation FROM apply_jobs WHERE correlation_id='bun'").Scan(&generation))
			if generation != -9223372036854775808 {
				t.Fatal("Bun signed minimum", generation)
			}
			operationalBun(t, path, "verify")
			before := certificateSnapshot(t, db, []string{"config_revisions", "apply_jobs", "proxycore_schema_migrations"})
			writeOK(t, execSQL(db, "CREATE INDEX operational_drift ON apply_jobs(status)"))
			if err := Migrate(context.Background(), db); err == nil {
				t.Fatal("post-v4 drift accepted")
			}
			if !reflect.DeepEqual(before, certificateSnapshot(t, db, []string{"config_revisions", "apply_jobs", "proxycore_schema_migrations"})) {
				t.Fatal("drift changed bytes")
			}
		})
	}
}

func TestOperationalRealV3Prefix(t *testing.T) {
	raw, err := migrationAssets.ReadFile("migrations/registry.json")
	writeOK(t, err)
	full := migrationFixture{files: map[string]string{}}
	writeOK(t, json.Unmarshal(raw, &full.entries))
	if len(full.entries) < 4 {
		t.Fatalf("v4 target unavailable: entries=%d", len(full.entries))
	}
	// Historical v3→v4 authority, including its private failing v5 successor.
	full.entries = full.entries[:4]
	for _, entry := range full.entries {
		body, err := migrationAssets.ReadFile("migrations/" + entry.File)
		writeOK(t, err)
		full.files[entry.File] = string(body)
	}
	for _, engine := range []string{"go", "bun"} {
		for _, failure := range []string{"none", "checksum", "pending", "drift"} {
			t.Run(engine+"/"+failure, func(t *testing.T) {
				db, path := migrationDB(t)
				prefix := full
				prefix.entries = full.entries[:3]
				writeOK(t, runMigrationFixture(t, engine, db, path, prefix))
				authInsert(t, db)
				configurationInsert(t, db)
				certificateInsert(t, db)
				prior := append(append([]string{}, configurationTables...), "users", "sessions", "audit_events", "secrets", "certificates", "provider_connections", "internal_ca", "internal_ca_enrollment_state")
				before := certificateSnapshot(t, db, prior)
				ledger := certificateSnapshot(t, db, []string{"proxycore_schema_migrations"})
				attempt := migrationFixture{entries: append([]fixtureMigration{}, full.entries...), files: map[string]string{}}
				for file, body := range full.files {
					attempt.files[file] = body
				}
				pending := attempt.entries[3].File
				switch failure {
				case "checksum":
					attempt.files[pending] += "\n"
				case "pending":
					attempt.files[pending] = "DELETE FROM zones; UPDATE secrets SET ciphertext='changed'; " + attempt.files[pending] + " INVALID SQL;"
					attempt.entries[3].Checksum = fmt.Sprintf("%x", sha256.Sum256([]byte(attempt.files[pending])))
				case "drift":
					writeOK(t, execSQL(db, "DROP INDEX zones_name_idx"))
				}
				err := runMigrationFixture(t, engine, db, path, attempt)
				if failure != "none" {
					if err == nil {
						t.Fatal("invalid upgrade accepted")
					}
					if !reflect.DeepEqual(before, certificateSnapshot(t, db, prior)) || !reflect.DeepEqual(ledger, certificateSnapshot(t, db, []string{"proxycore_schema_migrations"})) {
						t.Fatal("failure changed prior bytes/ledger")
					}
					var n int
					writeOK(t, db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name IN ('config_revisions','apply_jobs','health_observations','operational_artifacts')").Scan(&n))
					if n != 0 {
						t.Fatal("partial tables", n)
					}
					return
				}
				writeOK(t, err)
				operationalMetadata(t, db)
				next := certificateSnapshot(t, db, []string{"proxycore_schema_migrations"})
				if len(next) != 4 || !reflect.DeepEqual(ledger, next[:3]) || !reflect.DeepEqual(before, certificateSnapshot(t, db, prior)) {
					t.Fatal("upgrade changed prior bytes/ledger")
				}
				for table := range operationalColumns() {
					var n int
					writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
					if n != 0 {
						t.Fatal("upgrade populated", table)
					}
				}
				writeOK(t, db.Close())
				db, err = Open(path)
				writeOK(t, err)
				defer db.Close()
				operationalMetadata(t, db)
				other := "go"
				if engine == "go" {
					other = "bun"
				}
				writeOK(t, runMigrationFixture(t, other, db, path, full))
				writeOK(t, runMigrationFixture(t, engine, db, path, full))
				if !reflect.DeepEqual(next, certificateSnapshot(t, db, []string{"proxycore_schema_migrations"})) {
					t.Fatal("rerun changed ledger")
				}
				operationalInsert(t, db)
				all := append(append([]string{}, prior...), "config_revisions", "apply_jobs", "health_observations", "operational_artifacts", "proxycore_schema_migrations")
				product := certificateSnapshot(t, db, all)
				// Explicit private successor v5: v4 remains the production target.
				pendingSQL := "DELETE FROM apply_jobs; UPDATE config_revisions SET snapshot='null'; DELETE FROM health_observations; CREATE TABLE pending(id INTEGER); INVALID SQL;"
				attempt.entries = append(attempt.entries, fixtureMigration{5, "private_failure", "five.sql", fmt.Sprintf("%x", sha256.Sum256([]byte(pendingSQL)))})
				attempt.files["five.sql"] = pendingSQL
				for _, runner := range []string{engine, other} {
					if err := runMigrationFixture(t, runner, db, path, attempt); err == nil {
						t.Fatal("private successor failure accepted", runner)
					}
					if !reflect.DeepEqual(product, certificateSnapshot(t, db, all)) {
						t.Fatal("pending successor changed product bytes/ledger", runner)
					}
					var n int
					writeOK(t, db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name='pending'").Scan(&n))
					if n != 0 {
						t.Fatal("successor schema survived rollback")
					}
				}
				writeOK(t, execSQL(db, "UPDATE proxycore_schema_migrations SET checksum='changed' WHERE version=3"))
				corrupt := certificateSnapshot(t, db, all)
				for _, runner := range []string{engine, other} {
					if err := runMigrationFixture(t, runner, db, path, full); err == nil {
						t.Fatal("applied-prefix checksum drift accepted", runner)
					}
					if !reflect.DeepEqual(corrupt, certificateSnapshot(t, db, all)) {
						t.Fatal("checksum rejection changed product bytes/ledger", runner)
					}
				}
			})
		}
	}
}
