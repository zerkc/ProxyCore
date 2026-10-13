package httpexport

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/hostread"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

const sqliteRevisionID = "40000000-0000-4000-8000-000000000001"

var sqliteMasterKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x24}, 32))

type sqliteBundleFixture struct {
	db       *sql.DB
	exporter *SQLiteBackupExporter
}

func newSQLiteBundleFixture(t *testing.T) *sqliteBundleFixture {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlitedb.Open(filepath.Join(dir, "config.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	key := func(seconds int64) string {
		encoded, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: seconds, Nanos: 250})
		if err != nil || encoded == nil {
			t.Fatalf("encode timestamp: %v", err)
		}
		return *encoded
	}
	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("exec %.50q: %v", query, err)
		}
	}
	mustExec(`INSERT INTO users (id,username,password_hash,role,created_at,updated_at) VALUES (?,?,?,?,?,?)`,
		"40000000-0000-4000-8000-000000000002", "owner", "hash", "owner", key(1700000000), "2")
	mustExec(`INSERT INTO secrets (id,purpose,ciphertext,created_at,updated_at) VALUES (?,?,?,?,?)`,
		"40000000-0000-4000-8000-000000000003", "token", "ciphertext", key(1700000000), key(1700000001))
	mustExec(`INSERT INTO zones (id,name,created_at,updated_at) VALUES (?,?,?,?)`,
		"40000000-0000-4000-8000-000000000004", "example.test", "0", key(1700000001))
	mustExec(`INSERT INTO config_revisions (id,revision_number,checksum,snapshot,created_at,applied_at) VALUES (?,?,?,?,?,?)`,
		sqliteRevisionID, 1, "sum", `{"zones":[]}`, key(1700000000), key(1700000002))

	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("PROXYCORE_TEST=1\n"), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	certDir := filepath.Join(dir, "candidates", sqliteRevisionID, "nginx", "certs")
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		t.Fatalf("mkdir certs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "site.crt"), []byte("dummy-cert"), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	return &sqliteBundleFixture{db: db, exporter: &SQLiteBackupExporter{
		DB:              db,
		Identity:        staticIdentitySource{},
		Version:         "test-sqlite-exporter",
		Now:             func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
		MasterKeyBase64: sqliteMasterKey,
		HostConfig: hostread.Config{
			EnvPath:       envPath,
			CandidateRoot: filepath.Join(dir, "candidates"),
		},
	}}
}

func openSQLiteBundle(t *testing.T, bundle []byte, passphrase []byte) map[string]zipextract.File {
	t.Helper()
	archive, err := zipextract.Open(context.Background(), bytes.NewReader(bundle), passphrase)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	t.Cleanup(func() { archive.Close() })
	files, err := archive.Files(context.Background())
	if err != nil {
		t.Fatalf("list bundle: %v", err)
	}
	byPath := make(map[string]zipextract.File, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}
	return byPath
}

func readBundleFile(t *testing.T, files map[string]zipextract.File, path string) []byte {
	t.Helper()
	file, ok := files[path]
	if !ok {
		t.Fatalf("bundle has no %q", path)
	}
	reader, err := file.Open()
	if err != nil {
		t.Fatalf("open %q: %v", path, err)
	}
	data, readErr := io.ReadAll(reader)
	if err := errors.Join(readErr, reader.Close()); err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return data
}

// The helpers below mirror the DB-free importer-side validation in
// backupcore/dbimport (readManifest, verifyArchiveChecksums, verifyMasterKey,
// readTablePayloads); those are unexported, so the contract is re-asserted here.
func validateBundleLikeImporter(t *testing.T, files map[string]zipextract.File, manifestDigest string) backupcore.Manifest {
	t.Helper()
	manifestBytes := readBundleFile(t, files, "manifest.json")
	if manifestDigestForTest(manifestBytes) != manifestDigest {
		t.Fatalf("manifest digest mismatch")
	}
	var manifest backupcore.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if err := backupcore.Validate(manifest); err != nil {
		t.Fatalf("manifest invalid: %v", err)
	}

	named := make([]backupcore.NamedFile, 0, len(files))
	for path, file := range files {
		if path != "manifest.json" {
			named = append(named, backupcore.NamedFile{Path: path, Size: file.Size, Open: file.Open})
		}
	}
	computed, err := backupcore.ComputeEntryChecksums(named)
	if err != nil {
		t.Fatalf("checksums: %v", err)
	}
	if !reflect.DeepEqual(computed, manifest.EntryChecksums) {
		t.Fatalf("entry checksums differ\n got: %v\nwant: %v", computed, manifest.EntryChecksums)
	}

	// verifyMasterKey: exactly one marker row that decrypts with the key.
	var secretRows []map[string]any
	if err := json.Unmarshal(readBundleFile(t, files, "db/secrets.json"), &secretRows); err != nil {
		t.Fatalf("decode secrets: %v", err)
	}
	markers := 0
	for _, row := range secretRows {
		if row["purpose"] != dbexport.BackupMasterKeyMarkerPurpose {
			continue
		}
		markers++
		plaintext, err := secrets.DecryptSecret(row["ciphertext"].(string), sqliteMasterKey)
		if err != nil || plaintext != dbexport.BackupMasterKeyMarkerPlaintext {
			t.Fatalf("marker decrypt = %q, %v", plaintext, err)
		}
	}
	if markers != 1 {
		t.Fatalf("marker rows = %d, want 1", markers)
	}

	// readTablePayloads: all 16 tables listed, each entry a JSON array of
	// objects, and no unlisted db/*.json entry.
	if len(manifest.DBTables) != len(dbexport.DefaultConfigTables) {
		t.Fatalf("manifest tables = %v", manifest.DBTables)
	}
	listed := map[string]bool{}
	for _, table := range manifest.DBTables {
		listed[table] = true
		decoder := json.NewDecoder(bytes.NewReader(readBundleFile(t, files, "db/"+table+".json")))
		decoder.UseNumber()
		var rows []map[string]any
		if err := decoder.Decode(&rows); err != nil {
			t.Fatalf("table %s: %v", table, err)
		}
		if manifest.Counts[table] != len(rows) {
			t.Fatalf("table %s count=%d rows=%d", table, manifest.Counts[table], len(rows))
		}
	}
	for path := range files {
		if strings.HasPrefix(path, "db/") && !listed[strings.TrimSuffix(strings.TrimPrefix(path, "db/"), ".json")] {
			t.Fatalf("unlisted db entry %q", path)
		}
	}
	for _, table := range dbexport.DefaultConfigTables {
		if !listed[table] {
			t.Fatalf("manifest misses table %q", table)
		}
	}
	return manifest
}

func TestSQLiteBackupExporterRawBundlePassesImporterValidation(t *testing.T) {
	fixture := newSQLiteBundleFixture(t)
	var bundle bytes.Buffer
	digest, err := fixture.exporter.Export(context.Background(), &bundle, nil)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	files := openSQLiteBundle(t, bundle.Bytes(), nil)
	manifest := validateBundleLikeImporter(t, files, digest)

	wantPaths := []string{"env/env", "certs/site.crt", "manifest.json"}
	for _, table := range dbexport.SQLiteExportOrder {
		wantPaths = append(wantPaths, "db/"+table+".json")
	}
	gotPaths := make([]string, 0, len(files))
	for path := range files {
		gotPaths = append(gotPaths, path)
	}
	sort.Strings(wantPaths)
	sort.Strings(gotPaths)
	if len(manifest.Certs) != 1 {
		t.Fatalf("manifest certs = %v (bundle paths %v)", manifest.Certs, gotPaths)
	}
	// Certificate entry names come from hostread; assert the rest of the layout.
	for _, want := range wantPaths {
		if want == "certs/site.crt" {
			continue
		}
		if _, ok := files[want]; !ok {
			t.Errorf("bundle misses %q (have %v)", want, gotPaths)
		}
	}
	if len(files) != len(wantPaths) {
		t.Errorf("bundle has %d entries, want %d: %v", len(files), len(wantPaths), gotPaths)
	}
	if !manifest.EnvPresent || manifest.CreatedAt != "2026-01-02T03:04:05Z" || manifest.ExporterVersion != "test-sqlite-exporter" {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.InstallationID != "installation-test" || manifest.NodeID != "node-test" || manifest.Role != "standalone-primary" {
		t.Fatalf("manifest identity = %+v", manifest)
	}
	// Manifest table order follows the exporter order.
	if !reflect.DeepEqual(manifest.DBTables, dbexport.SQLiteExportOrder) {
		t.Fatalf("manifest tables = %v", manifest.DBTables)
	}
	if manifest.Counts["secrets"] != 2 || manifest.Counts["users"] != 1 || manifest.Counts["zones"] != 1 || manifest.Counts["node_state"] != 0 {
		t.Fatalf("counts = %v", manifest.Counts)
	}
	if got := string(readBundleFile(t, files, "db/users.json")); !strings.Contains(got, `"updated_at":"infinity"`) {
		t.Fatalf("users = %s", got)
	}
}

func TestSQLiteBackupExporterEncryptedBundleRoundTrips(t *testing.T) {
	fixture := newSQLiteBundleFixture(t)
	passphrase := []byte("correct horse battery staple")
	var bundle bytes.Buffer
	digest, err := fixture.exporter.Export(context.Background(), &bundle, passphrase)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if bytes.Contains(bundle.Bytes(), []byte("example.test")) {
		t.Fatal("encrypted bundle leaks plaintext rows")
	}
	if _, err := zipextract.Open(context.Background(), bytes.NewReader(bundle.Bytes()), []byte("wrong")); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
	validateBundleLikeImporter(t, openSQLiteBundle(t, bundle.Bytes(), passphrase), digest)
}

func TestSQLiteBackupExporterWithoutAppliedRevisionOmitsCerts(t *testing.T) {
	fixture := newSQLiteBundleFixture(t)
	if _, err := fixture.db.Exec(`UPDATE config_revisions SET applied_at = NULL`); err != nil {
		t.Fatalf("clear applied: %v", err)
	}
	var bundle bytes.Buffer
	digest, err := fixture.exporter.Export(context.Background(), &bundle, nil)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	manifest := validateBundleLikeImporter(t, openSQLiteBundle(t, bundle.Bytes(), nil), digest)
	if len(manifest.Certs) != 0 {
		t.Fatalf("certs = %v", manifest.Certs)
	}
}

func TestSQLiteBackupExporterRejectsInvalidSetup(t *testing.T) {
	fixture := newSQLiteBundleFixture(t)
	export := func(e *SQLiteBackupExporter) (string, int, error) {
		var out bytes.Buffer
		digest, err := e.Export(context.Background(), &out, nil)
		return digest, out.Len(), err
	}
	if _, _, err := export(nil); err == nil {
		t.Error("nil exporter accepted")
	}
	noDB := *fixture.exporter
	noDB.DB = nil
	if _, n, err := export(&noDB); err == nil || n != 0 {
		t.Errorf("nil db: err=%v bytes=%d", err, n)
	}
	noIdentity := *fixture.exporter
	noIdentity.Identity = nil
	if _, n, err := export(&noIdentity); err == nil || n != 0 {
		t.Errorf("nil identity: err=%v bytes=%d", err, n)
	}
	if _, err := fixture.exporter.Export(context.Background(), nil, nil); err == nil {
		t.Error("nil writer accepted")
	}
	noEnv := *fixture.exporter
	noEnv.HostConfig.EnvPath = filepath.Join(t.TempDir(), "missing.env")
	if _, _, err := export(&noEnv); err == nil {
		t.Error("missing env file accepted")
	}
	badKey := *fixture.exporter
	badKey.MasterKeyBase64 = "not-a-key"
	if _, _, err := export(&badKey); err == nil {
		t.Error("invalid master key accepted")
	}
}

func TestSQLiteBackupExporterFailsOnClosedDatabaseAndCanceledContext(t *testing.T) {
	fixture := newSQLiteBundleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fixture.exporter.Export(ctx, &bytes.Buffer{}, nil); err == nil {
		t.Fatal("canceled context accepted")
	}
	fixture.db.Close()
	if _, err := fixture.exporter.Export(context.Background(), &bytes.Buffer{}, nil); err == nil {
		t.Fatal("closed database accepted")
	}
}
