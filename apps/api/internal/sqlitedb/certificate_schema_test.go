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

const certificateOpaque = "  opaque 雪🦊\n\"quoted\" non-JSON  "
const certificateClock = "109223372036854775807.000000123"
const certificateMissing = "00000000-0000-0000-0000-000000000099"

type certificateColumn struct {
	name, kind   string
	required, pk int
	def          sql.NullString
}

// Independent effective migration-order contract, not generated from the DDL.
func certificateColumns() map[string][]certificateColumn {
	text := func(name string) certificateColumn { return certificateColumn{name: name, kind: "TEXT", required: 1} }
	optional := func(name string) certificateColumn { c := text(name); c.required = 0; return c }
	id := text("id")
	id.pk = 1
	environment := text("environment")
	environment.def = sql.NullString{String: "'production'", Valid: true}
	status := text("status")
	status.def = sql.NullString{String: "'pending'", Valid: true}
	enabled := certificateColumn{name: "enabled", kind: "INTEGER", required: 1, def: sql.NullString{String: "1", Valid: true}}
	return map[string][]certificateColumn{
		"secrets":                      {id, text("purpose"), text("ciphertext"), text("created_at"), text("updated_at")},
		"certificates":                 {id, text("hostnames"), text("issuer"), text("challenge"), environment, status, optional("expires_at"), optional("renew_after"), optional("key_secret_id"), optional("certificate_pem"), text("created_at"), text("updated_at"), optional("failure_reason")},
		"provider_connections":         {id, text("provider"), text("name"), text("secret_id"), text("scope"), enabled, text("created_at"), text("updated_at")},
		"internal_ca":                  {id, text("certificate_pem"), text("key_secret_id"), text("created_at"), text("updated_at"), optional("enrollment_certificate_pem"), optional("enrollment_key_secret_id")},
		"internal_ca_enrollment_state": {id, text("established_at")},
	}
}

func certificateBun(t *testing.T, path, mode string) {
	t.Helper()
	bun, err := exec.LookPath("bun")
	writeOK(t, err)
	bun, err = filepath.Abs(bun)
	writeOK(t, err)
	_, source, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(source), "../../../..", "packages/db/src/sqlite-foundation/certificate-schema.bun.ts")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", script, path, mode)
	cmd.Dir = filepath.Dir(path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Bun certificate: %v\n%s", err, output)
	}
}

func certificateMetadata(t *testing.T, db *sql.DB) {
	t.Helper()
	for table, expected := range certificateColumns() {
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
			t.Fatalf("%s metadata: %#v want %#v", table, actual, expected)
		}
		var ddl string
		writeOK(t, db.QueryRow("SELECT sql FROM sqlite_schema WHERE name=?", table).Scan(&ddl))
		if !strings.HasSuffix(ddl, " STRICT") {
			t.Fatal("not STRICT:", table)
		}
		for _, c := range expected {
			if strings.HasSuffix(c.name, "_at") || c.name == "renew_after" {
				if !strings.Contains(ddl, c.name+" TEXT COLLATE BINARY") {
					t.Fatal("clock collation:", c.name)
				}
			}
		}
		var secondary int
		writeOK(t, db.QueryRow("SELECT count(*) FROM pragma_index_list(?) WHERE origin!='pk'", table).Scan(&secondary))
		if secondary != 0 {
			t.Fatal("secondary indexes:", table)
		}
		var binary int
		writeOK(t, db.QueryRow("SELECT count(*) FROM pragma_index_list(?) AS i JOIN pragma_index_xinfo(i.name) AS x WHERE i.origin='pk' AND x.key=1 AND x.name='id' AND x.coll='BINARY'", table).Scan(&binary))
		if binary != 1 {
			t.Fatal("non-BINARY primary key:", table)
		}
		keys, err := db.Query("SELECT \"table\",\"from\",\"to\",on_delete,on_update FROM pragma_foreign_key_list(?) ORDER BY \"from\"", table)
		writeOK(t, err)
		var actualKeys []string
		for keys.Next() {
			var target, from, to, deletion, update string
			writeOK(t, keys.Scan(&target, &from, &to, &deletion, &update))
			actualKeys = append(actualKeys, target+":"+from+":"+to+":"+deletion+":"+update)
		}
		writeOK(t, keys.Err())
		writeOK(t, keys.Close())
		var expectedKeys []string
		for _, c := range expected {
			if strings.HasSuffix(c.name, "secret_id") {
				expectedKeys = append(expectedKeys, "secrets:"+c.name+":id:NO ACTION:NO ACTION")
			}
		}
		// internal_ca's alphabetic FK order differs from column order.
		if table == "internal_ca" {
			expectedKeys[0], expectedKeys[1] = expectedKeys[1], expectedKeys[0]
		}
		if !reflect.DeepEqual(actualKeys, expectedKeys) {
			t.Fatalf("%s FKs %v want %v", table, actualKeys, expectedKeys)
		}
	}
}

func certificateInsert(t *testing.T, db *sql.DB) {
	t.Helper()
	calls := 0
	writer := NewWriter(db, func() FiniteTimestamp { calls++; return FiniteTimestamp{Seconds: -1, Nanos: 123} })
	writeOK(t, writer.Write(context.Background(), func(v WriteValue) error {
		id, err := v.NewUUID()
		if err != nil {
			return err
		}
		for _, row := range []struct {
			sql  string
			args []any
		}{
			{"INSERT INTO secrets VALUES(?,'go',?,?,?)", []any{id, certificateOpaque, v.Timestamp(), v.Timestamp()}},
			{"INSERT INTO certificates(id,hostnames,issuer,challenge,key_secret_id,created_at,updated_at) VALUES(?,?,'uploaded','none',?,?,?)", []any{id, authJSON, id, v.Timestamp(), v.Timestamp()}},
			{"INSERT INTO provider_connections(id,provider,name,secret_id,scope,created_at,updated_at) VALUES(?,'go','雪',?,'any',?,?)", []any{id, id, v.Timestamp(), v.Timestamp()}},
			{"INSERT INTO internal_ca(id,certificate_pem,key_secret_id,created_at,updated_at,enrollment_key_secret_id) VALUES('go','not parsed',?,?,?,?)", []any{id, v.Timestamp(), v.Timestamp(), id}},
			{"INSERT INTO internal_ca_enrollment_state VALUES('go',?)", []any{v.Timestamp()}},
		} {
			if _, err := v.Conn().ExecContext(v.Context(), row.sql, row.args...); err != nil {
				return err
			}
		}
		return nil
	}))
	if calls != 1 {
		t.Fatalf("writer clock calls=%d", calls)
	}
}

func certificateSnapshot(t *testing.T, db *sql.DB, tables []string) []string {
	t.Helper()
	var result []string
	for _, table := range tables {
		rows, err := db.Query("SELECT * FROM " + table + " ORDER BY 1")
		writeOK(t, err)
		columns, err := rows.Columns()
		writeOK(t, err)
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			writeOK(t, rows.Scan(pointers...))
			raw, err := json.Marshal(values)
			writeOK(t, err)
			result = append(result, table+":"+string(raw))
		}
		writeOK(t, rows.Err())
		writeOK(t, rows.Close())
	}
	return result
}

func certificateConstraints(t *testing.T, db *sql.DB) {
	t.Helper()
	reject := func(statement string) {
		t.Helper()
		if _, err := db.Exec(statement); err == nil {
			t.Fatalf("accepted: %s", statement)
		}
	}
	for table, columns := range certificateColumns() {
		var n int
		writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
		if n == 0 {
			t.Fatal("vacuous probes:", table)
		}
		for _, c := range columns {
			var bad []string
			if c.required == 1 {
				bad = append(bad, "NULL")
			}
			if c.kind == "TEXT" {
				bad = append(bad, "coalesce("+c.name+",'x')||char(0)||'tail'", "CAST('x' AS BLOB)")
			}
			if c.name == "enabled" {
				bad = append(bad, "-1", "2", "1.5")
			}
			if c.name == "id" && table != "internal_ca" && table != "internal_ca_enrollment_state" || strings.HasSuffix(c.name, "secret_id") {
				bad = append(bad, "'bad'", "'AAAAAAAA-0000-0000-0000-000000000000'", "'00000000000000000000000000000000000000'")
			}
			if strings.HasSuffix(c.name, "_at") || c.name == "renew_after" {
				bad = append(bad, "'bad'", "'118446744073709551616.000000000'", "'100000000000000000000.00000000x'", "'1'", "'3'")
			}
			for _, value := range bad {
				reject("UPDATE " + table + " SET " + c.name + "=" + value)
			}
		}
		for _, omitted := range []string{"id", "created_at", "updated_at", "established_at"} {
			var names []string
			found := false
			for _, c := range columns {
				if c.name == omitted {
					found = true
				} else {
					names = append(names, c.name)
				}
			}
			if found {
				joined := strings.Join(names, ",")
				reject("INSERT INTO " + table + "(" + joined + ") SELECT " + joined + " FROM " + table + " LIMIT 1")
			}
		}
		reject("INSERT INTO " + table + " SELECT * FROM " + table + " LIMIT 1")
	}
	for _, assignment := range []string{"issuer='other'", "challenge='other'", "status='other'", "hostnames='{'", "hostnames=NULL"} {
		reject("UPDATE certificates SET " + assignment)
	}
	tables := []string{"secrets", "certificates", "provider_connections", "internal_ca", "internal_ca_enrollment_state"}
	before := certificateSnapshot(t, db, tables)
	_, err := db.Exec("BEGIN")
	writeOK(t, err)
	defer db.Exec("ROLLBACK")
	for column, values := range map[string][]string{
		"issuer": {"self-signed", "uploaded", "letsencrypt"}, "challenge": {"none", "http-01", "dns-01"}, "status": {"pending", "issued", "active", "expired", "failed"},
		"hostnames": {"null", "true", "123", "\"scalar\"", "922337203685477580812345", authJSON}, "environment": {"custom 雪", "", "staging"},
	} {
		for _, value := range values {
			_, err := db.Exec("UPDATE certificates SET "+column+"=?", value)
			writeOK(t, err)
			var raw, kind string
			writeOK(t, db.QueryRow("SELECT "+column+",typeof("+column+") FROM certificates LIMIT 1").Scan(&raw, &kind))
			if raw != value || kind != "text" {
				t.Fatal("raw TEXT changed")
			}
		}
	}
	for table, columns := range certificateColumns() {
		for _, c := range columns {
			if strings.HasSuffix(c.name, "_at") || c.name == "renew_after" {
				for _, clock := range []string{"0", "2", "100000000000000000000.000000000", "118446744073709551615.999999999", certificateClock} {
					_, err := db.Exec("UPDATE "+table+" SET "+c.name+"=?", clock)
					writeOK(t, err)
				}
			}
			if c.required == 0 {
				_, err := db.Exec("UPDATE " + table + " SET " + c.name + "=NULL")
				writeOK(t, err)
			}
		}
	}
	for _, pair := range [][2]string{{"certificates", "key_secret_id"}, {"provider_connections", "secret_id"}, {"internal_ca", "key_secret_id"}, {"internal_ca", "enrollment_key_secret_id"}} {
		reject("UPDATE " + pair[0] + " SET " + pair[1] + "='" + certificateMissing + "'")
		_, err := db.Exec("UPDATE " + pair[0] + " SET " + pair[1] + "=(SELECT id FROM secrets LIMIT 1)")
		writeOK(t, err)
		reject("DELETE FROM secrets")
		reject("UPDATE secrets SET id='" + certificateMissing + "'")
		_, err = db.Exec("DELETE FROM " + pair[0])
		writeOK(t, err)
		// Recreate rows before the next FK probe; each edge must independently block.
		_, err = db.Exec("ROLLBACK")
		writeOK(t, err)
		_, err = db.Exec("BEGIN")
		writeOK(t, err)
		for _, child := range []string{"certificates", "provider_connections", "internal_ca"} {
			if child != pair[0] {
				_, err = db.Exec("DELETE FROM " + child)
				writeOK(t, err)
			}
		}
		if pair[0] == "internal_ca" {
			other := "key_secret_id"
			if pair[1] == other {
				other = "enrollment_key_secret_id"
				_, err = db.Exec("UPDATE internal_ca SET " + other + "=NULL")
				writeOK(t, err)
			} else {
				_, err = db.Exec("INSERT INTO secrets VALUES('" + certificateMissing + "','fixture','opaque','0','2'); UPDATE internal_ca SET key_secret_id='" + certificateMissing + "'")
				writeOK(t, err)
			}
		}
		reject("DELETE FROM secrets WHERE purpose='go'")
		reject("UPDATE secrets SET id='00000000-0000-0000-0000-000000000098' WHERE purpose='go'")
		_, err = db.Exec("DELETE FROM " + pair[0] + "; DELETE FROM secrets")
		writeOK(t, err)
		_, err = db.Exec("ROLLBACK; BEGIN")
		writeOK(t, err)
	}
	_, err = db.Exec("INSERT INTO internal_ca SELECT 'free-form 雪',certificate_pem,key_secret_id,created_at,updated_at,NULL,NULL FROM internal_ca LIMIT 1; INSERT INTO internal_ca_enrollment_state VALUES('another 雪','0')")
	writeOK(t, err)
	_, err = db.Exec("ROLLBACK")
	writeOK(t, err)
	if !reflect.DeepEqual(before, certificateSnapshot(t, db, tables)) {
		t.Fatal("rollback changed row bytes")
	}
}

func TestCertificateSharedMigration(t *testing.T) {
	for _, first := range []string{"go", "bun"} {
		t.Run(first, func(t *testing.T) {
			db, path := migrationDB(t)
			if first == "bun" {
				certificateBun(t, path, "insert")
			}
			writeOK(t, Migrate(context.Background(), db))
			certificateMetadata(t, db)
			if first == "go" {
				for _, table := range []string{"secrets", "certificates", "provider_connections", "internal_ca", "internal_ca_enrollment_state", "config_revisions", "apply_jobs", "health_observations", "operational_artifacts", "installation_identity", "cluster_keys", "node_state", "applied_snapshots", "standalone_archives"} {
					var n int
					writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
					if n != 0 {
						t.Fatal("fresh rows:", table)
					}
				}
			}
			certificateInsert(t, db)
			certificateConstraints(t, db)
			writeOK(t, db.Close())
			mode := "verify"
			if first == "go" {
				mode = "insert"
			}
			certificateBun(t, path, mode)
			db, err := Open(path)
			writeOK(t, err)
			defer db.Close()
			writeOK(t, Migrate(context.Background(), db))
			certificateMetadata(t, db)
			for table := range certificateColumns() {
				var n int
				writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
				if n != 2 {
					t.Fatalf("%s rows=%d", table, n)
				}
			}
			var opaque, kind string
			writeOK(t, db.QueryRow("SELECT ciphertext,typeof(ciphertext) FROM secrets WHERE purpose='bun'").Scan(&opaque, &kind))
			if opaque != certificateOpaque || kind != "text" {
				t.Fatal("Bun opaque TEXT changed")
			}
			certificateBun(t, path, "verify")
			before := certificateSnapshot(t, db, []string{"secrets", "certificates", "proxycore_schema_migrations"})
			_, err = db.Exec("CREATE INDEX certificate_drift ON certificates(status)")
			writeOK(t, err)
			if err := Migrate(context.Background(), db); err == nil {
				t.Fatal("certificate drift adopted")
			}
			if !reflect.DeepEqual(before, certificateSnapshot(t, db, []string{"secrets", "certificates", "proxycore_schema_migrations"})) {
				t.Fatal("drift rejection changed row bytes")
			}
		})
	}
}

func TestCertificateRealV2Prefix(t *testing.T) {
	raw, err := migrationAssets.ReadFile("migrations/registry.json")
	writeOK(t, err)
	full := migrationFixture{files: map[string]string{}}
	writeOK(t, json.Unmarshal(raw, &full.entries))
	if len(full.entries) < 3 {
		t.Fatalf("certificate target v3 unavailable: entries=%d", len(full.entries))
	}
	// Historical v2→v3 authority: never adopt later production migrations.
	full.entries = full.entries[:3]
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
				prefix.entries = full.entries[:2]
				writeOK(t, runMigrationFixture(t, engine, db, path, prefix))
				authInsert(t, db)
				configurationInsert(t, db)
				priorTables := append(append([]string{}, configurationTables...), "users", "sessions", "audit_events")
				before := certificateSnapshot(t, db, append(priorTables, "proxycore_schema_migrations"))
				attempt := migrationFixture{entries: append([]fixtureMigration{}, full.entries...), files: map[string]string{}}
				for file, body := range full.files {
					attempt.files[file] = body
				}
				switch failure {
				case "checksum":
					attempt.files[attempt.entries[2].File] += "\n"
				case "pending":
					file := attempt.entries[2].File
					attempt.files[file] = "DELETE FROM zones; " + attempt.files[file] + " INVALID SQL;"
					attempt.entries[2].Checksum = fmt.Sprintf("%x", sha256.Sum256([]byte(attempt.files[file])))
				case "drift":
					_, err := db.Exec("DROP INDEX zones_name_idx")
					writeOK(t, err)
				}
				err := runMigrationFixture(t, engine, db, path, attempt)
				if failure != "none" {
					if err == nil {
						t.Fatal("bad upgrade accepted")
					}
					if !reflect.DeepEqual(before, certificateSnapshot(t, db, append(priorTables, "proxycore_schema_migrations"))) {
						t.Fatal("failed upgrade changed data/ledger")
					}
					var n int
					writeOK(t, db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name IN ('secrets','certificates','provider_connections','internal_ca','internal_ca_enrollment_state')").Scan(&n))
					if n != 0 {
						t.Fatal("partial tables survived")
					}
					return
				}
				writeOK(t, err)
				certificateMetadata(t, db)
				after := certificateSnapshot(t, db, priorTables)
				ledger := certificateSnapshot(t, db, []string{"proxycore_schema_migrations"})
				if !reflect.DeepEqual(before[:len(before)-2], after) || !reflect.DeepEqual(before[len(before)-2:], ledger[:2]) {
					t.Fatal("v2 data/checksums/clocks changed")
				}
				for table := range certificateColumns() {
					var n int
					writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
					if n != 0 {
						t.Fatal("upgrade populated:", table)
					}
				}
				other := "go"
				if engine == "go" {
					other = "bun"
				}
				writeOK(t, runMigrationFixture(t, other, db, path, full))
				writeOK(t, runMigrationFixture(t, engine, db, path, full))
				if !reflect.DeepEqual(ledger, certificateSnapshot(t, db, []string{"proxycore_schema_migrations"})) {
					t.Fatal("rerun changed ledger")
				}
				certificateInsert(t, db)
				productTables := []string{"secrets", "certificates", "provider_connections", "internal_ca", "internal_ca_enrollment_state", "proxycore_schema_migrations"}
				productBefore := certificateSnapshot(t, db, productTables)
				pending := "DELETE FROM certificates; UPDATE secrets SET ciphertext='changed'; CREATE TABLE pending(id INTEGER); INVALID SQL;"
				attempt.entries = append(attempt.entries, fixtureMigration{4, "private_failure", "four.sql", fmt.Sprintf("%x", sha256.Sum256([]byte(pending)))})
				attempt.files["four.sql"] = pending
				if err := runMigrationFixture(t, engine, db, path, attempt); err == nil {
					t.Fatal("failing private pending migration accepted")
				}
				if !reflect.DeepEqual(productBefore, certificateSnapshot(t, db, productTables)) {
					t.Fatal("pending failure changed certificate bytes/ledger")
				}
			})
		}
	}
}
