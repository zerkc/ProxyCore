package dbimport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

func TestImportPostgresMasterKeyMismatchDoesNotWrite(t *testing.T) {
	pool := openImportTestPool(t)
	ctx := context.Background()
	dbMarker := fmt.Sprintf("dbimport-marker-%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `insert into installation_settings (id) values ($1)`, dbMarker); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	correctKey := testMasterKey(0x31)
	wrongKey := testMasterKey(0x32)
	ciphertext, err := secrets.EncryptSecret("fixture", correctKey)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	markerCiphertext, err := secrets.EncryptSecret("proxycore-backup-master-key", correctKey)
	if err != nil {
		t.Fatalf("EncryptSecret(marker): %v", err)
	}
	archive := newImportArchive(t, map[string][]byte{
		"db/secrets.json": []byte(`[ {"id":"00000000-0000-0000-0000-000000000000","purpose":"__backup_master_key_marker__","ciphertext":"` + markerCiphertext + `","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"},{"id":"11111111-1111-4111-8111-111111111111","purpose":"fixture","ciphertext":"` + ciphertext + `","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"} ]`),
	})

	engine, err := New(Options{
		Pool:            pool,
		MasterKeyBase64: wrongKey,
		EnvRestorePath:  t.TempDir() + "/env",
		CandidateRoot:   t.TempDir(),
		EnvMode:         "0600",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := engine.Import(ctx, archive, nil, false); !errors.Is(err, httpserver.ErrMasterKeyMismatch) {
		t.Fatalf("Import(wrong master key) = %v, want ErrMasterKeyMismatch", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from installation_settings where id = $1`, dbMarker).Scan(&count); err != nil {
		t.Fatalf("check marker: %v", err)
	}
	if count != 1 {
		t.Fatalf("marker count = %d, want 1; master-key validation wrote to DB", count)
	}
}

func TestImportPostgresHappyRoundTrip(t *testing.T) {
	pool := openImportTestPool(t)
	ctx := context.Background()
	masterKey := testMasterKey(0x41)
	ciphertext, err := secrets.EncryptSecret("round-trip", masterKey)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	marker, err := secrets.EncryptSecret("proxycore-backup-master-key", masterKey)
	if err != nil {
		t.Fatalf("EncryptSecret(marker): %v", err)
	}
	archive := newImportArchive(t, map[string][]byte{
		"db/users.json":   []byte(`[ {"id":"44444444-4444-4444-8444-444444444444","username":"backup-owner","password_hash":"scrypt$16384$8$1$aa$bb","role":"owner","active":true,"password_change_required":false,"created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"} ]`),
		"db/secrets.json": []byte(`[ {"id":"00000000-0000-0000-0000-000000000000","purpose":"__backup_master_key_marker__","ciphertext":"` + marker + `","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"},{"id":"22222222-2222-4222-8222-222222222222","purpose":"round-trip","ciphertext":"` + ciphertext + `","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"} ]`),
	})
	envPath := t.TempDir() + "/env"
	candidateRoot := t.TempDir()
	apply := &recordingApply{}
	audit := &recordingAudit{}
	engine, err := New(Options{
		Pool:            pool,
		MasterKeyBase64: masterKey,
		EnvRestorePath:  envPath,
		CandidateRoot:   candidateRoot,
		NewRevisionID:   "revision-round-trip",
		Apply:           apply,
		Audit:           audit,
		EnvMode:         "0600",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := engine.Import(WithActorID(ctx, "owner-id"), archive, nil, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.DryRun || !report.AppliedPostImport || len(report.Tables) != len(dbexport.DefaultConfigTables) {
		t.Fatalf("report = %+v, want committed import of all tables", report)
	}
	if apply.calls != 1 {
		t.Fatalf("apply calls = %d, want 1", apply.calls)
	}
	if audit.calls != 1 || audit.bundle == "" || audit.dryRun || !audit.success {
		t.Fatalf("audit = %+v, want one successful non-dry-run call", audit)
	}
	var purpose string
	if err := pool.QueryRow(ctx, `select purpose from secrets where id = '22222222-2222-4222-8222-222222222222'`).Scan(&purpose); err != nil {
		t.Fatalf("query restored secret: %v", err)
	}
	if purpose != "round-trip" {
		t.Fatalf("restored secret purpose = %q, want round-trip", purpose)
	}
	var username, role string
	if err := pool.QueryRow(ctx, `select username, role::text from users where id = '44444444-4444-4444-8444-444444444444'`).Scan(&username, &role); err != nil {
		t.Fatalf("query restored user: %v", err)
	}
	if username != "backup-owner" || role != "owner" {
		t.Fatalf("restored user = (%q, %q), want backup-owner/owner", username, role)
	}
	envData, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read restored env: %v", err)
	}
	if string(envData) != "PROXYCORE_MASTER_KEY_BASE64=fixture\n" {
		t.Fatalf("restored env = %q", envData)
	}
	certPath := filepath.Join(candidateRoot, "revision-round-trip", "nginx", "certs", "site.crt")
	certData, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read restored certificate: %v", err)
	}
	if string(certData) != "certificate" {
		t.Fatalf("restored certificate = %q", certData)
	}
}

func TestImportPostgresTamperedChecksumDoesNotWrite(t *testing.T) {
	pool := openImportTestPool(t)
	ctx := context.Background()
	marker := fmt.Sprintf("checksum-marker-%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `insert into installation_settings (id) values ($1)`, marker); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	archive := newImportArchive(t, nil)
	for index, file := range archive.files {
		if file.Path == "db/users.json" {
			archive.files[index] = archiveFile(file.Path, []byte(`[{}]`))
			break
		}
	}
	engine, err := New(Options{Pool: pool, MasterKeyBase64: testMasterKey(0x51), EnvRestorePath: t.TempDir() + "/env", CandidateRoot: t.TempDir(), EnvMode: "0600"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := engine.Import(ctx, archive, nil, false); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Import(tampered checksum) = %v, want ErrChecksumMismatch", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from installation_settings where id = $1`, marker).Scan(&count); err != nil {
		t.Fatalf("check marker: %v", err)
	}
	if count != 1 {
		t.Fatalf("marker count = %d, want 1", count)
	}
}

func TestImportPostgresPartialFailureRollsBackTransaction(t *testing.T) {
	pool := openImportTestPool(t)
	ctx := context.Background()
	marker := fmt.Sprintf("rollback-marker-%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `insert into installation_settings (id) values ($1)`, marker); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	archive := newImportArchive(t, map[string][]byte{
		// The users row is intentionally missing required values, so the
		// failure occurs after earlier tables have been truncated.
		"db/users.json": []byte(`[ {"id":"55555555-5555-4555-8555-555555555555"} ]`),
	})
	engine, err := New(Options{Pool: pool, MasterKeyBase64: testMasterKey(0x81), EnvRestorePath: t.TempDir() + "/env", CandidateRoot: t.TempDir(), EnvMode: "0600"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := engine.Import(ctx, archive, nil, false); err == nil {
		t.Fatal("Import(partial failure) succeeded")
	}
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from installation_settings where id = $1`, marker).Scan(&count); err != nil {
		t.Fatalf("check rollback marker: %v", err)
	}
	if count != 1 {
		t.Fatalf("marker count = %d, want 1 after rollback", count)
	}
}

func TestImportPostgresConcurrentUsesAdvisoryLock(t *testing.T) {
	pool := openImportTestPool(t)
	ctx := context.Background()
	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock transaction: %v", err)
	}
	defer lockTx.Rollback(ctx)
	if _, err := lockTx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended('backup.import', 0))`); err != nil {
		t.Fatalf("hold import lock: %v", err)
	}
	masterKey := testMasterKey(0x61)
	engine, err := New(Options{Pool: pool, MasterKeyBase64: masterKey, EnvRestorePath: t.TempDir() + "/env", CandidateRoot: t.TempDir(), EnvMode: "0600"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := engine.Import(ctx, newImportArchiveWithSecrets(t, masterKey), nil, false); !errors.Is(err, ErrImportAlreadyInProgress) {
		t.Fatalf("Import(held lock) = %v, want ErrImportAlreadyInProgress", err)
	}
}

func TestImportPostgresApplyFailureIsReportedAfterCommit(t *testing.T) {
	pool := openImportTestPool(t)
	ctx := context.Background()
	masterKey := testMasterKey(0x71)
	ciphertext, err := secrets.EncryptSecret("apply-failure", masterKey)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	marker, err := secrets.EncryptSecret("proxycore-backup-master-key", masterKey)
	if err != nil {
		t.Fatalf("EncryptSecret(marker): %v", err)
	}
	archive := newImportArchive(t, map[string][]byte{
		"db/secrets.json": []byte(`[ {"id":"00000000-0000-0000-0000-000000000000","purpose":"__backup_master_key_marker__","ciphertext":"` + marker + `","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"},{"id":"33333333-3333-4333-8333-333333333333","purpose":"apply-failure","ciphertext":"` + ciphertext + `","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"} ]`),
	})
	applyErr := errors.New("apply failed")
	apply := &recordingApply{err: applyErr}
	audit := &recordingAudit{}
	engine, err := New(Options{Pool: pool, MasterKeyBase64: masterKey, EnvRestorePath: t.TempDir() + "/env", CandidateRoot: t.TempDir(), Apply: apply, Audit: audit, EnvMode: "0600"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := engine.Import(ctx, archive, nil, false)
	if !errors.Is(err, applyErr) {
		t.Fatalf("Import(apply failure) = %v, want apply error", err)
	}
	if report.AppliedPostImport {
		t.Fatal("report marked apply successful after trigger failure")
	}
	if apply.calls != 1 || audit.calls != 1 || audit.success {
		t.Fatalf("apply=%d audit=%+v, want one failed audit after commit", apply.calls, audit)
	}
	var purpose string
	if err := pool.QueryRow(ctx, `select purpose from secrets where id = '33333333-3333-4333-8333-333333333333'`).Scan(&purpose); err != nil {
		t.Fatalf("query committed database row: %v", err)
	}
	if purpose != "apply-failure" {
		t.Fatalf("purpose = %q, want committed row despite post-commit apply failure", purpose)
	}
}

func openImportTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := dbexport.NewTestPoolFromEnvWithSchema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping database: %v", err)
	}
	if err := auth.NewPostgresStore(pool).EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure auth schema: %v", err)
	}
	if err := configuration.EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("ensure configuration schema: %v", err)
	}
	// The FB-2 allowlist includes these two durable tables; older test schemas
	// may not have their production migrations yet, so provide empty fixtures.
	for _, statement := range []string{
		`create table if not exists resolver_pools (id text primary key)`,
		`create table if not exists forwarding_rules (id text primary key)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("ensure FB-2 fixture table: %v", err)
		}
	}
	return pool
}

func newImportArchive(t *testing.T, overrides map[string][]byte) *testArchive {
	t.Helper()
	entries := make(map[string][]byte, len(dbexport.DefaultConfigTables)+4)
	for _, table := range dbexport.DefaultConfigTables {
		entries["db/"+table+".json"] = []byte("[]")
	}
	for path, data := range overrides {
		entries[path] = data
	}
	if data, ok := entries["db/secrets.json"]; ok {
		entries["db/secrets.json"] = addLegacyMasterKeyMarker(data)
	}
	entries["env/env"] = []byte("PROXYCORE_MASTER_KEY_BASE64=fixture\n")
	entries["certs/site.crt"] = []byte("certificate")

	files := make([]backupcore.NamedFile, 0, len(entries))
	for path, data := range entries {
		path, data := path, append([]byte(nil), data...)
		files = append(files, backupcore.NamedFile{Path: path, Size: int64(len(data)), Open: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		}})
	}
	checksums, err := backupcore.ComputeEntryChecksums(files)
	if err != nil {
		t.Fatalf("ComputeEntryChecksums: %v", err)
	}
	manifest := backupcore.Manifest{
		FormatVersion:   backupcore.FormatVersion,
		CreatedAt:       "2026-01-02T03:04:05Z",
		ExporterVersion: "test",
		InstallationID:  "installation-test",
		NodeID:          "node-test",
		Role:            "standalone-primary",
		EntryChecksums:  checksums,
		DBTables:        append([]string(nil), dbexport.DefaultConfigTables...),
		Certs:           []string{"certs/site.crt"},
		EnvPresent:      true,
	}
	manifestBytes, err := backupcore.MarshalManifest(manifest)
	if err != nil {
		t.Fatalf("MarshalManifest: %v", err)
	}
	entries["manifest.json"] = manifestBytes

	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	archiveFiles := make([]zipextract.File, 0, len(paths))
	for _, path := range paths {
		archiveFiles = append(archiveFiles, archiveFile(path, entries[path]))
	}
	return &testArchive{files: archiveFiles}
}

func addLegacyMasterKeyMarker(data []byte) []byte {
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil || len(rows) == 0 {
		return data
	}
	for _, row := range rows {
		if _, ok := row["purpose"]; ok {
			return data
		}
	}
	rows[0]["purpose"] = dbexport.BackupMasterKeyMarkerPurpose
	normalized, err := json.Marshal(rows)
	if err != nil {
		return data
	}
	return normalized
}

func testMasterKey(fill byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

type recordingApply struct {
	calls int
	err   error
}

func (a *recordingApply) Trigger(context.Context, string) error {
	a.calls++
	return a.err
}

type recordingAudit struct {
	calls   int
	bundle  string
	dryRun  bool
	success bool
	err     error
}

func (a *recordingAudit) EmitBackupImport(_ context.Context, _ string, bundle string, dryRun, success bool) error {
	a.calls++
	a.bundle, a.dryRun, a.success = bundle, dryRun, success
	return a.err
}
