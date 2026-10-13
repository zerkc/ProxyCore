package dbimport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"sort"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
)

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
