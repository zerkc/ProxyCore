package sqlitedb

import (
	"context"
	"database/sql"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

var configurationTables = []string{"installation_settings", "zones", "dns_records", "resolver_pools", "forwarding_rules", "stream_routes"}

func configurationBun(t *testing.T, path, mode string) {
	t.Helper()
	bun, err := exec.LookPath("bun")
	writeOK(t, err)
	_, source, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(source), "../../../..", "packages/db/src/sqlite-foundation/configuration-schema.bun.ts")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", script, path, mode)
	cmd.Dir = filepath.Dir(path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Bun configuration: %v\n%s", err, output)
	}
}

func configurationInsert(t *testing.T, db *sql.DB) {
	t.Helper()
	calls := 0
	writer := NewWriter(db, func() FiniteTimestamp {
		calls++
		return FiniteTimestamp{Seconds: -1, Nanos: 123}
	})
	writeOK(t, writer.Write(context.Background(), func(v WriteValue) error {
		zone, err := v.NewUUID()
		writeOK(t, err)
		pool, err := v.NewUUID()
		writeOK(t, err)
		for _, row := range []struct {
			sql  string
			args []any
		}{
			{"INSERT INTO installation_settings(id,default_resolver_pool,forwarding_rules,enrollment_hostnames,created_at,updated_at) VALUES('go',NULL,'null',?,?,?)", []any{authJSON, v.Timestamp(), v.Timestamp()}},
			{"INSERT INTO zones(id,name,created_at,updated_at) VALUES(?,'go雪',?,?)", []any{zone, v.Timestamp(), v.Timestamp()}},
			{"INSERT INTO resolver_pools(id,name,endpoints,created_at,updated_at) VALUES(?,'go雪',?,?,?)", []any{pool, authJSON, v.Timestamp(), v.Timestamp()}},
			{"INSERT INTO forwarding_rules(id,suffix,pool_id,created_at,updated_at) VALUES(?,'go雪',?,?,?)", []any{zone, pool, v.Timestamp(), v.Timestamp()}},
			{"INSERT INTO stream_routes(id,protocol,listen_address,listen_port,upstream,created_at,updated_at) VALUES(?,'tcp','go雪',0,?,?,?)", []any{zone, authJSON, v.Timestamp(), v.Timestamp()}},
			{"INSERT INTO dns_records(id,zone_id,name,type,value,created_at,updated_at) VALUES(?,?,'雪','A',?,?,?)", []any{zone, zone, authJSON, v.Timestamp(), v.Timestamp()}},
			{"INSERT INTO dns_records(id,zone_id,name,type,value,created_at,updated_at) VALUES(?,?,'雪','TXT','null',?,?)", []any{pool, zone, v.Timestamp(), v.Timestamp()}},
		} {
			_, err := v.Conn().ExecContext(v.Context(), row.sql, row.args...)
			if err != nil {
				return err
			}
		}
		return nil
	}))
	if calls != 1 {
		t.Fatalf("clock captures=%d", calls)
	}
}

func configurationConstraints(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range configurationTables {
		rows, err := db.Query("SELECT name,type,\"notnull\" FROM pragma_table_info(?)", table)
		writeOK(t, err)
		var probes []string
		for rows.Next() {
			var column, kind string
			var required int
			writeOK(t, rows.Scan(&column, &kind, &required))
			if required == 1 {
				probes = append(probes, column+"=NULL")
			}
			if kind == "TEXT" {
				probes = append(probes, column+"=coalesce("+column+",'null')||char(0)||'suffix'", column+"=CAST(coalesce("+column+",'null') AS BLOB)")
			}
		}
		writeOK(t, rows.Err())
		writeOK(t, rows.Close())
		for _, probe := range probes {
			if _, err := db.Exec("UPDATE " + table + " SET " + probe); err == nil {
				t.Errorf("%s accepted %s", table, probe)
			}
		}
	}
	for table, assignments := range map[string][]string{
		"installation_settings": {"retention_max_age_days=2147483648", "retention_max_size_mb=-2147483649", "enrollment_hostnames='{'"},
		"zones":                 {"id='AAAAAAAA-0000-0000-0000-000000000000'", "enabled=2", "updated_at='bad'", "created_at='118446744073709551616.000000000'"},
		"dns_records":           {"type='NS'", "ttl=2147483648", "proxied=-1", "value='{'", "proxy_settings='{'", "zone_id='00000000-0000-0000-0000-000000000000'"},
		"resolver_pools":        {"endpoints='{'", "is_default=2"},
		"forwarding_rules":      {"pool_id='00000000-0000-0000-0000-000000000000'"},
		"stream_routes":         {"protocol='TCP'", "listen_port=-2147483649", "listen_port=1.5", "upstream='{'"},
	} {
		for _, assignment := range assignments {
			if _, err := db.Exec("UPDATE " + table + " SET " + assignment); err == nil {
				t.Errorf("%s accepted %s", table, assignment)
			}
		}
	}
}

func configurationRollback(t *testing.T, db *sql.DB) {
	t.Helper()
	tx, err := db.Begin()
	writeOK(t, err)
	defer tx.Rollback()
	for _, statement := range []string{
		"UPDATE installation_settings SET retention_max_age_days=-2147483648,retention_max_size_mb=0,current_desired_revision_id='free-form雪'",
		"UPDATE resolver_pools SET is_default=1,endpoints='922337203685477580812345'",
		"UPDATE stream_routes SET listen_port=2147483647,upstream='true'",
		"UPDATE dns_records SET ttl=0,proxy_settings='null'",
		"UPDATE zones SET created_at='0',updated_at='2'",
	} {
		_, err := tx.Exec(statement)
		writeOK(t, err)
	}
	var raw, kind string
	writeOK(t, tx.QueryRow("SELECT endpoints,typeof(endpoints) FROM resolver_pools LIMIT 1").Scan(&raw, &kind))
	if raw != "922337203685477580812345" || kind != "text" {
		t.Fatal("numeric JSON literal changed")
	}
	for _, statement := range []string{
		"UPDATE zones SET id='00000000-0000-0000-0000-000000000000'",
		"UPDATE resolver_pools SET id='00000000-0000-0000-0000-000000000000'",
	} {
		if _, err := tx.Exec(statement); err == nil {
			t.Fatalf("FK update accepted: %s", statement)
		}
	}
	_, err = tx.Exec("DELETE FROM zones; DELETE FROM resolver_pools")
	writeOK(t, err)
	for _, table := range []string{"dns_records", "forwarding_rules"} {
		var n int
		writeOK(t, tx.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
		if n != 0 {
			t.Fatalf("cascade missing: %s", table)
		}
	}
	writeOK(t, tx.Rollback())
	writeOK(t, db.QueryRow("SELECT endpoints,typeof(endpoints) FROM resolver_pools LIMIT 1").Scan(&raw, &kind))
	if raw != authJSON || kind != "text" {
		t.Fatal("rollback changed raw TEXT")
	}
}

func TestConfigurationSharedMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("Bun subprocess integration")
	}
	for _, first := range []string{"go", "bun"} {
		t.Run(first, func(t *testing.T) {
			db, path := migrationDB(t)
			if first == "bun" {
				configurationBun(t, path, "insert")
			}
			writeOK(t, Migrate(context.Background(), db))
			if first == "go" {
				for _, table := range append(append([]string{}, configurationTables...), "users", "sessions", "audit_events", "secrets", "certificates", "provider_connections", "internal_ca", "internal_ca_enrollment_state", "config_revisions", "apply_jobs", "health_observations", "operational_artifacts", "installation_identity", "cluster_keys", "node_state", "applied_snapshots", "standalone_archives") {
					var n int
					writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
					if n != 0 {
						t.Fatalf("fresh %s rows=%d", table, n)
					}
				}
			}
			configurationInsert(t, db)
			configurationConstraints(t, db)
			configurationRollback(t, db)
			writeOK(t, db.Close())
			if first == "go" {
				configurationBun(t, path, "insert")
			} else {
				configurationBun(t, path, "verify")
			}
			db, err := Open(path)
			writeOK(t, err)
			defer db.Close()
			writeOK(t, Migrate(context.Background(), db))
			for _, table := range configurationTables {
				want := 2
				if table == "dns_records" {
					want = 4
				}
				var n int
				writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
				if n != want {
					t.Fatalf("%s rows=%d", table, n)
				}
			}
			var raw string
			writeOK(t, db.QueryRow("SELECT enrollment_hostnames FROM installation_settings WHERE id='bun'").Scan(&raw))
			if raw != authJSON {
				t.Fatal("Bun raw JSON changed")
			}
			_, err = db.Exec("DROP INDEX zones_name_idx")
			writeOK(t, err)
			if err := Migrate(context.Background(), db); err == nil || !strings.Contains(err.Error(), "schema") {
				t.Fatalf("drift accepted: %v", err)
			}
		})
	}
}

func TestConfigurationRealV1Prefix(t *testing.T) {
	for _, engine := range []string{"go", "bun"} {
		t.Run(engine, func(t *testing.T) {
			db, path := migrationDB(t)
			registry, err := migrationAssets.ReadFile("migrations/registry.json")
			writeOK(t, err)
			f := migrationFixture{files: map[string]string{}}
			writeOK(t, json.Unmarshal(registry, &f.entries))
			f.entries = f.entries[:1]
			body, err := migrationAssets.ReadFile("migrations/" + f.entries[0].File)
			writeOK(t, err)
			f.files[f.entries[0].File] = string(body)
			writeOK(t, runMigrationFixture(t, engine, db, path, f))
			authInsert(t, db)
			var ledger string
			writeOK(t, db.QueryRow("SELECT applied_at FROM proxycore_schema_migrations WHERE version=1").Scan(&ledger))
			for i := 0; i < 2; i++ {
				if engine == "go" {
					writeOK(t, Migrate(context.Background(), db))
				} else {
					configurationBun(t, path, "empty")
				}
				authVerify(t, db, 1)
				var after string
				writeOK(t, db.QueryRow("SELECT applied_at FROM proxycore_schema_migrations WHERE version=1").Scan(&after))
				if !reflect.DeepEqual(ledger, after) {
					t.Fatal("v1 ledger changed")
				}
			}
		})
	}
}
