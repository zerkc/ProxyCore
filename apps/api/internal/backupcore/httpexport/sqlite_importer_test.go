package httpexport

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbimport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/hostread"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

const (
	sqliteOwnerID    = "40000000-0000-4000-8000-000000000002"
	sqliteSecretID   = "40000000-0000-4000-8000-000000000003"
	sqliteZoneID     = "40000000-0000-4000-8000-000000000004"
	sqliteGoneActor  = "40000000-0000-4000-8000-0000000000ee"
	sqliteRecordID   = "40000000-0000-4000-8000-000000000010"
	sqliteSessionID  = "40000000-0000-4000-8000-000000000011"
	sqliteInstallID  = "40000000-0000-4000-8000-000000000012"
	sqliteNodeID     = "40000000-0000-4000-8000-000000000013"
	sqliteClusterKey = "40000000-0000-4000-8000-000000000014"
)

type recordedApply struct {
	mu     sync.Mutex
	actors []string
	err    error
}

func (a *recordedApply) Trigger(_ context.Context, actorID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.actors = append(a.actors, actorID)
	return a.err
}

func newMigratedSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "target.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func sqliteExecT(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %.60q: %v", query, err)
	}
}

// seedRemainingSQLiteTables adds the other 12 bundle tables on top of the
// fixture's users, secrets, zones and config_revisions rows.
func seedRemainingSQLiteTables(t *testing.T, db *sql.DB) {
	t.Helper()
	key := func(seconds int64, nanos int32) string {
		encoded, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: seconds, Nanos: nanos})
		if err != nil || encoded == nil {
			t.Fatalf("encode: %v", err)
		}
		return *encoded
	}
	ts1, ts2 := key(1700000000, 123456789), key(1700000100, 0)
	exec := func(q string, args ...any) { sqliteExecT(t, db, q, args...) }
	exec(`INSERT INTO installation_settings (id,ingress_ipv4,default_resolver_pool,forwarding_rules,enrollment_hostnames,retention_max_age_days,retention_max_size_mb,created_at,updated_at) VALUES ('singleton','192.0.2.1','{"a":[1,2.50],"b":2}','[]','["node.example.test"]',14,100,?,?)`, ts1, ts2)
	exec(`INSERT INTO resolver_pools (id,name,endpoints,is_default,created_at,updated_at) VALUES ('40000000-0000-4000-8000-000000000020','default','[{"address":"192.0.2.53","port":53}]',1,?,?)`, ts1, ts2)
	exec(`INSERT INTO dns_records (id,zone_id,name,type,value,ttl,enabled,proxied,proxy_settings,comment,created_at,updated_at) VALUES (?,?,'www','A','"192.0.2.10"',300,1,1,'{"tls":true}',NULL,?,?)`, sqliteRecordID, sqliteZoneID, ts1, ts2)
	exec(`INSERT INTO forwarding_rules (id,suffix,pool_id,created_at,updated_at) VALUES ('40000000-0000-4000-8000-000000000021','corp.test','40000000-0000-4000-8000-000000000020',?,?)`, ts1, ts2)
	exec(`INSERT INTO stream_routes (id,enabled,protocol,listen_address,listen_port,upstream,created_at,updated_at) VALUES ('40000000-0000-4000-8000-000000000022',1,'tcp','0.0.0.0',5353,'{"ip":"192.0.2.1","port":53}',?,?)`, ts1, ts2)
	exec(`INSERT INTO internal_ca (id,certificate_pem,key_secret_id,created_at,updated_at) VALUES ('ca','pem',?,?,?)`, sqliteSecretID, ts1, ts2)
	exec(`INSERT INTO internal_ca_enrollment_state (id,established_at) VALUES ('ca',?)`, ts1)
	exec(`INSERT INTO certificates (id,hostnames,issuer,challenge,status,expires_at,renew_after,key_secret_id,certificate_pem,created_at,updated_at) VALUES ('40000000-0000-4000-8000-000000000023','["a.example.test"]','uploaded','none','active','2',NULL,?,'pem',?,?)`, sqliteSecretID, ts1, ts2)
	exec(`INSERT INTO provider_connections (id,provider,name,secret_id,scope,enabled,created_at,updated_at) VALUES ('40000000-0000-4000-8000-000000000024','cloudflare','main',?,'zone',1,?,?)`, sqliteSecretID, ts1, ts2)
	exec(`INSERT INTO cluster_keys (id,purpose,wrapped_kek,wrapping_key_version,created_at,retired_at) VALUES (?,'cluster','wrapped',3,?,NULL)`, sqliteClusterKey, ts1)
	exec(`INSERT INTO installation_identity (id,installation_id,node_id,role,leadership_generation,latest_known_generation,cluster_key_id,updated_at) VALUES ('identity',?,?,'primary',?,2,?,?)`, sqliteInstallID, sqliteNodeID, int64(1)<<60, sqliteClusterKey, ts2)
	exec(`INSERT INTO node_state (id,sync_enabled,consecutive_failures,last_seen_at,updated_at) VALUES ('node',1,4,?,?)`, ts1, ts2)
}

var sqliteBundleTables = dbexport.DefaultConfigTables

func dumpSQLiteTables(t *testing.T, db *sql.DB, tables []string, skipMarker bool) map[string][]string {
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
			marker := false
			parts := make([]string, len(values))
			for index, value := range values {
				if columns[index] == "purpose" && value == dbexport.BackupMasterKeyMarkerPurpose {
					marker = true
				}
				if raw, ok := value.([]byte); ok {
					parts[index] = fmt.Sprintf("%s=blob:%x", columns[index], raw)
				} else {
					parts[index] = fmt.Sprintf("%s=%T:%v", columns[index], value, value)
				}
			}
			if skipMarker && table == "secrets" && marker {
				continue
			}
			lines = append(lines, strings.Join(parts, "|"))
		}
		rows.Close()
		result[table] = lines
	}
	return result
}

func exportSQLiteBundle(t *testing.T, fixture *sqliteBundleFixture, passphrase []byte) []byte {
	t.Helper()
	var bundle bytes.Buffer
	if _, err := fixture.exporter.Export(context.Background(), &bundle, passphrase); err != nil {
		t.Fatalf("export: %v", err)
	}
	return bundle.Bytes()
}

type importerHarness struct {
	importer *SQLiteBackupImporter
	apply    *recordedApply
	envPath  string
	certRoot string
}

func newImporterHarness(t *testing.T, db *sql.DB, masterKey string) *importerHarness {
	t.Helper()
	dir := t.TempDir()
	apply := &recordedApply{}
	h := &importerHarness{apply: apply, envPath: filepath.Join(dir, "restored.env"), certRoot: filepath.Join(dir, "candidates")}
	h.importer = &SQLiteBackupImporter{
		DB:              db,
		MasterKeyBase64: masterKey,
		EnvRestorePath:  h.envPath,
		CandidateRoot:   h.certRoot,
		EnvMode:         "0600",
		Apply:           apply,
		Audit:           &dbimport.SQLiteAuditEmitter{DB: db},
		NewRevisionID:   "restored-revision",
	}
	return h
}

func (h *importerHarness) run(t *testing.T, bundle, passphrase []byte, dryRun bool, actor string) (httpserver.ImportReport, error) {
	t.Helper()
	return h.importer.ImportWithActorID(context.Background(), bytes.NewReader(bundle), int64(len(bundle)), passphrase, dryRun, actor)
}

type auditSummary struct {
	action, result string
	actor          sql.NullString
	after          string
}

func auditRows(t *testing.T, db *sql.DB) []auditSummary {
	t.Helper()
	rows, err := db.Query(`SELECT action, result, actor_user_id, after_value FROM audit_events WHERE action LIKE 'backup.import%' ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditSummary
	for rows.Next() {
		var row auditSummary
		if err := rows.Scan(&row.action, &row.result, &row.actor, &row.after); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	return out
}

func TestSQLiteBackupImporterRoundTripsEveryTableIntoAFreshDatabase(t *testing.T) {
	for _, passphrase := range [][]byte{nil, []byte("correct horse battery staple")} {
		t.Run(fmt.Sprintf("passphrase=%v", passphrase != nil), func(t *testing.T) {
			fixture := newSQLiteBundleFixture(t)
			seedRemainingSQLiteTables(t, fixture.db)
			bundle := exportSQLiteBundle(t, fixture, passphrase)

			target := newMigratedSQLite(t)
			h := newImporterHarness(t, target, sqliteMasterKey)
			report, err := h.run(t, bundle, passphrase, false, sqliteOwnerID)
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			if report.DryRun || len(report.Tables) != len(sqliteBundleTables) || report.BundleSHA256 == "" {
				t.Fatalf("report = %+v", report)
			}
			want := dumpSQLiteTables(t, fixture.db, sqliteBundleTables, true)
			got := dumpSQLiteTables(t, target, sqliteBundleTables, true)
			if !reflect.DeepEqual(got, want) {
				for _, table := range sqliteBundleTables {
					if !reflect.DeepEqual(got[table], want[table]) {
						t.Errorf("table %s differs:\n got %v\nwant %v", table, got[table], want[table])
					}
				}
				t.FailNow()
			}
			var ciphertext string
			if err := target.QueryRow(`SELECT ciphertext FROM secrets WHERE purpose = ?`, dbexport.BackupMasterKeyMarkerPurpose).Scan(&ciphertext); err != nil {
				t.Fatalf("marker row not kept: %v", err)
			}
			if plain, err := secrets.DecryptSecret(ciphertext, sqliteMasterKey); err != nil || plain != dbexport.BackupMasterKeyMarkerPlaintext {
				t.Fatalf("marker decrypt = %q, %v", plain, err)
			}
			// Files restored and apply triggered with the actor that still exists.
			if data, err := os.ReadFile(h.envPath); err != nil || string(data) != "PROXYCORE_TEST=1\n" {
				t.Fatalf("env = %q, %v", data, err)
			}
			if !reflect.DeepEqual(h.apply.actors, []string{sqliteOwnerID}) {
				t.Fatalf("apply actors = %v", h.apply.actors)
			}
			audits := auditRows(t, target)
			if len(audits) != 1 || audits[0].action != "backup.import" || audits[0].result != "success" || audits[0].actor.String != sqliteOwnerID {
				t.Fatalf("audit = %+v", audits)
			}
			if strings.Contains(audits[0].after, string(passphrase)) && passphrase != nil {
				t.Fatal("audit leaks the passphrase")
			}
		})
	}
}

func TestSQLiteBackupImporterReplacesPopulatedDatabaseAndClearsDependents(t *testing.T) {
	fixture := newSQLiteBundleFixture(t)
	bundle := exportSQLiteBundle(t, fixture, nil)

	target := newMigratedSQLite(t)
	ts := "2"
	sqliteExecT(t, target, `INSERT INTO users (id,username,password_hash,role,created_at,updated_at) VALUES (?,?,?,?,?,?)`, sqliteGoneActor, "stale", "hash", "owner", ts, ts)
	sqliteExecT(t, target, `INSERT INTO zones (id,name,created_at,updated_at) VALUES (?,?,?,?)`, "40000000-0000-4000-8000-0000000000dd", "stale.test", ts, ts)
	sqliteExecT(t, target, `INSERT INTO sessions(id,user_id,token_hash,expires_at,created_at) VALUES(?,?,?,'2',?)`, sqliteSessionID, sqliteGoneActor, "token", ts)
	h := newImporterHarness(t, target, sqliteMasterKey)

	if _, err := h.run(t, bundle, nil, false, sqliteGoneActor); err != nil {
		t.Fatalf("import: %v", err)
	}
	var zones, sessions, staleUsers int
	_ = target.QueryRow(`SELECT count(*) FROM zones`).Scan(&zones)
	_ = target.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions)
	_ = target.QueryRow(`SELECT count(*) FROM users WHERE username = 'stale'`).Scan(&staleUsers)
	if zones != 1 || sessions != 0 || staleUsers != 0 {
		t.Fatalf("zones=%d sessions=%d staleUsers=%d", zones, sessions, staleUsers)
	}
	// The actor no longer exists: apply and audit must not hit the FK.
	if !reflect.DeepEqual(h.apply.actors, []string{""}) {
		t.Fatalf("apply actors = %v, want empty actor", h.apply.actors)
	}
	audits := auditRows(t, target)
	if len(audits) != 1 || audits[0].result != "success" || audits[0].actor.Valid {
		t.Fatalf("audit = %+v, want success with NULL actor", audits)
	}
}

func populatedTarget(t *testing.T) *sql.DB {
	t.Helper()
	other := newSQLiteBundleFixture(t)
	sqliteExecT(t, other.db, `UPDATE users SET username = 'before-import'`)
	return other.db
}

func TestSQLiteBackupImporterWrongMasterKeyLeavesDatabaseUntouched(t *testing.T) {
	bundle := exportSQLiteBundle(t, newSQLiteBundleFixture(t), nil)
	target := populatedTarget(t)
	before := dumpSQLiteTables(t, target, sqliteBundleTables, false)
	h := newImporterHarness(t, target, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	_, err := h.run(t, bundle, nil, false, sqliteOwnerID)
	if !errors.Is(err, httpserver.ErrMasterKeyMismatch) {
		t.Fatalf("err = %v, want ErrMasterKeyMismatch", err)
	}
	if after := dumpSQLiteTables(t, target, sqliteBundleTables, false); !reflect.DeepEqual(after, before) {
		t.Fatal("database changed after master-key mismatch")
	}
	if _, statErr := os.Stat(h.envPath); statErr == nil {
		t.Fatal("env file restored despite mismatch")
	}
	if len(h.apply.actors) != 0 {
		t.Fatal("apply triggered despite mismatch")
	}
	audits := auditRows(t, target)
	if len(audits) != 1 || audits[0].action != "backup.import" || audits[0].result != "failure" || audits[0].actor.String != sqliteOwnerID {
		t.Fatalf("audit = %+v, want one failure row", audits)
	}
}

func TestSQLiteBackupImporterRejectsCorruptedBundles(t *testing.T) {
	good := exportSQLiteBundle(t, newSQLiteBundleFixture(t), nil)
	truncated := good[:len(good)/2]
	// Entries are deflated, so damage every 5th byte: deterministic and
	// guaranteed to hit entry data, headers and the central directory.
	scrambled := append([]byte(nil), good...)
	for index := 0; index < len(scrambled); index += 5 {
		scrambled[index] ^= 0xff
	}
	for name, bundle := range map[string][]byte{"garbage": []byte("not a zip"), "truncated": truncated, "scrambled": scrambled} {
		t.Run(name, func(t *testing.T) {
			target := populatedTarget(t)
			before := dumpSQLiteTables(t, target, sqliteBundleTables, false)
			h := newImporterHarness(t, target, sqliteMasterKey)
			if _, err := h.run(t, bundle, nil, false, ""); err == nil {
				t.Fatal("corrupted bundle imported")
			}
			if after := dumpSQLiteTables(t, target, sqliteBundleTables, false); !reflect.DeepEqual(after, before) {
				t.Fatal("database changed after a corrupted bundle")
			}
		})
	}
}

func TestSQLiteBackupImporterEncryptedBundleNeedsThePassphrase(t *testing.T) {
	bundle := exportSQLiteBundle(t, newSQLiteBundleFixture(t), []byte("right"))
	target := populatedTarget(t)
	before := dumpSQLiteTables(t, target, sqliteBundleTables, false)
	h := newImporterHarness(t, target, sqliteMasterKey)
	if _, err := h.run(t, bundle, []byte("wrong"), false, ""); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
	if after := dumpSQLiteTables(t, target, sqliteBundleTables, false); !reflect.DeepEqual(after, before) {
		t.Fatal("database changed after a wrong passphrase")
	}
}

func TestSQLiteBackupImporterDryRunWritesNothing(t *testing.T) {
	bundle := exportSQLiteBundle(t, newSQLiteBundleFixture(t), nil)
	target := populatedTarget(t)
	before := dumpSQLiteTables(t, target, sqliteBundleTables, false)
	h := newImporterHarness(t, target, sqliteMasterKey)

	report, err := h.run(t, bundle, nil, true, sqliteOwnerID)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !report.DryRun || len(report.Tables) != len(sqliteBundleTables) || report.AppliedPostImport {
		t.Fatalf("report = %+v", report)
	}
	counts := map[string]int{}
	for _, table := range report.Tables {
		counts[table.Name] = table.RowCount
	}
	if counts["secrets"] != 2 || counts["users"] != 1 || counts["zones"] != 1 {
		t.Fatalf("preview counts = %v", counts)
	}
	if after := dumpSQLiteTables(t, target, sqliteBundleTables, false); !reflect.DeepEqual(after, before) {
		t.Fatal("dry run changed the database")
	}
	if _, statErr := os.Stat(h.envPath); statErr == nil || len(h.apply.actors) != 0 {
		t.Fatal("dry run restored files or applied")
	}
	audits := auditRows(t, target)
	if len(audits) != 1 || audits[0].action != "backup.import.dryrun" || audits[0].result != "success" {
		t.Fatalf("audit = %+v", audits)
	}
}

func TestSQLiteBackupImporterConcurrentImportsAreSingleFlight(t *testing.T) {
	bundle := exportSQLiteBundle(t, newSQLiteBundleFixture(t), nil)
	target := newMigratedSQLite(t)
	h := newImporterHarness(t, target, sqliteMasterKey)
	h.importer.Audit = nil

	const workers = 6
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.run(t, bundle, nil, false, "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	succeeded := 0
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, dbimport.ErrImportAlreadyInProgress):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if succeeded == 0 {
		t.Fatal("no import succeeded")
	}
	var users int
	_ = target.QueryRow(`SELECT count(*) FROM users`).Scan(&users)
	if users != 1 {
		t.Fatalf("users = %d", users)
	}
}

func TestSQLiteBackupImporterCanceledContext(t *testing.T) {
	bundle := exportSQLiteBundle(t, newSQLiteBundleFixture(t), nil)
	target := populatedTarget(t)
	before := dumpSQLiteTables(t, target, sqliteBundleTables, false)
	h := newImporterHarness(t, target, sqliteMasterKey)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.importer.ImportWithActorID(ctx, bytes.NewReader(bundle), int64(len(bundle)), nil, false, ""); err == nil {
		t.Fatal("canceled import succeeded")
	}
	if after := dumpSQLiteTables(t, target, sqliteBundleTables, false); !reflect.DeepEqual(after, before) {
		t.Fatal("database changed after cancellation")
	}
	// Like PostgreSQL, a failure before the engine starts (archive open) is not audited.
	if audits := auditRows(t, target); len(audits) != 0 {
		t.Fatalf("audit = %+v", audits)
	}
}

func TestSQLiteBackupImporterCancelDuringReplaceRollsBackAndAuditsFailure(t *testing.T) {
	bundle := exportSQLiteBundle(t, newSQLiteBundleFixture(t), nil)
	target := populatedTarget(t)
	before := dumpSQLiteTables(t, target, sqliteBundleTables, false)
	h := newImporterHarness(t, target, sqliteMasterKey)
	ctx, cancel := context.WithCancel(context.Background())
	h.importer.Apply = nil
	h.importer.Audit = &dbimport.SQLiteAuditEmitter{DB: target}
	// Cancel as soon as the importer reaches the database: the first table
	// description runs on the transaction connection with this context.
	h.importer.Writer = sqlitedb.NewWriter(target, func() sqlitedb.FiniteTimestamp {
		cancel()
		return sqlitedb.FiniteTimestamp{Seconds: 1700000000}
	})
	if _, err := h.importer.ImportWithActorID(ctx, bytes.NewReader(bundle), int64(len(bundle)), nil, false, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if after := dumpSQLiteTables(t, target, sqliteBundleTables, false); !reflect.DeepEqual(after, before) {
		t.Fatal("database changed after cancellation during the import transaction")
	}
	if audits := auditRows(t, target); len(audits) != 1 || audits[0].result != "failure" {
		t.Fatalf("audit = %+v", audits)
	}
}

func TestSQLiteBackupImporterFailedApplyKeepsTheImportAndAuditsFailure(t *testing.T) {
	bundle := exportSQLiteBundle(t, newSQLiteBundleFixture(t), nil)
	target := newMigratedSQLite(t)
	h := newImporterHarness(t, target, sqliteMasterKey)
	h.apply.err = errors.New("apply failed")
	if _, err := h.run(t, bundle, nil, false, ""); err == nil || err.Error() != "apply failed" {
		t.Fatalf("err = %v", err)
	}
	var users int
	_ = target.QueryRow(`SELECT count(*) FROM users`).Scan(&users)
	if users != 1 {
		t.Fatal("committed import was rolled back by the apply failure")
	}
	if audits := auditRows(t, target); len(audits) != 1 || audits[0].result != "failure" {
		t.Fatalf("audit = %+v", audits)
	}
}

func TestSQLiteBackupImporterBodyLimitAndNilGuards(t *testing.T) {
	target := newMigratedSQLite(t)
	h := newImporterHarness(t, target, sqliteMasterKey)
	h.importer.BodyLimit = 10
	if _, err := h.importer.Import(context.Background(), strings.NewReader("x"), 11, nil, false); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
	if _, err := (*SQLiteBackupImporter)(nil).Import(context.Background(), strings.NewReader(""), 0, nil, false); err == nil {
		t.Fatal("nil importer must fail")
	}
	if _, err := (&SQLiteBackupImporter{}).Import(context.Background(), strings.NewReader(""), 0, nil, false); err == nil {
		t.Fatal("nil database must fail")
	}
}

// pgEraBundle assembles a bundle whose db/*.json entries follow the PostgreSQL
// exporter's representation byte for byte (RFC3339 timestamps, uppercase UUIDs
// as a hand-edited dump may carry them, float64-style numbers, decimal strings
// above 2^53, jsonb embedded as JSON, bytea as unpadded base64url).
func pgEraBundle(t *testing.T, tables map[string]string) []byte {
	t.Helper()
	fixture := newSQLiteBundleFixture(t)
	marker, err := secrets.EncryptSecret(dbexport.BackupMasterKeyMarkerPlaintext, sqliteMasterKey)
	if err != nil {
		t.Fatal(err)
	}
	tables["secrets"] = strings.ReplaceAll(tables["secrets"], "MARKER", marker)
	var out bytes.Buffer
	_, err = exportBundle(context.Background(), &out, nil, bundleConfig{
		Identity:        staticIdentitySource{},
		Version:         "pg-era",
		Now:             func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
		MasterKeyBase64: sqliteMasterKey,
		Host:            hostread.NewSQLite(fixture.db, fixture.exporter.HostConfig),
		ExportDB: func(ctx context.Context, entries dbexport.ZipWriter, _ func() time.Time, _ string) (dbexport.ExportReport, error) {
			report := dbexport.ExportReport{}
			for _, table := range dbexport.DefaultConfigTables {
				payload := tables[table]
				if payload == "" {
					payload = "[]"
				}
				var rows []json.RawMessage
				if err := json.Unmarshal([]byte(payload), &rows); err != nil {
					t.Fatalf("hand-crafted %s: %v", table, err)
				}
				data := []byte(payload)
				if err := entries.Add("db/"+table+".json", int64(len(data)), func() (io.ReadCloser, error) {
					return io.NopCloser(bytes.NewReader(data)), nil
				}); err != nil {
					return report, err
				}
				report.Tables = append(report.Tables, dbexport.TableStats{Name: table, RowCount: len(rows)})
			}
			return report, nil
		},
	})
	if err != nil {
		t.Fatalf("assemble pg-era bundle: %v", err)
	}
	return out.Bytes()
}

func TestSQLiteBackupImporterImportsAPostgresEraBundle(t *testing.T) {
	bundle := pgEraBundle(t, map[string]string{
		"users": `[{"id":"40000000-0000-4000-8000-0000000000AA","username":"pg-owner","password_hash":"hash","role":"owner","active":true,"password_change_required":false,"created_at":"2026-01-01T10:00:00.123456Z","updated_at":"2026-01-01T12:00:00+02:00"}]`,
		"secrets": `[{"id":"00000000-0000-0000-0000-000000000000","purpose":"__backup_master_key_marker__","ciphertext":"MARKER","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"},` +
			`{"id":"40000000-0000-4000-8000-0000000000BB","purpose":"token","ciphertext":"cipher-pg","created_at":"infinity","updated_at":"-infinity"}]`,
		"zones":                 `[{"id":"40000000-0000-4000-8000-0000000000CC","name":"pg.test","enabled":true,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`,
		"dns_records":           `[{"id":"40000000-0000-4000-8000-0000000000DD","zone_id":"40000000-0000-4000-8000-0000000000CC","name":"www","type":"A","value":"192.0.2.9","ttl":300,"enabled":true,"proxied":false,"proxy_settings":{"b":[1,2.5],"a":null},"comment":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`,
		"installation_identity": `[{"id":"identity","installation_id":"40000000-0000-4000-8000-0000000000EE","node_id":"40000000-0000-4000-8000-0000000000EF","role":"primary","leadership_generation":"9007199254740993","latest_known_generation":2,"cluster_key_id":null,"updated_at":"2026-01-01T00:00:00Z"}]`,
		"config_revisions":      `[{"id":"40000000-0000-4000-8000-0000000000F0","revision_number":3,"checksum":"c","source":"ordinary","snapshot":{"zones":[{"name":"pg.test"}],"big":12345678901234567890},"actor_user_id":"40000000-0000-4000-8000-0000000000AA","created_at":"2026-01-01T00:00:00Z","applied_at":null}]`,
	})
	target := newMigratedSQLite(t)
	h := newImporterHarness(t, target, sqliteMasterKey)
	if _, err := h.run(t, bundle, nil, false, "40000000-0000-4000-8000-0000000000aa"); err != nil {
		t.Fatalf("import: %v", err)
	}
	var id, updated string
	var active int
	if err := target.QueryRow(`SELECT id, active, updated_at FROM users`).Scan(&id, &active, &updated); err != nil {
		t.Fatal(err)
	}
	// 12:00+02:00 is 10:00:00Z.
	want, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC).Unix()})
	if id != "40000000-0000-4000-8000-0000000000aa" || active != 1 || updated != *want {
		t.Fatalf("user = %s %d %s", id, active, updated)
	}
	var created, updatedAt, value, settings, snapshot, generation string
	_ = target.QueryRow(`SELECT created_at, updated_at FROM secrets WHERE purpose='token'`).Scan(&created, &updatedAt)
	if created != "2" || updatedAt != "0" {
		t.Fatalf("infinities = %s / %s", created, updatedAt)
	}
	_ = target.QueryRow(`SELECT value, proxy_settings FROM dns_records`).Scan(&value, &settings)
	if value != `"192.0.2.9"` || settings != `{"a":null,"b":[1,2.5]}` {
		t.Fatalf("json columns = %s / %s", value, settings)
	}
	_ = target.QueryRow(`SELECT snapshot FROM config_revisions`).Scan(&snapshot)
	if !strings.Contains(snapshot, `"big":12345678901234567890`) {
		t.Fatalf("snapshot = %s", snapshot)
	}
	var big int64
	_ = target.QueryRow(`SELECT leadership_generation FROM installation_identity`).Scan(&big)
	generation = fmt.Sprint(big)
	if generation != "9007199254740993" {
		t.Fatalf("big int = %s", generation)
	}
	audits := auditRows(t, target)
	if len(audits) != 1 || audits[0].result != "success" || audits[0].actor.String != "40000000-0000-4000-8000-0000000000aa" {
		t.Fatalf("audit = %+v", audits)
	}
}

func TestSQLiteBackupImporterRejectsAPostgresEraBundleWithUnknownColumn(t *testing.T) {
	bundle := pgEraBundle(t, map[string]string{
		"secrets": `[{"id":"00000000-0000-0000-0000-000000000000","purpose":"__backup_master_key_marker__","ciphertext":"MARKER","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}]`,
		"zones":   `[{"id":"40000000-0000-4000-8000-0000000000CC","name":"pg.test","enabled":true,"pg_only_column":1,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`,
	})
	target := populatedTarget(t)
	before := dumpSQLiteTables(t, target, sqliteBundleTables, false)
	h := newImporterHarness(t, target, sqliteMasterKey)
	_, err := h.run(t, bundle, nil, false, "")
	if err == nil || !strings.Contains(err.Error(), `unknown column "pg_only_column"`) {
		t.Fatalf("err = %v", err)
	}
	if after := dumpSQLiteTables(t, target, sqliteBundleTables, false); !reflect.DeepEqual(after, before) {
		t.Fatal("database changed after rejecting the bundle")
	}
}
