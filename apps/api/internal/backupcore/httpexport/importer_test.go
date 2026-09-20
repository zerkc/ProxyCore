package httpexport

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipwriter"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

func TestBackupImporterBodyLimit(t *testing.T) {
	_, err := (&BackupImporter{BodyLimit: 3}).Import(context.Background(), strings.NewReader("1234"), 4, nil, true)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Import oversized body = %v, want ErrBodyTooLarge", err)
	}
}

func TestBackupImporterRoundTripValidation(t *testing.T) {
	masterKey := testImporterMasterKey()
	passphrase := []byte("bundle-passphrase")
	bundle := newImporterTestBundle(t, masterKey, passphrase)
	importer := &BackupImporter{
		MasterKeyBase64: masterKey,
		EnvRestorePath:  filepath.Join(t.TempDir(), "env"),
		CandidateRoot:   t.TempDir(),
		EnvMode:         "0600",
		BodyLimit:       int64(len(bundle)),
	}

	report, err := importer.Import(context.Background(), bytes.NewReader(bundle), int64(len(bundle)), passphrase, true)
	if err != nil {
		t.Fatalf("Import(dry-run): %v", err)
	}
	if !report.DryRun || report.AppliedPostImport {
		t.Fatalf("dry-run report = %+v", report)
	}

	if _, err := importer.Import(context.Background(), bytes.NewReader(bundle), int64(len(bundle)), []byte("wrong"), true); !errors.Is(err, zipextract.ErrPassphraseMismatch) {
		t.Fatalf("Import(wrong passphrase) = %v, want ErrPassphraseMismatch", err)
	}
}

func TestBackupImporterPostgresRoundTrip(t *testing.T) {
	fixture := newHTTPExportFixture(t)
	exporter := &BackupExporter{
		Pool:     fixture.pool,
		Identity: staticIdentitySource{},
		Version:  "test-exporter",
	}
	var rawBundle bytes.Buffer
	if _, err := exporter.Export(context.Background(), &rawBundle, nil); err != nil {
		t.Fatalf("Export(raw): %v", err)
	}
	var encryptedBundle bytes.Buffer
	passphrase := []byte("bundle-passphrase")
	if _, err := exporter.Export(context.Background(), &encryptedBundle, passphrase); err != nil {
		t.Fatalf("Export(encrypted): %v", err)
	}

	restoreRoot := t.TempDir()
	importer := &BackupImporter{
		Pool:            fixture.pool,
		MasterKeyBase64: fixture.masterKey,
		EnvRestorePath:  filepath.Join(restoreRoot, ".env"),
		CandidateRoot:   filepath.Join(restoreRoot, "candidates"),
		EnvMode:         "0600",
		NewRevisionID:   "httpexport-roundtrip-revision",
		BodyLimit:       16 << 20,
	}

	report, err := importer.Import(context.Background(), bytes.NewReader(rawBundle.Bytes()), int64(rawBundle.Len()), nil, true)
	if err != nil {
		t.Fatalf("Import(raw dry-run): %v", err)
	}
	if !report.DryRun || report.AppliedPostImport {
		t.Fatalf("raw dry-run report = %+v", report)
	}
	if _, err := importer.Import(context.Background(), bytes.NewReader(encryptedBundle.Bytes()), int64(encryptedBundle.Len()), []byte("wrong"), true); !errors.Is(err, zipextract.ErrPassphraseMismatch) {
		t.Fatalf("Import(wrong passphrase) = %v, want ErrPassphraseMismatch", err)
	}

	wrongKeyImporter := *importer
	wrongKeyImporter.MasterKeyBase64 = testImporterMasterKeyWithByte(0x29)
	if _, err := wrongKeyImporter.Import(context.Background(), bytes.NewReader(rawBundle.Bytes()), int64(rawBundle.Len()), nil, true); !errors.Is(err, httpserver.ErrMasterKeyMismatch) {
		t.Fatalf("Import(wrong master key) = %v, want ErrMasterKeyMismatch", err)
	}

	report, err = importer.Import(context.Background(), bytes.NewReader(rawBundle.Bytes()), int64(rawBundle.Len()), nil, false)
	if err != nil {
		t.Fatalf("Import(raw real): %v", err)
	}
	if report.DryRun || report.AppliedPostImport {
		t.Fatalf("raw real report = %+v", report)
	}
	envData, err := os.ReadFile(importer.EnvRestorePath)
	if err != nil {
		t.Fatalf("read restored env: %v", err)
	}
	if string(envData) != "PROXYCORE_MASTER_KEY_BASE64="+fixture.masterKey+"\n" {
		t.Fatalf("restored env = %q", envData)
	}
}

func TestBackupImporterUsesRequestActorID(t *testing.T) {
	masterKey := testImporterMasterKey()
	bundle := newImporterTestBundle(t, masterKey, nil)
	audit := &recordingImportAudit{}
	importer := &BackupImporter{
		MasterKeyBase64: masterKey,
		Audit:           audit,
		EnvMode:         "0600",
	}

	if _, err := importer.ImportWithActorID(context.Background(), bytes.NewReader(bundle), int64(len(bundle)), nil, true, "owner-request"); err != nil {
		t.Fatalf("ImportWithActorID: %v", err)
	}
	if audit.calls != 1 || audit.actorID != "owner-request" || !audit.dryRun || !audit.success {
		t.Fatalf("audit = %+v, want one successful dry-run for owner-request", audit)
	}
}

func TestBackupImporterMasterKeyMismatchAndRealPath(t *testing.T) {
	correctKey := testImporterMasterKey()
	bundle := newImporterTestBundle(t, correctKey, nil)
	importer := &BackupImporter{
		MasterKeyBase64: testImporterMasterKeyWithByte(0x29),
		EnvRestorePath:  filepath.Join(t.TempDir(), "env"),
		CandidateRoot:   t.TempDir(),
		EnvMode:         "0600",
	}

	if _, err := importer.Import(context.Background(), bytes.NewReader(bundle), int64(len(bundle)), nil, true); !errors.Is(err, httpserver.ErrMasterKeyMismatch) {
		t.Fatalf("Import(wrong master key) = %v, want ErrMasterKeyMismatch", err)
	}

	importer.MasterKeyBase64 = correctKey
	report, err := importer.Import(context.Background(), bytes.NewReader(bundle), int64(len(bundle)), nil, false)
	if err == nil {
		t.Fatal("Import(real) unexpectedly succeeded without a database pool")
	}
	if report.DryRun || report.AppliedPostImport {
		t.Fatalf("real import report = %+v", report)
	}
}

func newImporterTestBundle(t *testing.T, masterKey string, passphrase []byte) []byte {
	t.Helper()
	ciphertext, err := secrets.EncryptSecret("fixture", masterKey)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	entries := map[string][]byte{
		"env/env":         []byte("PROXYCORE_MASTER_KEY_BASE64=fixture\n"),
		"db/secrets.json": []byte(`[{"ciphertext":"` + ciphertext + `"}]`),
	}
	for _, table := range dbexport.DefaultConfigTables {
		path := "db/" + table + ".json"
		if _, ok := entries[path]; !ok {
			entries[path] = []byte("[]")
		}
	}

	named := make([]backupcore.NamedFile, 0, len(entries))
	paths := make([]string, 0, len(entries))
	for path, data := range entries {
		path, data := path, append([]byte(nil), data...)
		paths = append(paths, path)
		named = append(named, backupcore.NamedFile{
			Path: path,
			Size: int64(len(data)),
			Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
		})
	}
	checksums, err := backupcore.ComputeEntryChecksums(named)
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
		EnvPresent:      true,
	}
	manifestBytes, err := backupcore.MarshalManifest(manifest)
	if err != nil {
		t.Fatalf("MarshalManifest: %v", err)
	}

	var bundle bytes.Buffer
	writer, err := zipwriter.OpenEncrypted(&bundle, passphrase)
	if err != nil {
		t.Fatalf("OpenEncrypted: %v", err)
	}
	sort.Strings(paths)
	for _, path := range paths {
		data := entries[path]
		if err := writer.Add(path, int64(len(data)), func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		}); err != nil {
			t.Fatalf("Add(%q): %v", path, err)
		}
	}
	if err := writer.Add("manifest.json", int64(len(manifestBytes)), func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(manifestBytes)), nil
	}); err != nil {
		t.Fatalf("Add(manifest): %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return bundle.Bytes()
}

type recordingImportAudit struct {
	calls   int
	actorID string
	dryRun  bool
	success bool
}

func (a *recordingImportAudit) EmitBackupImport(_ context.Context, actorID, _ string, dryRun, success bool) error {
	a.calls++
	a.actorID = actorID
	a.dryRun = dryRun
	a.success = success
	return nil
}

func testImporterMasterKey() string {
	return testImporterMasterKeyWithByte(0x42)
}

func testImporterMasterKeyWithByte(value byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{value}, 32))
}
