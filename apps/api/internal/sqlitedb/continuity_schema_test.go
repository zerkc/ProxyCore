package sqlitedb

import (
	"bytes"
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

// Independently declared effective PG column order, including the 0008 tail.
func continuityColumns() map[string][]certificateColumn {
	text := func(n string) certificateColumn { return certificateColumn{name: n, kind: "TEXT", required: 1} }
	optional := func(n string) certificateColumn { c := text(n); c.required = 0; return c }
	integer := func(n, def string) certificateColumn {
		c := text(n)
		c.kind = "INTEGER"
		if def != "" {
			c.def = sql.NullString{String: def, Valid: true}
		}
		return c
	}
	literal := func(n, def string) certificateColumn {
		c := text(n)
		c.def = sql.NullString{String: def, Valid: true}
		return c
	}
	id := text("id")
	id.pk = 1
	blob := optional("snapshot_body")
	blob.kind = "BLOB"
	return map[string][]certificateColumn{
		"installation_identity": {id, text("installation_id"), text("node_id"), literal("role", "'standalone-primary'"), integer("leadership_generation", "1"), integer("latest_known_generation", "1"), optional("cluster_key_id"), text("updated_at")},
		"cluster_keys":          {id, text("purpose"), text("wrapped_kek"), integer("wrapping_key_version", ""), text("created_at"), optional("retired_at")},
		"node_state":            {id, optional("enrolled_at"), optional("enrollment_token_hash"), optional("enrollment_primary_id"), optional("last_seen_at"), optional("last_applied_snapshot_id"), optional("enrollment_attempt_id"), optional("primary_url"), optional("primary_installation_id"), optional("primary_tls_spki_sha256"), optional("credential_id"), integer("sync_enabled", "0"), optional("last_attempt_at"), optional("last_success_at"), integer("consecutive_failures", "0"), optional("next_attempt_at"), optional("last_error_code"), text("updated_at")},
		"applied_snapshots":     {id, text("source_primary_id"), integer("leadership_generation", ""), integer("snapshot_version", ""), integer("replication_version", ""), text("content_hash"), optional("revision_id"), literal("status", "'applied'"), optional("apply_job_id"), optional("failure_code"), text("applied_at"), optional("discarded_at"), blob},
		"standalone_archives":   {id, optional("snapshot_content_hash"), optional("revision_id"), text("archive_blob"), text("capture_reason"), text("captured_at"), optional("retention_started_at"), optional("expires_at"), integer("restoration_eligible", "1"), text("created_at")},
	}
}

var continuityTables = []string{"installation_identity", "cluster_keys", "node_state", "applied_snapshots", "standalone_archives"}
var continuityIndexes = map[string][]string{
	"node_state":          {"node_state_credential_idx:credential_id:1", "node_state_enrollment_attempt_idx:enrollment_attempt_id:1"},
	"applied_snapshots":   {"applied_snapshots_content_hash_idx:content_hash:1"},
	"standalone_archives": {"standalone_archives_expiry_idx:expires_at:0"},
}
var continuityEdges = [][3]string{{"applied_snapshots", "apply_job_id", "apply_jobs"}, {"applied_snapshots", "revision_id", "config_revisions"}, {"standalone_archives", "revision_id", "config_revisions"}}

func continuityUUID(table, col string) bool {
	return strings.HasSuffix(col, "_id") || col == "id" && table != "installation_identity" && table != "node_state"
}
func continuityBool(col string) bool { return col == "sync_enabled" || col == "restoration_eligible" }
func continuityInt64(col string) bool {
	return col == "leadership_generation" || col == "latest_known_generation"
}

func continuityMetadata(t *testing.T, db *sql.DB) {
	t.Helper()
	total := 0
	for table, expected := range continuityColumns() {
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
		var ddl string
		writeOK(t, db.QueryRow("SELECT sql FROM sqlite_schema WHERE name=?", table).Scan(&ddl))
		if !strings.HasSuffix(ddl, " STRICT") {
			t.Fatal("not STRICT", table)
		}
		for _, c := range expected {
			if strings.HasSuffix(c.name, "_at") && !strings.Contains(ddl, c.name+" TEXT COLLATE BINARY") {
				t.Fatal("clock collation", table, c.name)
			}
		}
		if table == "applied_snapshots" && !strings.Contains(ddl, "CONSTRAINT applied_snapshots_status_check CHECK (status IN ('pending','applied','rejected','rolled-back','archived'))") {
			t.Fatal("named status membership", ddl)
		}
		keys, err := db.Query("SELECT \"table\",\"from\",\"to\",on_delete,on_update FROM pragma_foreign_key_list(?) ORDER BY \"from\"", table)
		writeOK(t, err)
		var got, want []string
		for keys.Next() {
			var target, from, to, del, upd string
			writeOK(t, keys.Scan(&target, &from, &to, &del, &upd))
			got = append(got, target+":"+from+":"+to+":"+del+":"+upd)
		}
		writeOK(t, keys.Err())
		writeOK(t, keys.Close())
		for _, edge := range continuityEdges {
			if edge[0] == table {
				want = append(want, edge[2]+":"+edge[1]+":id:NO ACTION:NO ACTION")
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("exact FKs", table, got, want)
		}
		indexes, err := db.Query("SELECT name,\"unique\",origin,partial FROM pragma_index_list(?) ORDER BY name", table)
		writeOK(t, err)
		type index struct {
			name, origin    string
			unique, partial int
		}
		var list []index
		for indexes.Next() {
			var i index
			writeOK(t, indexes.Scan(&i.name, &i.unique, &i.origin, &i.partial))
			list = append(list, i)
		}
		writeOK(t, indexes.Err())
		writeOK(t, indexes.Close())
		got = nil
		for _, i := range list {
			x, err := db.Query("SELECT name,coll,desc FROM pragma_index_xinfo(?) WHERE key=1", i.name)
			writeOK(t, err)
			var cols []string
			for x.Next() {
				var name, coll string
				var desc int
				writeOK(t, x.Scan(&name, &coll, &desc))
				if coll != "BINARY" || desc != 0 {
					t.Fatal("index ordering", i.name)
				}
				cols = append(cols, name)
			}
			writeOK(t, x.Err())
			writeOK(t, x.Close())
			if len(cols) != 1 || i.partial != 0 {
				t.Fatal("index shape", i.name)
			}
			if i.origin == "pk" {
				if cols[0] != "id" || i.unique != 1 {
					t.Fatal("PK", table)
				}
				continue
			}
			if i.origin != "c" {
				t.Fatal("unexpected index", i)
			}
			got = append(got, fmt.Sprintf("%s:%s:%d", i.name, cols[0], i.unique))
		}
		if !reflect.DeepEqual(got, continuityIndexes[table]) {
			t.Fatal("indexes", table, got)
		}
	}
	if total != 55 {
		t.Fatal("contract column count", total)
	}
}

func TestContinuityMetadata(t *testing.T) {
	db, _ := migrationDB(t)
	writeOK(t, Migrate(context.Background(), db))
	continuityMetadata(t, db)
	rows, err := db.Query("SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name")
	writeOK(t, err)
	var names []string
	for rows.Next() {
		var name string
		writeOK(t, rows.Scan(&name))
		names = append(names, name)
	}
	writeOK(t, rows.Err())
	writeOK(t, rows.Close())
	want := []string{"applied_snapshots", "apply_jobs", "audit_events", "certificates", "cluster_keys", "config_revisions", "dns_records", "enrolled_nodes", "enrollment_attempts", "enrollment_grants", "enrollment_tokens", "forwarding_rules", "health_observations", "installation_identity", "installation_settings", "internal_ca", "internal_ca_enrollment_state", "node_credentials", "node_snapshot_acks", "node_state", "operational_artifacts", "provider_connections", "proxycore_schema_migrations", "resolver_pools", "secrets", "sessions", "standalone_archives", "stream_routes", "sync_attempts", "users", "zones"}
	if !reflect.DeepEqual(names, want) {
		t.Fatal("exact product inventory", names)
	}
}

func continuityBun(t *testing.T, path, mode string) {
	t.Helper()
	bun, err := exec.LookPath("bun")
	writeOK(t, err)
	bun, err = filepath.Abs(bun)
	writeOK(t, err)
	_, source, _, _ := runtime.Caller(0)
	script := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../..", "packages/db/src/sqlite-foundation/continuity-schema.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", script, path, mode)
	cmd.Dir = filepath.Dir(path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Bun continuity: %v\n%s", err, output)
	}
}

// Each snapshot includes schema inventory (even empty tables), typed row bytes,
// and storage classes. It intentionally makes no file-byte identity claim.
func continuityState(t *testing.T, db *sql.DB, exclude ...string) []string {
	t.Helper()
	rows, err := db.Query("SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY name")
	writeOK(t, err)
	var result, tables []string
	for rows.Next() {
		var kind, name, table string
		var ddl sql.NullString
		writeOK(t, rows.Scan(&kind, &name, &table, &ddl))
		skipped := false
		for _, ignored := range exclude {
			if table == ignored {
				skipped = true
			}
		}
		if skipped {
			continue
		}
		result = append(result, fmt.Sprintf("%s:%s:%s:%v", kind, name, table, ddl))
		if kind == "table" {
			tables = append(tables, name)
		}
	}
	writeOK(t, rows.Err())
	writeOK(t, rows.Close())
	for _, table := range tables {
		result = append(result, certificateSnapshot(t, db, []string{table})...)
		cols, err := db.Query("SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
		writeOK(t, err)
		var expressions []string
		for cols.Next() {
			var col string
			writeOK(t, cols.Scan(&col))
			expressions = append(expressions, "typeof("+col+")")
		}
		writeOK(t, cols.Err())
		writeOK(t, cols.Close())
		r, err := db.Query("SELECT " + strings.Join(expressions, "||':'||") + " FROM " + table + " ORDER BY 1")
		writeOK(t, err)
		for r.Next() {
			var value string
			writeOK(t, r.Scan(&value))
			result = append(result, table+":"+value)
		}
		writeOK(t, r.Err())
		writeOK(t, r.Close())
	}
	return result
}

func continuityFixture(t *testing.T) migrationFixture {
	t.Helper()
	raw, err := migrationAssets.ReadFile("migrations/registry.json")
	writeOK(t, err)
	f := migrationFixture{files: map[string]string{}}
	writeOK(t, json.Unmarshal(raw, &f.entries))
	if len(f.entries) < 5 {
		t.Fatal("v5 authority unavailable", len(f.entries))
	}
	f.entries = f.entries[:5]
	for _, entry := range f.entries {
		body, err := migrationAssets.ReadFile("migrations/" + entry.File)
		writeOK(t, err)
		f.files[entry.File] = string(body)
	}
	return f
}

func continuityInsert(t *testing.T, db *sql.DB) {
	t.Helper()
	calls := 0
	writeOK(t, NewWriter(db, func() FiniteTimestamp { calls++; return FiniteTimestamp{Seconds: -1, Nanos: 123} }).Write(context.Background(), func(v WriteValue) error {
		id, err := v.NewUUID()
		if err != nil {
			return err
		}
		for _, row := range []struct {
			query string
			args  []any
		}{
			{"INSERT INTO installation_identity(id,installation_id,node_id,cluster_key_id,updated_at) VALUES('go',?,?,?,?)", []any{id, certificateMissing, certificateMissing, v.Timestamp()}},
			{"INSERT INTO cluster_keys(id,purpose,wrapped_kek,wrapping_key_version,created_at,retired_at) VALUES(?,'go',?,-1,?,'2')", []any{id, certificateOpaque, v.Timestamp()}},
			{"INSERT INTO node_state(id,enrollment_primary_id,last_applied_snapshot_id,enrollment_attempt_id,primary_installation_id,credential_id,updated_at,enrolled_at) VALUES('go',?,?,?,?,?,?,'0')", []any{certificateMissing, certificateMissing, id, certificateMissing, id, v.Timestamp()}},
			{"INSERT INTO applied_snapshots(id,source_primary_id,leadership_generation,snapshot_version,replication_version,content_hash,applied_at,snapshot_body) VALUES(?,?,-9223372036854775808,0,-1,'go',?,?)", []any{id, certificateMissing, v.Timestamp(), []byte{0, 255, 195, 40}}},
			{"INSERT INTO standalone_archives(id,archive_blob,capture_reason,captured_at,created_at,expires_at) VALUES(?,?,'go',?,?,'2')", []any{id, certificateOpaque, v.Timestamp(), v.Timestamp()}},
		} {
			if _, err := v.Conn().ExecContext(v.Context(), row.query, row.args...); err != nil {
				return err
			}
		}
		for _, sample := range []struct {
			suffix string
			body   any
		}{{"null", nil}, {"empty", []byte{}}, {"nul", []byte{0}}} {
			key, err := v.NewUUID()
			if err != nil {
				return err
			}
			if _, err := v.Conn().ExecContext(v.Context(), "INSERT INTO applied_snapshots(id,source_primary_id,leadership_generation,snapshot_version,replication_version,content_hash,applied_at,snapshot_body) VALUES(?,?,0,0,0,?,?,?)", key, certificateMissing, "go-"+sample.suffix, v.Timestamp(), sample.body); err != nil {
				return err
			}
		}
		return nil
	}))
	if calls != 1 {
		t.Fatal("writer clock calls", calls)
	}
	var role, clock, historical string
	var generation, latest, enabled, failures, eligible int64
	writeOK(t, db.QueryRow("SELECT role,leadership_generation,latest_known_generation,updated_at FROM installation_identity WHERE id='go'").Scan(&role, &generation, &latest, &clock))
	if role != "standalone-primary" || generation != 1 || latest != 1 || clock != certificateClock {
		t.Fatal("identity defaults/clock")
	}
	writeOK(t, db.QueryRow("SELECT sync_enabled,consecutive_failures,enrolled_at,updated_at FROM node_state WHERE id='go'").Scan(&enabled, &failures, &historical, &clock))
	if enabled != 0 || failures != 0 || historical != "0" || clock != certificateClock {
		t.Fatal("state defaults/historical clock")
	}
	writeOK(t, db.QueryRow("SELECT restoration_eligible,expires_at,created_at FROM standalone_archives WHERE capture_reason='go'").Scan(&eligible, &historical, &clock))
	if eligible != 1 || historical != "2" || clock != certificateClock {
		t.Fatal("archive defaults/historical clock")
	}
}

func continuityConstraints(t *testing.T, db *sql.DB) {
	t.Helper()
	reject := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err == nil {
			t.Fatal("accepted", q)
		}
	}
	before := continuityState(t, db)
	writeOK(t, execSQL(db, "BEGIN"))
	defer db.Exec("ROLLBACK")
	for table, cols := range continuityColumns() {
		var n int
		writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
		if n == 0 {
			t.Fatal("vacuous probes", table)
		}
		for _, c := range cols {
			var bad []string
			if c.required == 1 {
				bad = append(bad, "NULL")
			}
			switch c.kind {
			case "TEXT":
				bad = append(bad, "coalesce("+c.name+",'x')||char(0)||'tail'", "CAST('x' AS BLOB)")
			case "INTEGER":
				bad = append(bad, "1.5", "CAST('1' AS BLOB)")
				if continuityBool(c.name) {
					bad = append(bad, "-1", "2")
				} else if continuityInt64(c.name) {
					bad = append(bad, "9223372036854775808", "-9223372036854775809")
				} else {
					bad = append(bad, "2147483648", "-2147483649")
				}
			case "BLOB":
				bad = append(bad, "'text'", "1")
			}
			if continuityUUID(table, c.name) {
				bad = append(bad, "'bad'", "'AAAAAAAA-0000-0000-0000-000000000000'", "'00000000000000000000000000000000000000'")
			}
			if strings.HasSuffix(c.name, "_at") {
				bad = append(bad, "'bad'", "'1'", "'3'", "'118446744073709551616.000000000'", "'100000000000000000000.00000000x'")
			}
			for _, value := range bad {
				reject("UPDATE " + table + " SET " + c.name + "=" + value)
			}
			var values []any
			if strings.HasSuffix(c.name, "_at") {
				values = []any{"0", "2", "100000000000000000000.000000000", "118446744073709551615.999999999", certificateClock}
			}
			if c.kind == "INTEGER" {
				values = []any{int64(0), int64(1)}
				if !continuityBool(c.name) {
					values = append(values, int64(-2147483648), int64(2147483647), int64(-1))
					if continuityInt64(c.name) {
						values = append(values, int64(-9223372036854775808), int64(9223372036854775807), int64(9007199254740993), int64(-9007199254740993))
					}
				}
			}
			if c.kind == "BLOB" {
				values = []any{[]byte{}, []byte{0}, []byte{255, 195, 40, 0}}
			}
			if c.kind == "TEXT" && !continuityUUID(table, c.name) && !strings.HasSuffix(c.name, "_at") && c.name != "role" && c.name != "status" {
				values = []any{"", certificateOpaque}
			}
			if continuityUUID(table, c.name) && c.name != "id" && c.name != "revision_id" && c.name != "apply_job_id" {
				values = []any{certificateMissing, "ffffffff-ffff-ffff-ffff-ffffffffffff"}
			}
			if c.name == "role" {
				values = []any{"standalone-primary", "primary", "primary-with-nodes", "node", "stale-primary"}
				reject("UPDATE " + table + " SET role='other'")
			}
			if c.name == "status" {
				values = []any{"pending", "applied", "rejected", "rolled-back", "archived"}
				_, err := db.Exec("UPDATE " + table + " SET status='other'")
				if err == nil || !strings.Contains(err.Error(), "applied_snapshots_status_check") {
					t.Fatal("named membership", err)
				}
			}
			for _, value := range values {
				writeOK(t, execSQL(db, "UPDATE "+table+" SET "+c.name+"=? WHERE id=(SELECT id FROM "+table+" ORDER BY id LIMIT 1)", value))
				var actual any
				var kind string
				writeOK(t, db.QueryRow("SELECT "+c.name+",typeof("+c.name+") FROM "+table+" ORDER BY id LIMIT 1").Scan(&actual, &kind))
				equal := reflect.DeepEqual(actual, value)
				if blob, ok := value.([]byte); ok {
					equal = bytes.Equal(actual.([]byte), blob)
				}
				if !equal || kind != strings.ToLower(c.kind) {
					t.Fatalf("%s.%s raw=%#v want=%#v kind=%s", table, c.name, actual, value, kind)
				}
			}
			if c.required == 0 {
				writeOK(t, execSQL(db, "UPDATE "+table+" SET "+c.name+"=NULL"))
				var kind string
				writeOK(t, db.QueryRow("SELECT typeof("+c.name+") FROM "+table+" LIMIT 1").Scan(&kind))
				if kind != "null" {
					t.Fatal("nullable storage", table, c.name)
				}
			}
		}
	}
	writeOK(t, execSQL(db, "ROLLBACK"))
	if !reflect.DeepEqual(before, continuityState(t, db)) {
		t.Fatal("domain rollback changed state")
	}
	// Separate unique probes keep other keys and the primary key distinct.
	for _, col := range []string{"credential_id", "enrollment_attempt_id"} {
		other := "credential_id"
		if col == other {
			other = "enrollment_attempt_id"
		}
		reject("INSERT INTO node_state(id," + col + "," + other + ",updated_at) SELECT 'unique-other'," + col + ",NULL,updated_at FROM node_state LIMIT 1")
	}
	reject("INSERT INTO applied_snapshots(id,source_primary_id,leadership_generation,snapshot_version,replication_version,content_hash,applied_at) SELECT '" + certificateMissing + "',source_primary_id,0,0,0,content_hash,applied_at FROM applied_snapshots LIMIT 1")
	writeOK(t, execSQL(db, "BEGIN"))
	for i := 0; i < 4; i++ {
		writeOK(t, execSQL(db, "INSERT INTO node_state(id,updated_at) VALUES(?,'0')", fmt.Sprintf("free-form %d 雪", i)))
		writeOK(t, execSQL(db, "INSERT INTO installation_identity(id,installation_id,node_id,updated_at) VALUES(?, ?, ?, '2')", fmt.Sprintf("free-form %d 雪", i), certificateMissing, certificateMissing))
	}
	writeOK(t, execSQL(db, "INSERT INTO standalone_archives SELECT '"+certificateMissing+"',snapshot_content_hash,NULL,archive_blob,capture_reason,captured_at,retention_started_at,expires_at,restoration_eligible,created_at FROM standalone_archives LIMIT 1"))
	writeOK(t, execSQL(db, "ROLLBACK"))
	for _, edge := range continuityEdges {
		writeOK(t, execSQL(db, "BEGIN"))
		reject("UPDATE " + edge[0] + " SET " + edge[1] + "='" + certificateMissing + "'")
		writeOK(t, execSQL(db, "INSERT INTO config_revisions(id,revision_number,checksum,snapshot,created_at) VALUES('"+certificateMissing+"',99,'edge','null','0')"))
		if edge[2] == "apply_jobs" {
			writeOK(t, execSQL(db, "INSERT INTO apply_jobs(id,revision_id,target,correlation_id,created_at) VALUES('"+certificateMissing+"','"+certificateMissing+"','combined','edge','2')"))
		}
		writeOK(t, execSQL(db, "UPDATE "+edge[0]+" SET "+edge[1]+"='"+certificateMissing+"'"))
		reject("DELETE FROM " + edge[2] + " WHERE id='" + certificateMissing + "'")
		reject("UPDATE " + edge[2] + " SET id='ffffffff-ffff-ffff-ffff-ffffffffffff' WHERE id='" + certificateMissing + "'")
		writeOK(t, execSQL(db, "UPDATE "+edge[0]+" SET "+edge[1]+"=NULL"))
		writeOK(t, execSQL(db, "DELETE FROM "+edge[0]+"; DELETE FROM "+edge[2]+" WHERE id='"+certificateMissing+"'"))
		writeOK(t, execSQL(db, "ROLLBACK"))
	}
	if !reflect.DeepEqual(before, continuityState(t, db)) {
		t.Fatal("edge rollback changed state")
	}
}

func TestContinuitySharedMigration(t *testing.T) {
	for _, first := range []string{"go", "bun"} {
		t.Run(first, func(t *testing.T) {
			db, path := migrationDB(t)
			if first == "bun" {
				continuityBun(t, path, "insert")
			}
			writeOK(t, Migrate(context.Background(), db))
			continuityMetadata(t, db)
			if first == "go" {
				for _, table := range continuityTables {
					var n int
					writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
					if n != 0 {
						t.Fatal("fresh populated", table)
					}
				}
			}
			continuityInsert(t, db)
			continuityConstraints(t, db)
			writeOK(t, db.Close())
			mode := "verify"
			if first == "go" {
				mode = "insert"
			}
			continuityBun(t, path, mode)
			var err error
			db, err = Open(path)
			writeOK(t, err)
			defer db.Close()
			writeOK(t, Migrate(context.Background(), db))
			continuityMetadata(t, db)
			for _, table := range continuityTables {
				var n int
				writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
				want := 2
				if table == "applied_snapshots" {
					want = 8
				}
				if n != want {
					t.Fatal("cross writer rows", table, n)
				}
			}
			var raw []byte
			var kind, opaque string
			var generation int64
			writeOK(t, db.QueryRow("SELECT snapshot_body,typeof(snapshot_body),leadership_generation FROM applied_snapshots WHERE content_hash='bun'").Scan(&raw, &kind, &generation))
			if !reflect.DeepEqual(raw, []byte{0, 255, 195, 40}) || kind != "blob" || generation != 9007199254740993 {
				t.Fatal("Bun raw bytes/int64", raw, kind, generation)
			}
			for _, sample := range []struct {
				suffix, kind string
				bytes        []byte
			}{{"null", "null", nil}, {"empty", "blob", []byte{}}, {"nul", "blob", []byte{0}}} {
				writeOK(t, db.QueryRow("SELECT snapshot_body,typeof(snapshot_body) FROM applied_snapshots WHERE content_hash=?", "bun-"+sample.suffix).Scan(&raw, &kind))
				if !bytes.Equal(raw, sample.bytes) || kind != sample.kind {
					t.Fatal("Bun blob distinction", sample.suffix, raw, kind)
				}
			}
			writeOK(t, db.QueryRow("SELECT wrapped_kek FROM cluster_keys WHERE purpose='bun'").Scan(&opaque))
			if opaque != certificateOpaque {
				t.Fatal("Bun opaque bytes", opaque)
			}
			before := continuityState(t, db)
			continuityBun(t, path, "verify")
			writeOK(t, Migrate(context.Background(), db))
			if !reflect.DeepEqual(before, continuityState(t, db)) {
				t.Fatal("cross rerun changed state")
			}
		})
	}
}

func TestContinuityRealV4Prefix(t *testing.T) {
	for _, engine := range []string{"go", "bun"} {
		for _, failure := range []string{"none", "checksum", "pending", "pre-drift", "post-drift", "ledger", "successor"} {
			t.Run(engine+"/"+failure, func(t *testing.T) {
				full := continuityFixture(t)
				prefix := full
				prefix.entries = full.entries[:4]
				db, path := migrationDB(t)
				writeOK(t, runMigrationFixture(t, engine, db, path, prefix))
				authInsert(t, db)
				configurationInsert(t, db)
				certificateInsert(t, db)
				operationalInsert(t, db)
				// All 18 prior product tables are populated before attempting the upgrade.
				prior := append(append([]string{}, configurationTables...), "users", "sessions", "audit_events", "secrets", "certificates", "provider_connections", "internal_ca", "internal_ca_enrollment_state", "config_revisions", "apply_jobs", "health_observations", "operational_artifacts")
				for _, table := range prior {
					var n int
					writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
					if n == 0 {
						t.Fatal("vacuous prefix", table)
					}
				}
				rows := certificateSnapshot(t, db, prior)
				priorInventory := continuityState(t, db, append(append([]string{}, continuityTables...), "proxycore_schema_migrations")...)
				ledger := certificateSnapshot(t, db, []string{"proxycore_schema_migrations"})
				pending := full.entries[4].File
				switch failure {
				case "checksum":
					full.files[pending] += "\n"
				case "pending":
					full.files[pending] = "UPDATE secrets SET ciphertext='changed'; DELETE FROM health_observations; " + full.files[pending] + " INVALID SQL;"
					full.entries[4].Checksum = fmt.Sprintf("%x", sha256.Sum256([]byte(full.files[pending])))
				case "pre-drift":
					writeOK(t, execSQL(db, "CREATE INDEX continuity_drift ON users(role)"))
				}
				before := continuityState(t, db)
				err := runMigrationFixture(t, engine, db, path, full)
				if failure == "checksum" || failure == "pending" || failure == "pre-drift" {
					if err == nil {
						t.Fatal("invalid upgrade accepted")
					}
					if !reflect.DeepEqual(before, continuityState(t, db)) {
						t.Fatal("failed upgrade changed inventory/bytes/classes/ledger")
					}
					return
				}
				writeOK(t, err)
				continuityMetadata(t, db)
				if !reflect.DeepEqual(priorInventory, continuityState(t, db, append(append([]string{}, continuityTables...), "proxycore_schema_migrations")...)) {
					t.Fatal("upgrade changed prior schema/typed bytes/storage classes")
				}
				next := certificateSnapshot(t, db, []string{"proxycore_schema_migrations"})
				if len(next) != 5 || !reflect.DeepEqual(ledger, next[:4]) || !reflect.DeepEqual(rows, certificateSnapshot(t, db, prior)) {
					t.Fatal("upgrade changed prior rows/hash/clock")
				}
				for _, table := range continuityTables {
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
				other := "go"
				if engine == "go" {
					other = "bun"
				}
				stable := continuityState(t, db)
				for _, runner := range []string{other, engine} {
					writeOK(t, runMigrationFixture(t, runner, db, path, full))
					if !reflect.DeepEqual(stable, continuityState(t, db)) {
						t.Fatal("upgrade cross rerun changed state")
					}
				}
				if failure == "none" {
					return
				}
				if failure == "post-drift" {
					writeOK(t, execSQL(db, "CREATE INDEX continuity_drift ON node_state(last_error_code)"))
				}
				if failure == "ledger" {
					writeOK(t, execSQL(db, "UPDATE proxycore_schema_migrations SET checksum='changed' WHERE version=4"))
				}
				if failure == "successor" {
					continuityInsert(t, db)
					body := "UPDATE standalone_archives SET archive_blob='changed'; DELETE FROM applied_snapshots; UPDATE secrets SET ciphertext='changed'; CREATE TABLE pending(id INTEGER); INVALID SQL;"
					full.entries = append(full.entries, fixtureMigration{6, "private_failure", "six.sql", fmt.Sprintf("%x", sha256.Sum256([]byte(body)))})
					full.files["six.sql"] = body
				}
				before = continuityState(t, db)
				for _, runner := range []string{engine, other} {
					if err := runMigrationFixture(t, runner, db, path, full); err == nil {
						t.Fatal("invalid post-v5 authority accepted", runner)
					}
					if !reflect.DeepEqual(before, continuityState(t, db)) {
						t.Fatal("failure changed inventory/bytes/classes/ledger", runner)
					}
				}
			})
		}
	}
}
