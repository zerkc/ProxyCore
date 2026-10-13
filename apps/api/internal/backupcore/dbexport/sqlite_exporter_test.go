package dbexport

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

const (
	sqliteTestUser     = "10000000-0000-4000-8000-000000000001"
	sqliteTestSecret   = "10000000-0000-4000-8000-000000000002"
	sqliteTestSecret2  = "10000000-0000-4000-8000-000000000003"
	sqliteTestZone     = "10000000-0000-4000-8000-000000000004"
	sqliteTestRecord   = "10000000-0000-4000-8000-000000000005"
	sqliteTestPool     = "10000000-0000-4000-8000-000000000006"
	sqliteTestRule     = "10000000-0000-4000-8000-000000000007"
	sqliteTestStream   = "10000000-0000-4000-8000-000000000008"
	sqliteTestCert     = "10000000-0000-4000-8000-000000000009"
	sqliteTestProvider = "10000000-0000-4000-8000-00000000000a"
	sqliteTestRevision = "10000000-0000-4000-8000-00000000000b"
	sqliteTestCluster  = "10000000-0000-4000-8000-00000000000c"
	sqliteTestInstall  = "10000000-0000-4000-8000-00000000000d"
	sqliteTestNode     = "10000000-0000-4000-8000-00000000000e"
)

var sqliteTestMasterKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))

func sqliteTestKey(t testing.TB, seconds int64, nanos int32) string {
	t.Helper()
	key, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: seconds, Nanos: nanos})
	if err != nil || key == nil {
		t.Fatalf("encode timestamp: %v", err)
	}
	return *key
}

func openSQLiteExportDB(t testing.TB) *sql.DB {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "export.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	return db
}

func sqliteExec(t testing.TB, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %.60q: %v", query, err)
	}
}

// seedSQLiteExport writes representative dummy rows into all 16 tables.
func seedSQLiteExport(t testing.TB, db *sql.DB) {
	t.Helper()
	ts1 := sqliteTestKey(t, 1700000000, 123456789)
	ts2 := sqliteTestKey(t, 1700000100, 0)
	const negInf, posInf = "0", "2"

	sqliteExec(t, db, `INSERT INTO users (id,username,password_hash,role,active,password_change_required,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		sqliteTestUser, "owner", "hash", "owner", 1, 0, ts1, posInf)
	sqliteExec(t, db, `INSERT INTO secrets (id,purpose,ciphertext,created_at,updated_at) VALUES (?,?,?,?,?)`,
		sqliteTestSecret, "provider-token", "ciphertext-one", ts1, ts2)
	sqliteExec(t, db, `INSERT INTO secrets (id,purpose,ciphertext,created_at,updated_at) VALUES (?,?,?,?,?)`,
		sqliteTestSecret2, "ca-key", "ciphertext-two", ts1, ts2)
	// A stale marker left by a prior import must never leak into the bundle.
	sqliteExec(t, db, `INSERT INTO secrets (id,purpose,ciphertext,created_at,updated_at) VALUES (?,?,?,?,?)`,
		"20000000-0000-4000-8000-000000000000", BackupMasterKeyMarkerPurpose, "stale-marker", ts1, ts1)
	sqliteExec(t, db, `INSERT INTO installation_settings (id,ingress_ipv4,default_resolver_pool,forwarding_rules,enrollment_hostnames,retention_max_age_days,retention_max_size_mb,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		"singleton", "192.0.2.1", `{"b":2,"a":[1,2.50]}`, `[]`, `["node.example.test"]`, 14, 100, ts1, ts2)
	sqliteExec(t, db, `INSERT INTO resolver_pools (id,name,endpoints,is_default,created_at,updated_at) VALUES (?,?,?,?,?,?)`,
		sqliteTestPool, "default", `[{"address":"192.0.2.53","port":53}]`, 1, ts1, ts2)
	sqliteExec(t, db, `INSERT INTO zones (id,name,enabled,created_at,updated_at) VALUES (?,?,?,?,?)`,
		sqliteTestZone, "example.test", 0, negInf, ts2)
	sqliteExec(t, db, `INSERT INTO dns_records (id,zone_id,name,type,value,ttl,enabled,proxied,proxy_settings,comment,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		sqliteTestRecord, sqliteTestZone, "www", "A", `"192.0.2.10"`, 300, 1, 1, `{"tls":true}`, nil, ts1, ts2)
	sqliteExec(t, db, `INSERT INTO forwarding_rules (id,suffix,pool_id,created_at,updated_at) VALUES (?,?,?,?,?)`,
		sqliteTestRule, "corp.test", sqliteTestPool, ts1, ts2)
	sqliteExec(t, db, `INSERT INTO stream_routes (id,enabled,protocol,listen_address,listen_port,upstream,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		sqliteTestStream, 1, "tcp", "0.0.0.0", 5353, `{"ip":"192.0.2.1","port":53}`, ts1, ts2)
	sqliteExec(t, db, `INSERT INTO internal_ca (id,certificate_pem,key_secret_id,created_at,updated_at,enrollment_certificate_pem,enrollment_key_secret_id) VALUES (?,?,?,?,?,?,?)`,
		"ca", "-----BEGIN CERTIFICATE-----\ndummy\n-----END CERTIFICATE-----", sqliteTestSecret2, ts1, ts2, nil, nil)
	sqliteExec(t, db, `INSERT INTO internal_ca_enrollment_state (id,established_at) VALUES (?,?)`, "ca", ts1)
	sqliteExec(t, db, `INSERT INTO certificates (id,hostnames,issuer,challenge,status,expires_at,renew_after,key_secret_id,certificate_pem,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		sqliteTestCert, `["a.example.test","b.example.test"]`, "uploaded", "none", "active", posInf, nil, sqliteTestSecret, "pem", ts1, ts2)
	sqliteExec(t, db, `INSERT INTO provider_connections (id,provider,name,secret_id,scope,enabled,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		sqliteTestProvider, "cloudflare", "main", sqliteTestSecret, "zone", 1, ts1, ts2)
	sqliteExec(t, db, `INSERT INTO config_revisions (id,revision_number,checksum,snapshot,actor_user_id,created_at,applied_at) VALUES (?,?,?,?,?,?,?)`,
		sqliteTestRevision, 7, "sum", `{"zones":[],"n":12345678901234567890}`, sqliteTestUser, ts1, ts2)
	sqliteExec(t, db, `INSERT INTO cluster_keys (id,purpose,wrapped_kek,wrapping_key_version,created_at,retired_at) VALUES (?,?,?,?,?,?)`,
		sqliteTestCluster, "cluster", "wrapped", 3, ts1, nil)
	sqliteExec(t, db, `INSERT INTO installation_identity (id,installation_id,node_id,role,leadership_generation,latest_known_generation,cluster_key_id,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		"identity", sqliteTestInstall, sqliteTestNode, "primary", int64(1)<<60, 2, sqliteTestCluster, ts2)
	sqliteExec(t, db, `INSERT INTO node_state (id,sync_enabled,consecutive_failures,last_seen_at,updated_at) VALUES (?,?,?,?,?)`,
		"node", 1, 4, ts1, ts2)
}

type orderedMemWriter struct {
	files map[string][]byte
	order []string
	hook  func(path string)
}

func (w *orderedMemWriter) Add(path string, _ int64, open func() (io.ReadCloser, error)) error {
	if w.files == nil {
		w.files = make(map[string][]byte)
	}
	if w.hook != nil {
		w.hook(path)
	}
	reader, err := open()
	if err != nil {
		return err
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	w.files[path] = data
	w.order = append(w.order, path)
	return nil
}

func exportSQLite(t testing.TB, db *sql.DB, opts ExporterOptions) (*orderedMemWriter, ExportReport) {
	t.Helper()
	writer := &orderedMemWriter{}
	report, err := NewSQLite(db, opts).Export(context.Background(), writer)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	return writer, report
}

func sqliteFixedNow() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

func decodeRows(t testing.TB, data []byte) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var rows []map[string]any
	if err := decoder.Decode(&rows); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return rows
}

func TestSQLiteExportOrderRespectsForeignKeyGraph(t *testing.T) {
	db := openSQLiteExportDB(t)
	if len(SQLiteExportOrder) != len(DefaultConfigTables) {
		t.Fatalf("order has %d tables, want %d", len(SQLiteExportOrder), len(DefaultConfigTables))
	}
	position := map[string]int{}
	for index, table := range SQLiteExportOrder {
		position[table] = index
	}
	for _, table := range DefaultConfigTables {
		if _, ok := position[table]; !ok {
			t.Fatalf("table %q missing from SQLiteExportOrder", table)
		}
	}
	var edges []tableEdge
	for _, table := range DefaultConfigTables {
		rows, err := db.Query(fmt.Sprintf(`PRAGMA foreign_key_list(%s)`, quoteIdentifier(table)))
		if err != nil {
			t.Fatalf("foreign_key_list(%s): %v", table, err)
		}
		for rows.Next() {
			var id, seq int
			var parent, from string
			var to, onUpdate, onDelete, match sql.NullString
			if err := rows.Scan(&id, &seq, &parent, &from, &to, &onUpdate, &onDelete, &match); err != nil {
				t.Fatalf("scan foreign key: %v", err)
			}
			if _, ok := position[parent]; !ok {
				continue
			}
			if parent == table {
				t.Fatalf("self-referencing foreign key on %s would defeat table ordering", table)
			}
			edges = append(edges, tableEdge{parent: parent, child: table})
			if position[parent] >= position[table] {
				t.Errorf("table %s (%d) must be exported after its parent %s (%d)", table, position[table], parent, position[parent])
			}
		}
		rows.Close()
	}
	if len(edges) == 0 {
		t.Fatal("expected foreign-key edges among the configuration tables")
	}
	derived, err := topologicalOrder(DefaultConfigTables, edges)
	if err != nil {
		t.Fatalf("topologicalOrder: %v", err)
	}
	if !reflect.DeepEqual(derived, SQLiteExportOrder) {
		t.Fatalf("SQLiteExportOrder is stale\n got: %v\nwant: %v", SQLiteExportOrder, derived)
	}
}

func TestSQLiteColumnKindsMatchSchemaConstraints(t *testing.T) {
	db := openSQLiteExportDB(t)
	for _, table := range DefaultConfigTables {
		var ddl string
		if err := db.QueryRow(`SELECT sql FROM sqlite_schema WHERE type='table' AND name=?`, table).Scan(&ddl); err != nil {
			t.Fatalf("schema for %s: %v", table, err)
		}
		columns, err := sqliteTableColumns(context.Background(), db, table)
		if err != nil {
			t.Fatalf("columns for %s: %v", table, err)
		}
		for _, column := range columns {
			name := regexp.QuoteMeta(column.name)
			var want sqliteKind
			switch {
			case regexp.MustCompile(`\b` + name + ` IN \('0','2'\)`).MatchString(ddl):
				want = kindTimestamp
			case regexp.MustCompile(`json_valid\(` + name + `\)`).MatchString(ddl):
				want = kindJSON
			case regexp.MustCompile(`\b` + name + ` IN \(0,1\)`).MatchString(ddl):
				want = kindBool
			case column.declared == "INTEGER":
				want = kindInt
			default:
				want = kindText
			}
			if column.kind != want {
				t.Errorf("%s.%s: kind=%d, schema constraints imply %d", table, column.name, column.kind, want)
			}
		}
	}
}

func TestSQLiteExporterExportsAllTablesInOrder(t *testing.T) {
	db := openSQLiteExportDB(t)
	seedSQLiteExport(t, db)
	writer, report := exportSQLite(t, db, ExporterOptions{Now: sqliteFixedNow, MasterKeyBase64: sqliteTestMasterKey})

	wantPaths := make([]string, 0, len(SQLiteExportOrder))
	for _, table := range SQLiteExportOrder {
		wantPaths = append(wantPaths, "db/"+table+".json")
	}
	if !reflect.DeepEqual(writer.order, wantPaths) {
		t.Fatalf("entry order = %v, want %v", writer.order, wantPaths)
	}
	if len(report.Tables) != len(DefaultConfigTables) {
		t.Fatalf("report has %d tables", len(report.Tables))
	}
	counts := map[string]int{}
	for _, stats := range report.Tables {
		counts[stats.Name] = stats.RowCount
		if stats.Error != nil || stats.Bytes != int64(len(writer.files["db/"+stats.Name+".json"])) {
			t.Fatalf("stats for %s = %+v", stats.Name, stats)
		}
	}
	for table, want := range map[string]int{"secrets": 3, "users": 1, "config_revisions": 1, "node_state": 1} {
		if counts[table] != want {
			t.Errorf("row count %s = %d, want %d", table, counts[table], want)
		}
	}

	// secrets: marker first, stale marker dropped, real rows ordered by id.
	secretRows := decodeRows(t, writer.files["db/secrets.json"])
	if len(secretRows) != 3 {
		t.Fatalf("secrets rows = %d, want marker + 2", len(secretRows))
	}
	marker := secretRows[0]
	if marker["id"] != BackupMasterKeyMarkerID || marker["purpose"] != BackupMasterKeyMarkerPurpose {
		t.Fatalf("marker = %v", marker)
	}
	if marker["created_at"] != "2026-01-02T03:04:05Z" || marker["updated_at"] != "2026-01-02T03:04:05Z" {
		t.Fatalf("marker timestamps = %v / %v", marker["created_at"], marker["updated_at"])
	}
	plaintext, err := secrets.DecryptSecret(marker["ciphertext"].(string), sqliteTestMasterKey)
	if err != nil || plaintext != BackupMasterKeyMarkerPlaintext {
		t.Fatalf("marker decrypt = %q, %v", plaintext, err)
	}
	if secretRows[1]["id"] != sqliteTestSecret || secretRows[2]["id"] != sqliteTestSecret2 {
		t.Fatalf("secret order = %v, %v", secretRows[1]["id"], secretRows[2]["id"])
	}
	if secretRows[1]["created_at"] != "2023-11-14T22:13:20.123456789Z" || secretRows[1]["updated_at"] != "2023-11-14T22:15:00Z" {
		t.Fatalf("finite timestamps = %v / %v", secretRows[1]["created_at"], secretRows[1]["updated_at"])
	}
	if strings.Contains(string(writer.files["db/secrets.json"]), "stale-marker") {
		t.Fatal("stale marker ciphertext leaked into the bundle")
	}

	// users: boolean mapping and positive infinity.
	user := decodeRows(t, writer.files["db/users.json"])[0]
	if user["active"] != true || user["password_change_required"] != false || user["updated_at"] != "infinity" {
		t.Fatalf("user row = %v", user)
	}
	// zones: negative infinity and false boolean.
	zone := decodeRows(t, writer.files["db/zones.json"])[0]
	if zone["created_at"] != "-infinity" || zone["enabled"] != false {
		t.Fatalf("zone row = %v", zone)
	}
	// dns_records: JSON embedded (not a string), NULL preserved.
	record := decodeRows(t, writer.files["db/dns_records.json"])[0]
	if record["value"] != "192.0.2.10" || record["comment"] != nil || record["ttl"] != json.Number("300") {
		t.Fatalf("dns record = %v", record)
	}
	if settings, ok := record["proxy_settings"].(map[string]any); !ok || settings["tls"] != true {
		t.Fatalf("proxy_settings = %#v, want embedded object", record["proxy_settings"])
	}
	if record["proxied"] != true || record["enabled"] != true {
		t.Fatalf("dns booleans = %v %v", record["proxied"], record["enabled"])
	}
	// JSON canonicalization: keys sorted, numbers lossless.
	if !strings.Contains(string(writer.files["db/installation_settings.json"]), `"default_resolver_pool":{"a":[1,2.50],"b":2}`) {
		t.Fatalf("installation_settings = %s", writer.files["db/installation_settings.json"])
	}
	if !strings.Contains(string(writer.files["db/config_revisions.json"]), `"n":12345678901234567890`) {
		t.Fatalf("config_revisions = %s", writer.files["db/config_revisions.json"])
	}
	// Large int64 follows the PG exporter: decimal string above 2^53.
	identity := decodeRows(t, writer.files["db/installation_identity.json"])[0]
	if identity["leadership_generation"] != "1152921504606846976" || identity["latest_known_generation"] != json.Number("2") {
		t.Fatalf("identity = %v", identity)
	}
	// Infinity and NULL timestamps on nullable columns.
	cert := decodeRows(t, writer.files["db/certificates.json"])[0]
	if cert["expires_at"] != "infinity" || cert["renew_after"] != nil {
		t.Fatalf("certificate = %v", cert)
	}
	if node := decodeRows(t, writer.files["db/node_state.json"])[0]; node["sync_enabled"] != true || node["last_success_at"] != nil {
		t.Fatalf("node_state = %v", node)
	}
	// Every bundle table is a JSON array, including empty ones.
	for _, table := range DefaultConfigTables {
		if data := writer.files["db/"+table+".json"]; len(data) < 2 || data[0] != '[' || data[len(data)-1] != ']' {
			t.Fatalf("%s payload is not a JSON array: %q", table, data)
		}
	}
}

func TestSQLiteExporterEmptyTablesAndMarkerOnlySecrets(t *testing.T) {
	db := openSQLiteExportDB(t)
	writer, report := exportSQLite(t, db, ExporterOptions{Now: sqliteFixedNow, MasterKeyBase64: sqliteTestMasterKey})
	if string(writer.files["db/users.json"]) != "[]" {
		t.Fatalf("users = %q", writer.files["db/users.json"])
	}
	if rows := decodeRows(t, writer.files["db/secrets.json"]); len(rows) != 1 {
		t.Fatalf("secrets = %v", rows)
	}
	for _, stats := range report.Tables {
		if stats.Name == "secrets" && stats.RowCount != 1 {
			t.Fatalf("secrets RowCount = %d", stats.RowCount)
		}
	}
}

func TestSQLiteExporterIsDeterministic(t *testing.T) {
	db := openSQLiteExportDB(t)
	seedSQLiteExport(t, db)
	opts := ExporterOptions{Now: sqliteFixedNow, MasterKeyBase64: sqliteTestMasterKey}
	first, _ := exportSQLite(t, db, opts)
	second, _ := exportSQLite(t, db, opts)
	for path, data := range first.files {
		if path == "db/secrets.json" {
			// The marker ciphertext uses a random nonce; compare all other rows.
			a, b := decodeRows(t, data), decodeRows(t, second.files[path])
			a[0]["ciphertext"], b[0]["ciphertext"] = nil, nil
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("secrets differ between exports")
			}
			continue
		}
		if !bytes.Equal(data, second.files[path]) {
			t.Fatalf("%s differs between exports", path)
		}
	}
}

func TestSQLiteExporterUsesOneConsistentSnapshot(t *testing.T) {
	db := openSQLiteExportDB(t)
	seedSQLiteExport(t, db)
	writer := &orderedMemWriter{}
	wrote := false
	writer.hook = func(path string) {
		if wrote {
			return
		}
		wrote = true
		// Commit concurrent changes to later and earlier tables mid-export.
		sqliteExec(t, db, `INSERT INTO zones (id,name,enabled,created_at,updated_at) VALUES (?,?,?,?,?)`,
			"30000000-0000-4000-8000-000000000001", "late.test", 1, sqliteTestKey(t, 1, 0), sqliteTestKey(t, 1, 0))
		sqliteExec(t, db, `UPDATE users SET username='changed'`)
		sqliteExec(t, db, `DELETE FROM node_state`)
	}
	if _, err := NewSQLite(db, ExporterOptions{Now: sqliteFixedNow, MasterKeyBase64: sqliteTestMasterKey}).Export(context.Background(), writer); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if !wrote {
		t.Fatal("hook did not run")
	}
	if rows := decodeRows(t, writer.files["db/zones.json"]); len(rows) != 1 {
		t.Fatalf("zones = %d rows, concurrent insert leaked into the snapshot", len(rows))
	}
	if user := decodeRows(t, writer.files["db/users.json"])[0]; user["username"] != "owner" {
		t.Fatalf("users.username = %v, concurrent update leaked", user["username"])
	}
	if rows := decodeRows(t, writer.files["db/node_state.json"]); len(rows) != 1 {
		t.Fatalf("node_state = %d rows, concurrent delete leaked", len(rows))
	}
	var zones int
	if err := db.QueryRow(`SELECT count(*) FROM zones`).Scan(&zones); err != nil || zones != 2 {
		t.Fatalf("live zones = %d, %v; the concurrent write must still commit", zones, err)
	}
}

func TestSQLiteExporterTablesOptionKeepsFixedOrder(t *testing.T) {
	db := openSQLiteExportDB(t)
	writer, _ := exportSQLite(t, db, ExporterOptions{Tables: []string{"dns_records", "zones"}, Now: sqliteFixedNow})
	if want := []string{"db/zones.json", "db/dns_records.json"}; !reflect.DeepEqual(writer.order, want) {
		t.Fatalf("order = %v, want %v", writer.order, want)
	}
	_, err := NewSQLite(db, ExporterOptions{Tables: []string{"sessions"}}).Export(context.Background(), &orderedMemWriter{})
	if err == nil {
		t.Fatal("unknown table accepted")
	}
}

func TestSQLiteExporterErrorPaths(t *testing.T) {
	t.Run("nil arguments", func(t *testing.T) {
		db := openSQLiteExportDB(t)
		if _, err := (*SQLiteExporter)(nil).Export(context.Background(), &orderedMemWriter{}); err == nil {
			t.Fatal("nil exporter accepted")
		}
		if _, err := NewSQLite(nil, ExporterOptions{}).Export(context.Background(), &orderedMemWriter{}); err == nil {
			t.Fatal("nil db accepted")
		}
		if _, err := NewSQLite(db, ExporterOptions{}).Export(nil, &orderedMemWriter{}); err == nil { //nolint:staticcheck
			t.Fatal("nil context accepted")
		}
		if _, err := NewSQLite(db, ExporterOptions{}).Export(context.Background(), nil); err == nil {
			t.Fatal("nil writer accepted")
		}
	})
	t.Run("closed database", func(t *testing.T) {
		db := openSQLiteExportDB(t)
		db.Close()
		writer := &orderedMemWriter{}
		if _, err := NewSQLite(db, ExporterOptions{MasterKeyBase64: sqliteTestMasterKey}).Export(context.Background(), writer); err == nil {
			t.Fatal("closed database accepted")
		}
		if len(writer.order) != 0 {
			t.Fatalf("wrote entries from a closed database: %v", writer.order)
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		db := openSQLiteExportDB(t)
		seedSQLiteExport(t, db)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		writer := &orderedMemWriter{}
		_, err := NewSQLite(db, ExporterOptions{MasterKeyBase64: sqliteTestMasterKey}).Export(ctx, writer)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("cancel mid export", func(t *testing.T) {
		db := openSQLiteExportDB(t)
		seedSQLiteExport(t, db)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		writer := &orderedMemWriter{}
		writer.hook = func(path string) {
			if path == "db/secrets.json" {
				cancel()
			}
		}
		_, err := NewSQLite(db, ExporterOptions{MasterKeyBase64: sqliteTestMasterKey}).Export(ctx, writer)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("bad master key", func(t *testing.T) {
		db := openSQLiteExportDB(t)
		_, err := NewSQLite(db, ExporterOptions{MasterKeyBase64: "not-a-key"}).Export(context.Background(), &orderedMemWriter{})
		if err == nil {
			t.Fatal("invalid master key accepted")
		}
	})
	t.Run("missing table", func(t *testing.T) {
		db := openSQLiteExportDB(t)
		sqliteExec(t, db, `DROP TABLE node_state`)
		report, err := NewSQLite(db, ExporterOptions{MasterKeyBase64: sqliteTestMasterKey}).Export(context.Background(), &orderedMemWriter{})
		var missing *MissingTablesError
		if !errors.As(err, &missing) || len(missing.Tables) != 1 || missing.Tables[0] != "node_state" {
			t.Fatalf("err = %v, want MissingTablesError(node_state)", err)
		}
		if len(report.Tables) != len(DefaultConfigTables) {
			t.Fatalf("report tables = %d", len(report.Tables))
		}
	})
	t.Run("writer failure", func(t *testing.T) {
		db := openSQLiteExportDB(t)
		boom := errors.New("boom")
		_, err := NewSQLite(db, ExporterOptions{MasterKeyBase64: sqliteTestMasterKey}).Export(context.Background(), failingZipWriter{boom})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	})
}

type failingZipWriter struct{ err error }

func (w failingZipWriter) Add(string, int64, func() (io.ReadCloser, error)) error { return w.err }

func TestSQLiteValueEncoding(t *testing.T) {
	col := func(kind sqliteKind) sqliteColumn { return sqliteColumn{name: "c", declared: "TEXT", kind: kind} }
	tests := []struct {
		name  string
		kind  sqliteKind
		value any
		want  any
	}{
		{"null", kindText, nil, nil},
		{"text", kindText, "abc", "abc"},
		{"text bytes", kindText, []byte("abc"), "abc"},
		{"small int", kindInt, int64(-5), float64(-5)},
		{"safe boundary", kindInt, int64(1 << 53), float64(1 << 53)},
		{"above boundary", kindInt, int64(1<<53 + 1), "9007199254740993"},
		{"bool true", kindBool, int64(1), true},
		{"bool false", kindBool, int64(0), false},
		{"neg infinity", kindTimestamp, "0", "-infinity"},
		{"pos infinity", kindTimestamp, "2", "infinity"},
		{"finite", kindTimestamp, sqliteTestKey(t, 0, 5), "1970-01-01T00:00:00.000000005Z"},
		{"pre epoch", kindTimestamp, sqliteTestKey(t, -1, 500000000), "1969-12-31T23:59:59.5Z"},
		{"blob", kindBlob, []byte{0xfb, 0xff}, base64.RawURLEncoding.EncodeToString([]byte{0xfb, 0xff})},
		{"empty blob", kindBlob, []byte{}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := encodeSQLiteValue("t", col(test.kind), test.value)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %#v, want %#v", got, test.want)
			}
		})
	}
	json1, err := encodeSQLiteValue("t", col(kindJSON), `{"b":1,"a":2}`)
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	if encoded, _ := json.Marshal(json1); string(encoded) != `{"a":2,"b":1}` {
		t.Fatalf("json = %s", encoded)
	}

	for name, test := range map[string]struct {
		kind  sqliteKind
		value any
	}{
		"bool out of range": {kindBool, int64(2)},
		"bool as text":      {kindBool, "1"},
		"int as float":      {kindInt, 1.5},
		"text as int":       {kindText, int64(1)},
		"timestamp garbage": {kindTimestamp, "yesterday"},
		"timestamp as int":  {kindTimestamp, int64(1)},
		"json garbage":      {kindJSON, `{`},
		"json trailing":     {kindJSON, `1 2`},
		"blob as string":    {kindBlob, "x"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := encodeSQLiteValue("t", col(test.kind), test.value); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	_, err = encodeSQLiteValue("tbl", sqliteColumn{name: "col", declared: "TEXT", kind: kindText}, 1.5)
	var unsupported *UnsupportedSQLiteTypeError
	if !errors.As(err, &unsupported) || unsupported.Table != "tbl" || unsupported.Column != "col" {
		t.Fatalf("err = %v, want UnsupportedSQLiteTypeError", err)
	}
}

func TestSQLiteColumnKindRejectsUnsupportedDeclaredTypes(t *testing.T) {
	for _, declared := range []string{"REAL", "NUMERIC", "ANY", ""} {
		_, err := resolveSQLiteKind("zones", "name", declared)
		var unsupported *UnsupportedSQLiteTypeError
		if !errors.As(err, &unsupported) || unsupported.TypeName != declared {
			t.Errorf("declared %q: err = %v, want UnsupportedSQLiteTypeError", declared, err)
		}
	}
	// A column known to hold booleans/timestamps must carry the matching type.
	if _, err := resolveSQLiteKind("zones", "enabled", "TEXT"); err == nil {
		t.Error("bool column declared TEXT accepted")
	}
	if _, err := resolveSQLiteKind("zones", "created_at", "INTEGER"); err == nil {
		t.Error("timestamp column declared INTEGER accepted")
	}
	if kind, err := resolveSQLiteKind("zones", "name", "BLOB"); err != nil || kind != kindBlob {
		t.Errorf("BLOB = %v, %v", kind, err)
	}
}
