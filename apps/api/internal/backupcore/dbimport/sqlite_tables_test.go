package dbimport

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
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

const (
	siUser     = "10000000-0000-4000-8000-000000000001"
	siSecret   = "10000000-0000-4000-8000-000000000002"
	siSecret2  = "10000000-0000-4000-8000-000000000003"
	siZone     = "10000000-0000-4000-8000-000000000004"
	siRecord   = "10000000-0000-4000-8000-000000000005"
	siPool     = "10000000-0000-4000-8000-000000000006"
	siRule     = "10000000-0000-4000-8000-000000000007"
	siStream   = "10000000-0000-4000-8000-000000000008"
	siCert     = "10000000-0000-4000-8000-000000000009"
	siProvider = "10000000-0000-4000-8000-00000000000a"
	siRevision = "10000000-0000-4000-8000-00000000000b"
	siCluster  = "10000000-0000-4000-8000-00000000000c"
	siInstall  = "10000000-0000-4000-8000-00000000000d"
	siNode     = "10000000-0000-4000-8000-00000000000e"
	siSession  = "10000000-0000-4000-8000-000000000011"
	siAudit    = "10000000-0000-4000-8000-000000000012"
	siJob      = "10000000-0000-4000-8000-000000000013"
	siSnapshot = "10000000-0000-4000-8000-000000000014"
	siArchive  = "10000000-0000-4000-8000-000000000015"
	siArtifact = "10000000-0000-4000-8000-000000000016"
	siToken    = "10000000-0000-4000-8000-000000000017"
)

var siMasterKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))

func openSQLiteImportDB(t testing.TB) *sql.DB {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "import.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	return db
}

func siExec(t testing.TB, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %.60q: %v", query, err)
	}
}

func siKey(t testing.TB, seconds int64, nanos int32) string {
	t.Helper()
	key, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: seconds, Nanos: nanos})
	if err != nil || key == nil {
		t.Fatalf("encode timestamp: %v", err)
	}
	return *key
}

// seedSQLiteImportSource writes representative rows into all 16 bundle tables
// (JSON in canonical sorted-key form so the round trip compares byte-exact).
func seedSQLiteImportSource(t testing.TB, db *sql.DB) {
	t.Helper()
	ts1 := siKey(t, 1700000000, 123456789)
	ts2 := siKey(t, 1700000100, 0)
	const negInf, posInf = "0", "2"
	siExec(t, db, `INSERT INTO users (id,username,password_hash,role,active,password_change_required,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		siUser, "owner", "hash", "owner", 1, 0, ts1, posInf)
	siExec(t, db, `INSERT INTO secrets (id,purpose,ciphertext,created_at,updated_at) VALUES (?,?,?,?,?)`, siSecret, "provider-token", "ciphertext-one", ts1, ts2)
	siExec(t, db, `INSERT INTO secrets (id,purpose,ciphertext,created_at,updated_at) VALUES (?,?,?,?,?)`, siSecret2, "ca-key", "ciphertext-two", ts1, ts2)
	siExec(t, db, `INSERT INTO installation_settings (id,ingress_ipv4,default_resolver_pool,forwarding_rules,enrollment_hostnames,retention_max_age_days,retention_max_size_mb,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		"singleton", "192.0.2.1", `{"a":[1,2.50],"b":2}`, `[]`, `["node.example.test"]`, 14, 100, ts1, ts2)
	siExec(t, db, `INSERT INTO resolver_pools (id,name,endpoints,is_default,created_at,updated_at) VALUES (?,?,?,?,?,?)`,
		siPool, "default", `[{"address":"192.0.2.53","port":53}]`, 1, ts1, ts2)
	siExec(t, db, `INSERT INTO zones (id,name,enabled,created_at,updated_at) VALUES (?,?,?,?,?)`, siZone, "example.test", 0, negInf, ts2)
	siExec(t, db, `INSERT INTO dns_records (id,zone_id,name,type,value,ttl,enabled,proxied,proxy_settings,comment,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		siRecord, siZone, "www", "A", `"192.0.2.10"`, 300, 1, 1, `{"tls":true}`, nil, ts1, ts2)
	siExec(t, db, `INSERT INTO forwarding_rules (id,suffix,pool_id,created_at,updated_at) VALUES (?,?,?,?,?)`, siRule, "corp.test", siPool, ts1, ts2)
	siExec(t, db, `INSERT INTO stream_routes (id,enabled,protocol,listen_address,listen_port,upstream,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		siStream, 1, "tcp", "0.0.0.0", 5353, `{"ip":"192.0.2.1","port":53}`, ts1, ts2)
	siExec(t, db, `INSERT INTO internal_ca (id,certificate_pem,key_secret_id,created_at,updated_at,enrollment_certificate_pem,enrollment_key_secret_id) VALUES (?,?,?,?,?,?,?)`,
		"ca", "-----BEGIN CERTIFICATE-----\ndummy\n-----END CERTIFICATE-----", siSecret2, ts1, ts2, nil, nil)
	siExec(t, db, `INSERT INTO internal_ca_enrollment_state (id,established_at) VALUES (?,?)`, "ca", ts1)
	siExec(t, db, `INSERT INTO certificates (id,hostnames,issuer,challenge,status,expires_at,renew_after,key_secret_id,certificate_pem,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		siCert, `["a.example.test","b.example.test"]`, "uploaded", "none", "active", posInf, nil, siSecret, "pem", ts1, ts2)
	siExec(t, db, `INSERT INTO provider_connections (id,provider,name,secret_id,scope,enabled,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		siProvider, "cloudflare", "main", siSecret, "zone", 1, ts1, ts2)
	siExec(t, db, `INSERT INTO config_revisions (id,revision_number,checksum,snapshot,actor_user_id,created_at,applied_at) VALUES (?,?,?,?,?,?,?)`,
		siRevision, 7, "sum", `{"n":12345678901234567890,"zones":[]}`, siUser, ts1, ts2)
	siExec(t, db, `INSERT INTO cluster_keys (id,purpose,wrapped_kek,wrapping_key_version,created_at,retired_at) VALUES (?,?,?,?,?,?)`,
		siCluster, "cluster", "wrapped", 3, ts1, nil)
	siExec(t, db, `INSERT INTO installation_identity (id,installation_id,node_id,role,leadership_generation,latest_known_generation,cluster_key_id,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		"identity", siInstall, siNode, "primary", int64(1)<<60, 2, siCluster, ts2)
	siExec(t, db, `INSERT INTO node_state (id,sync_enabled,consecutive_failures,last_seen_at,updated_at) VALUES (?,?,?,?,?)`, "node", 1, 4, ts1, ts2)
}

// seedSQLiteDependents fills the tables outside the bundle that reference it.
func seedSQLiteDependents(t testing.TB, db *sql.DB) {
	t.Helper()
	ts := siKey(t, 1700000200, 0)
	siExec(t, db, `INSERT INTO sessions(id,user_id,token_hash,expires_at,created_at) VALUES(?,?,?,'2',?)`, siSession, siUser, "token-hash", ts)
	siExec(t, db, `INSERT INTO audit_events(id,actor_user_id,action,resource_type,correlation_id,result,created_at) VALUES(?,?,?,?,?,?,?)`,
		siAudit, siUser, "login", "session", "", "success", ts)
	siExec(t, db, `INSERT INTO audit_events(id,actor_user_id,action,resource_type,correlation_id,result,created_at) VALUES(?,NULL,?,?,?,?,?)`,
		"10000000-0000-4000-8000-000000000099", "login.failed", "session", "", "failure", ts)
	siExec(t, db, `INSERT INTO apply_jobs(id,revision_id,target,correlation_id,actor_user_id,created_at) VALUES(?,?,'combined','c',?,?)`, siJob, siRevision, siUser, ts)
	siExec(t, db, `INSERT INTO applied_snapshots(id,source_primary_id,leadership_generation,snapshot_version,replication_version,content_hash,revision_id,apply_job_id,applied_at) VALUES(?,?,1,1,1,'hash',?,?,?)`,
		siSnapshot, siInstall, siRevision, siJob, ts)
	siExec(t, db, `INSERT INTO standalone_archives(id,revision_id,archive_blob,capture_reason,captured_at,created_at) VALUES(?,?,'blob','test',?,?)`, siArchive, siRevision, ts, ts)
	siExec(t, db, `INSERT INTO enrollment_tokens(id,token_selector,token_hash,hash_version,created_by_user_id,created_at,expires_at) VALUES(?,?,?,?,?,?,'2')`,
		siToken, "selector", "hash", "v1", siUser, ts)
	siExec(t, db, `INSERT INTO operational_artifacts VALUES(?,'go','artifact',-1,?,?)`, siArtifact, siRevision, ts)
}

type memZip struct{ files map[string][]byte }

func (z *memZip) Add(path string, _ int64, open func() (io.ReadCloser, error)) error {
	reader, err := open()
	if err != nil {
		return err
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if z.files == nil {
		z.files = map[string][]byte{}
	}
	z.files[path] = data
	return err
}

// sqlitePayloads exports src with the SQLite exporter and decodes the entries
// like the engine does, in the manifest (DefaultConfigTables) order.
func sqlitePayloads(t testing.TB, src *sql.DB) []tablePayload {
	t.Helper()
	zip := &memZip{}
	if _, err := dbexport.NewSQLite(src, dbexport.ExporterOptions{MasterKeyBase64: siMasterKey}).Export(context.Background(), zip); err != nil {
		t.Fatalf("export: %v", err)
	}
	payloads := make([]tablePayload, 0, len(dbexport.DefaultConfigTables))
	for _, table := range dbexport.DefaultConfigTables {
		rows, err := decodeTableRows(zip.files["db/"+table+".json"])
		if err != nil {
			t.Fatalf("decode %s: %v", table, err)
		}
		payloads = append(payloads, tablePayload{name: table, rows: rows})
	}
	return payloads
}

func payloadFor(payloads []tablePayload, table string) *tablePayload {
	for index := range payloads {
		if payloads[index].name == table {
			return &payloads[index]
		}
	}
	return nil
}

func sqliteTableNames(t testing.TB, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

// dumpSQLite renders every row of the given tables with its storage type, so a
// comparison catches type affinity as well as value changes.
func dumpSQLite(t testing.TB, db *sql.DB, tables []string, skip func(table string, row []any, columns []string) bool) map[string][]string {
	t.Helper()
	result := map[string][]string{}
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM "` + table + `" ORDER BY 1`)
		if err != nil {
			t.Fatalf("dump %s: %v", table, err)
		}
		columns, _ := rows.Columns()
		lines := []string{}
		for rows.Next() {
			values := make([]any, len(columns))
			targets := make([]any, len(columns))
			for index := range values {
				targets[index] = &values[index]
			}
			if err := rows.Scan(targets...); err != nil {
				t.Fatal(err)
			}
			if skip != nil && skip(table, values, columns) {
				continue
			}
			parts := make([]string, len(values))
			for index, value := range values {
				switch value := value.(type) {
				case []byte:
					parts[index] = fmt.Sprintf("%s=blob:%x", columns[index], value)
				default:
					parts[index] = fmt.Sprintf("%s=%T:%v", columns[index], value, value)
				}
			}
			lines = append(lines, strings.Join(parts, "|"))
		}
		rows.Close()
		result[table] = lines
	}
	return result
}

func skipMarker(table string, row []any, columns []string) bool {
	if table != "secrets" {
		return false
	}
	for index, column := range columns {
		if column == "purpose" && row[index] == dbexport.BackupMasterKeyMarkerPurpose {
			return true
		}
	}
	return false
}

func countRows(t testing.TB, db *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM "` + table + `"`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func newImporter(db *sql.DB) *SQLiteTableImporter { return NewSQLiteTableImporter(db, nil) }

func TestSQLiteDependentTablesMatchSchema(t *testing.T) {
	db := openSQLiteImportDB(t)
	bundle := map[string]bool{}
	for _, table := range dbexport.DefaultConfigTables {
		bundle[table] = true
	}
	// child -> parents, from the migrated schema.
	parents := map[string]map[string]bool{}
	for _, table := range sqliteTableNames(t, db) {
		rows, err := db.Query(`SELECT "table" FROM pragma_foreign_key_list(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var parent string
			if err := rows.Scan(&parent); err != nil {
				t.Fatal(err)
			}
			if parents[table] == nil {
				parents[table] = map[string]bool{}
			}
			parents[table][parent] = true
		}
		rows.Close()
	}
	// Transitive closure of non-bundle tables that reference the bundle.
	replaced := map[string]bool{}
	for table := range bundle {
		replaced[table] = true
	}
	dependents := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for child, set := range parents {
			if replaced[child] {
				continue
			}
			for parent := range set {
				if replaced[parent] {
					replaced[child], dependents[child] = true, true
					changed = true
					break
				}
			}
		}
	}
	got := SQLiteDependentTables()
	want := make([]string, 0, len(dependents))
	for table := range dependents {
		want = append(want, table)
	}
	sort.Strings(want)
	sortedGot := append([]string(nil), got...)
	sort.Strings(sortedGot)
	if !reflect.DeepEqual(sortedGot, want) {
		t.Fatalf("dependent tables = %v, schema closure = %v: handle new dependent tables in sqliteDependentDeletes", sortedGot, want)
	}

	// Deletion order (dependents, then the bundle in reverse export order)
	// must delete every child before its parent.
	order := append([]string(nil), got...)
	for index := len(dbexport.SQLiteExportOrder) - 1; index >= 0; index-- {
		order = append(order, dbexport.SQLiteExportOrder[index])
	}
	position := map[string]int{}
	for index, table := range order {
		position[table] = index
	}
	for child, set := range parents {
		for parent := range set {
			childAt, childOK := position[child]
			parentAt, parentOK := position[parent]
			if childOK && parentOK && child != parent && childAt > parentAt {
				t.Errorf("%s is deleted after its parent %s", child, parent)
			}
		}
	}
}

func TestSQLiteTableImporterRoundTripIntoFreshDatabase(t *testing.T) {
	src := openSQLiteImportDB(t)
	seedSQLiteImportSource(t, src)
	payloads := sqlitePayloads(t, src)

	dst := openSQLiteImportDB(t)
	previews, err := newImporter(dst).ReplaceTables(context.Background(), payloads)
	if err != nil {
		t.Fatalf("ReplaceTables: %v", err)
	}
	if len(previews) != len(payloads) || previews[0].Action != "truncate+reinsert" {
		t.Fatalf("previews = %+v", previews)
	}
	want := dumpSQLite(t, src, dbexport.DefaultConfigTables, skipMarker)
	got := dumpSQLite(t, dst, dbexport.DefaultConfigTables, skipMarker)
	if !reflect.DeepEqual(got, want) {
		for _, table := range dbexport.DefaultConfigTables {
			if !reflect.DeepEqual(got[table], want[table]) {
				t.Errorf("table %s differs:\n got %v\nwant %v", table, got[table], want[table])
			}
		}
		t.FailNow()
	}
	// Spot-check the representation, not just equality of the two sides.
	if got["zones"][0] == "" || !strings.Contains(got["zones"][0], "created_at=string:0") {
		t.Fatalf("negative infinity not stored as tag 0: %v", got["zones"])
	}
	if !strings.Contains(got["users"][0], "updated_at=string:2") || !strings.Contains(got["users"][0], "active=int64:1") {
		t.Fatalf("user row = %v", got["users"])
	}
	if !strings.Contains(got["installation_identity"][0], "leadership_generation=int64:1152921504606846976") {
		t.Fatalf("big int lost: %v", got["installation_identity"])
	}
	if !strings.Contains(got["config_revisions"][0], `"n":12345678901234567890`) {
		t.Fatalf("big JSON number lost: %v", got["config_revisions"])
	}
	// The master-key marker row is kept, like PostgreSQL does.
	var ciphertext string
	if err := dst.QueryRow(`SELECT ciphertext FROM secrets WHERE purpose = ?`, dbexport.BackupMasterKeyMarkerPurpose).Scan(&ciphertext); err != nil {
		t.Fatalf("marker row missing: %v", err)
	}
	if plaintext, err := secrets.DecryptSecret(ciphertext, siMasterKey); err != nil || plaintext != dbexport.BackupMasterKeyMarkerPlaintext {
		t.Fatalf("marker decrypt = %q, %v", plaintext, err)
	}
	assertForeignKeysIntact(t, dst)
}

func assertForeignKeysIntact(t testing.TB, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check reports a violation")
	}
}

func TestSQLiteTableImporterReplacesPopulatedDatabaseAndClearsDependents(t *testing.T) {
	src := openSQLiteImportDB(t)
	seedSQLiteImportSource(t, src)
	payloads := sqlitePayloads(t, src)
	// Same payload shape, different data: rename the user so replacement is visible.
	payloadFor(payloads, "users").rows[0]["username"] = "restored-owner"

	dst := openSQLiteImportDB(t)
	seedSQLiteImportSource(t, dst)
	siExec(t, dst, `UPDATE users SET username = 'stale'`)
	seedSQLiteDependents(t, dst)
	dependents := SQLiteDependentTables()
	for _, table := range []string{"sessions", "apply_jobs", "applied_snapshots", "standalone_archives", "operational_artifacts", "enrollment_tokens"} {
		if countRows(t, dst, table) == 0 {
			t.Fatalf("seed left %s empty", table)
		}
	}
	if countRows(t, dst, "audit_events") != 2 {
		t.Fatal("seed audit rows missing")
	}

	if _, err := newImporter(dst).ReplaceTables(context.Background(), payloads); err != nil {
		t.Fatalf("ReplaceTables: %v", err)
	}
	var username string
	if err := dst.QueryRow(`SELECT username FROM users`).Scan(&username); err != nil || username != "restored-owner" {
		t.Fatalf("username = %q, %v", username, err)
	}
	for _, table := range dependents {
		want := 0
		if table == "audit_events" {
			want = 1 // actor-less rows are kept
		}
		if got := countRows(t, dst, table); got != want {
			t.Errorf("%s rows after import = %d, want %d", table, got, want)
		}
	}
	assertForeignKeysIntact(t, dst)
}

func TestSQLiteTableImporterIsAtomicWhenALaterTableFails(t *testing.T) {
	src := openSQLiteImportDB(t)
	seedSQLiteImportSource(t, src)
	payloads := sqlitePayloads(t, src)
	// dns_records inserts after zones: a dangling zone fails mid-import, after
	// the deletes and the earlier inserts have already run.
	payloadFor(payloads, "dns_records").rows[0]["zone_id"] = "10000000-0000-4000-8000-0000000000ff"

	dst := openSQLiteImportDB(t)
	seedSQLiteImportSource(t, dst)
	siExec(t, dst, `UPDATE users SET username = 'before'`)
	seedSQLiteDependents(t, dst)
	tables := sqliteTableNames(t, dst)
	before := dumpSQLite(t, dst, tables, nil)

	_, err := newImporter(dst).ReplaceTables(context.Background(), payloads)
	if err == nil || !strings.Contains(err.Error(), `insert table "dns_records"`) {
		t.Fatalf("err = %v, want insert dns_records failure", err)
	}
	if after := dumpSQLite(t, dst, tables, nil); !reflect.DeepEqual(after, before) {
		t.Fatal("database changed after a failed import")
	}
}

func TestSQLiteTableImporterRejectsBadPayloadsBeforeTouchingTheDatabase(t *testing.T) {
	src := openSQLiteImportDB(t)
	seedSQLiteImportSource(t, src)
	cases := map[string]func(payloads []tablePayload){
		"unknown column":       func(p []tablePayload) { payloadFor(p, "zones").rows[0]["bogus"] = "x" },
		"bad uuid-bearing int": func(p []tablePayload) { payloadFor(p, "dns_records").rows[0]["ttl"] = json.Number("abc") },
		"bad timestamp":        func(p []tablePayload) { payloadFor(p, "zones").rows[0]["created_at"] = "yesterday" },
		"bad boolean":          func(p []tablePayload) { payloadFor(p, "zones").rows[0]["enabled"] = "maybe" },
		"missing not null":     func(p []tablePayload) { delete(payloadFor(p, "zones").rows[0], "name") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			payloads := sqlitePayloads(t, src)
			mutate(payloads)
			dst := openSQLiteImportDB(t)
			seedSQLiteImportSource(t, dst)
			tables := sqliteTableNames(t, dst)
			before := dumpSQLite(t, dst, tables, nil)
			if _, err := newImporter(dst).ReplaceTables(context.Background(), payloads); err == nil {
				t.Fatal("expected an error")
			}
			if after := dumpSQLite(t, dst, tables, nil); !reflect.DeepEqual(after, before) {
				t.Fatal("database changed after a rejected payload")
			}
		})
	}
}

func TestSQLiteTableImporterSingleFlight(t *testing.T) {
	src := openSQLiteImportDB(t)
	seedSQLiteImportSource(t, src)
	payloads := sqlitePayloads(t, src)
	dst := openSQLiteImportDB(t)
	importer := newImporter(dst)

	lockValue, _ := sqliteImportLocks.LoadOrStore(dst, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	_, err := importer.ReplaceTables(context.Background(), payloads)
	lock.Unlock()
	if !errors.Is(err, ErrImportAlreadyInProgress) {
		t.Fatalf("err = %v, want ErrImportAlreadyInProgress", err)
	}
	if countRows(t, dst, "users") != 0 {
		t.Fatal("rejected import wrote rows")
	}
	if _, err := importer.ReplaceTables(context.Background(), payloads); err != nil {
		t.Fatalf("import after release: %v", err)
	}
}

func TestSQLiteTableImporterConcurrentImportsAllowOneAtATime(t *testing.T) {
	src := openSQLiteImportDB(t)
	seedSQLiteImportSource(t, src)
	payloads := sqlitePayloads(t, src)
	dst := openSQLiteImportDB(t)
	importer := newImporter(dst)

	results := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := importer.ReplaceTables(context.Background(), payloads)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, ErrImportAlreadyInProgress) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if countRows(t, dst, "users") != 1 {
		t.Fatal("final state is not one consistent import")
	}
}

func TestSQLiteTableImporterHonorsCanceledContext(t *testing.T) {
	src := openSQLiteImportDB(t)
	seedSQLiteImportSource(t, src)
	payloads := sqlitePayloads(t, src)
	dst := openSQLiteImportDB(t)
	seedSQLiteImportSource(t, dst)
	tables := sqliteTableNames(t, dst)
	before := dumpSQLite(t, dst, tables, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newImporter(dst).ReplaceTables(ctx, payloads); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if after := dumpSQLite(t, dst, tables, nil); !reflect.DeepEqual(after, before) {
		t.Fatal("database changed after cancellation")
	}
}

func TestSQLiteTableImporterRejectsUnknownTableAndNilDatabase(t *testing.T) {
	if _, err := (*SQLiteTableImporter)(nil).ReplaceTables(context.Background(), nil); err == nil {
		t.Fatal("nil importer must fail")
	}
	dst := openSQLiteImportDB(t)
	_, err := newImporter(dst).ReplaceTables(context.Background(), []tablePayload{{name: "sessions"}})
	if err == nil || !strings.Contains(err.Error(), "unsupported backup table") {
		t.Fatalf("err = %v", err)
	}
}

func TestCoerceSQLiteValue(t *testing.T) {
	spec := func(kind dbexport.SQLiteKind, name string) dbexport.SQLiteColumnSpec {
		return dbexport.SQLiteColumnSpec{Name: name, Kind: kind, Declared: "X"}
	}
	finite := func(seconds int64, nanos int32) string { return siKey(t, seconds, nanos) }
	tests := []struct {
		name   string
		column dbexport.SQLiteColumnSpec
		value  any
		want   any
		fail   bool
	}{
		{"null", spec(dbexport.SQLiteKindText, "name"), nil, nil, false},
		{"uuid lowercased", spec(dbexport.SQLiteKindText, "zone_id"), "10000000-0000-4000-8000-0000000000AB", "10000000-0000-4000-8000-0000000000ab", false},
		{"uuid compact", spec(dbexport.SQLiteKindText, "id"), "10000000000040008000000000000AB0", "10000000-0000-4000-8000-000000000ab0", false},
		{"non uuid id kept", spec(dbexport.SQLiteKindText, "id"), "singleton", "singleton", false},
		{"uppercase non-id text kept", spec(dbexport.SQLiteKindText, "name"), "10000000-0000-4000-8000-0000000000AB", "10000000-0000-4000-8000-0000000000AB", false},
		{"text number", spec(dbexport.SQLiteKindText, "name"), json.Number("12"), "12", false},
		{"text wrong type", spec(dbexport.SQLiteKindText, "name"), true, nil, true},
		{"int", spec(dbexport.SQLiteKindInt, "ttl"), json.Number("300"), int64(300), false},
		{"int float64", spec(dbexport.SQLiteKindInt, "ttl"), float64(300), int64(300), false},
		{"int big string", spec(dbexport.SQLiteKindInt, "n"), "1152921504606846976", int64(1) << 60, false},
		{"int min", spec(dbexport.SQLiteKindInt, "n"), "-9223372036854775808", int64(-9223372036854775808), false},
		{"int exponent", spec(dbexport.SQLiteKindInt, "n"), json.Number("3e2"), int64(300), false},
		{"int fractional", spec(dbexport.SQLiteKindInt, "n"), json.Number("1.5"), nil, true},
		{"int overflow", spec(dbexport.SQLiteKindInt, "n"), "9223372036854775808", nil, true},
		{"bool true", spec(dbexport.SQLiteKindBool, "active"), true, int64(1), false},
		{"bool false", spec(dbexport.SQLiteKindBool, "active"), false, int64(0), false},
		{"bool bad", spec(dbexport.SQLiteKindBool, "active"), "maybe", nil, true},
		{"timestamp finite", spec(dbexport.SQLiteKindTimestamp, "created_at"), "2023-11-14T22:13:20.123456789Z", finite(1700000000, 123456789), false},
		{"timestamp offset", spec(dbexport.SQLiteKindTimestamp, "created_at"), "2023-11-15T00:13:20+02:00", finite(1700000000, 0), false},
		{"timestamp pg text", spec(dbexport.SQLiteKindTimestamp, "created_at"), "2023-11-14 22:13:20.5", finite(1700000000, 500000000), false},
		{"timestamp before epoch", spec(dbexport.SQLiteKindTimestamp, "created_at"), "1969-12-31T23:59:59.5Z", finite(-1, 500000000), false},
		{"infinity", spec(dbexport.SQLiteKindTimestamp, "created_at"), "infinity", "2", false},
		{"-infinity", spec(dbexport.SQLiteKindTimestamp, "created_at"), "-infinity", "0", false},
		{"timestamp bad", spec(dbexport.SQLiteKindTimestamp, "created_at"), "soon", nil, true},
		{"timestamp wrong type", spec(dbexport.SQLiteKindTimestamp, "created_at"), json.Number("1"), nil, true},
		{"json object", spec(dbexport.SQLiteKindJSON, "snapshot"), map[string]any{"b": json.Number("2.50"), "a": []any{}}, `{"a":[],"b":2.50}`, false},
		{"json string", spec(dbexport.SQLiteKindJSON, "value"), "192.0.2.10", `"192.0.2.10"`, false},
		{"json bool", spec(dbexport.SQLiteKindJSON, "value"), true, `true`, false},
		{"blob", spec(dbexport.SQLiteKindBlob, "snapshot_body"), base64.RawURLEncoding.EncodeToString([]byte{0, 255, 195, 40}), []byte{0, 255, 195, 40}, false},
		{"blob empty", spec(dbexport.SQLiteKindBlob, "snapshot_body"), "", []byte{}, false},
		{"blob bad", spec(dbexport.SQLiteKindBlob, "snapshot_body"), "***", nil, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := coerceSQLiteValue(test.column, test.value)
			if (err != nil) != test.fail {
				t.Fatalf("err = %v, want failure %v", err, test.fail)
			}
			if !test.fail && !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %#v, want %#v", got, test.want)
			}
		})
	}
	var unsupported *dbexport.UnsupportedSQLiteTypeError
	if _, err := coerceSQLiteValue(dbexport.SQLiteColumnSpec{Name: "x", Kind: dbexport.SQLiteKind(99), Declared: "REAL"}, "v"); !errors.As(err, &unsupported) {
		t.Fatalf("unknown kind err = %v", err)
	}
}
