package dbimport

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

func TestNewValidatesEnvModeAndAllocatesRevision(t *testing.T) {
	for _, mode := range []string{"", "0640", "600", "0666"} {
		t.Run("reject_"+mode, func(t *testing.T) {
			if _, err := New(Options{EnvMode: mode}); !errors.Is(err, ErrInvalidEnvMode) {
				t.Fatalf("New(%q) error = %v, want ErrInvalidEnvMode", mode, err)
			}
		})
	}

	engine, err := New(Options{
		EnvMode: "0600",
		Now:     func() time.Time { return time.Unix(1, 0).UTC() },
	})
	if err != nil {
		t.Fatalf("New(valid options): %v", err)
	}
	if engine.revisionID == "" {
		t.Fatal("New did not allocate a revision ID")
	}
	if engine.now == nil || !engine.now().Equal(time.Unix(1, 0).UTC()) {
		t.Fatal("New did not retain the supplied clock")
	}
}

func TestVerifyMasterKeyEngineUsesSentinelCiphertext(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	masterKey := encodeMasterKeyForTest(t, key)
	ciphertext, err := secrets.EncryptSecret("proxycore-backup-master-key", masterKey)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	files := map[string]zipextract.File{
		"db/secrets.json": archiveFile("db/secrets.json", []byte(`[{"id":"00000000-0000-0000-0000-000000000000","purpose":"__backup_master_key_marker__","ciphertext":"`+ciphertext+`","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}]`)),
	}

	if err := verifyMasterKey(context.Background(), files, masterKey); err != nil {
		t.Fatalf("verifyMasterKey(correct key): %v", err)
	}
	if err := verifyMasterKey(context.Background(), files, encodeMasterKeyForTest(t, bytes.Repeat([]byte{0x24}, 32))); !errors.Is(err, httpserver.ErrMasterKeyMismatch) {
		t.Fatalf("verifyMasterKey(wrong key) = %v, want ErrMasterKeyMismatch", err)
	}

	files["db/secrets.json"] = archiveFile("db/secrets.json", []byte(`[{"purpose":"__backup_master_key_marker__","ciphertext":"not-a-ciphertext"}]`))
	if err := verifyMasterKey(context.Background(), files, masterKey); !errors.Is(err, httpserver.ErrMasterKeyMismatch) {
		t.Fatalf("verifyMasterKey(malformed ciphertext) = %v, want ErrMasterKeyMismatch", err)
	}
}

func TestImportDryRunDoesNotWriteFilesOrApply(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "env")
	candidateRoot := t.TempDir()
	apply := &recordingApply{}
	audit := &recordingAudit{}
	masterKey := encodeMasterKeyForTest(t, bytes.Repeat([]byte{0x42}, 32))
	engine, err := New(Options{
		MasterKeyBase64: masterKey,
		EnvRestorePath:  envPath,
		CandidateRoot:   candidateRoot,
		Apply:           apply,
		Audit:           audit,
		EnvMode:         "0644",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := engine.Import(context.Background(), newImportArchiveWithSecrets(t, masterKey), nil, true)
	if err != nil {
		t.Fatalf("Import(dry-run): %v", err)
	}
	if !report.DryRun || len(report.Tables) != 16 || report.AppliedPostImport {
		t.Fatalf("dry-run report = %+v", report)
	}
	if apply.calls != 0 {
		t.Fatalf("apply calls = %d, want 0", apply.calls)
	}
	if audit.calls != 1 || !audit.dryRun || !audit.success {
		t.Fatalf("audit = %+v, want one successful dry-run call", audit)
	}
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Fatalf("dry-run env stat error = %v, want file absent", err)
	}
	if _, err := os.Stat(filepath.Join(candidateRoot, "revision-round-trip")); !os.IsNotExist(err) {
		t.Fatalf("dry-run candidate stat error = %v, want no candidate directory", err)
	}
}

func TestImportRejectsMissingManifestAsCorruptArchive(t *testing.T) {
	engine, err := New(Options{EnvMode: "0600"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = engine.Import(context.Background(), &testArchive{files: []zipextract.File{archiveFile("db/secrets.json", []byte("[]"))}}, nil, true)
	if !errors.Is(err, zipextract.ErrCorruptArchive) {
		t.Fatalf("Import(missing manifest) = %v, want ErrCorruptArchive", err)
	}
}

func newImportArchiveWithSecrets(t *testing.T, masterKey string) *testArchive {
	t.Helper()
	ciphertext, err := secrets.EncryptSecret("proxycore-backup-master-key", masterKey)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	return newImportArchive(t, map[string][]byte{
		"db/secrets.json": []byte(`[{"id":"00000000-0000-0000-0000-000000000000","purpose":"__backup_master_key_marker__","ciphertext":"` + ciphertext + `","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}]`),
	})
}

func encodeMasterKeyForTest(t *testing.T, key []byte) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(key)
}

func archiveFile(path string, data []byte) zipextract.File {
	return zipextract.File{
		Path: path,
		Size: int64(len(data)),
		Open: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		},
	}
}

type testArchive struct {
	files  []zipextract.File
	closed bool
}

func (a *testArchive) Files(context.Context) ([]zipextract.File, error) {
	return append([]zipextract.File(nil), a.files...), nil
}

func (a *testArchive) Close() error {
	a.closed = true
	return nil
}

var _ zipextract.Archive = (*testArchive)(nil)

func TestApplyDoesNotMarkApplied_FlagTrue(t *testing.T) {
	pool := openImportTestPool(t)
	masterKey := testMasterKey(0x91)
	apply := &recordingApply{}
	engine, err := New(Options{
		Pool:                    pool,
		MasterKeyBase64:         masterKey,
		EnvRestorePath:          filepath.Join(t.TempDir(), "env"),
		CandidateRoot:           t.TempDir(),
		Apply:                   apply,
		ApplyDoesNotMarkApplied: true,
		EnvMode:                 "0600",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	report, err := engine.Import(WithActorID(context.Background(), "owner-flag-true"), newApplyTestArchive(t, masterKey), nil, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if apply.calls != 1 {
		t.Fatalf("apply calls = %d, want 1", apply.calls)
	}
	if report.AppliedPostImport {
		t.Fatal("AppliedPostImport = true, want false when marker suppression is enabled")
	}
}

func TestApplyDoesNotMarkApplied_FlagFalse(t *testing.T) {
	pool := openImportTestPool(t)
	masterKey := testMasterKey(0x92)
	apply := &recordingApply{}
	engine, err := New(Options{
		Pool:            pool,
		MasterKeyBase64: masterKey,
		EnvRestorePath:  filepath.Join(t.TempDir(), "env"),
		CandidateRoot:   t.TempDir(),
		Apply:           apply,
		EnvMode:         "0600",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	report, err := engine.Import(context.Background(), newApplyTestArchive(t, masterKey), nil, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if apply.calls != 1 {
		t.Fatalf("apply calls = %d, want 1", apply.calls)
	}
	if !report.AppliedPostImport {
		t.Fatal("AppliedPostImport = false, want true by default")
	}
}

func TestApplyNil_AlwaysFalse(t *testing.T) {
	for _, flag := range []bool{false, true} {
		t.Run(fmt.Sprintf("flag_%t", flag), func(t *testing.T) {
			pool := openImportTestPool(t)
			masterKey := testMasterKey(byte(0x93 + btoi(flag)))
			engine, err := New(Options{
				Pool:                    pool,
				MasterKeyBase64:         masterKey,
				EnvRestorePath:          filepath.Join(t.TempDir(), "env"),
				CandidateRoot:           t.TempDir(),
				ApplyDoesNotMarkApplied: flag,
				EnvMode:                 "0600",
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			report, err := engine.Import(context.Background(), newApplyTestArchive(t, masterKey), nil, false)
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if report.AppliedPostImport {
				t.Fatal("AppliedPostImport = true with nil Apply trigger")
			}
		})
	}
}

func newApplyTestArchive(t *testing.T, masterKey string) *testArchive {
	t.Helper()
	ciphertext, err := secrets.EncryptSecret("apply-flag", masterKey)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	marker, err := secrets.EncryptSecret("proxycore-backup-master-key", masterKey)
	if err != nil {
		t.Fatalf("EncryptSecret(marker): %v", err)
	}
	return newImportArchive(t, map[string][]byte{
		"db/secrets.json": []byte(`[{"id":"00000000-0000-0000-0000-000000000000","purpose":"__backup_master_key_marker__","ciphertext":"` + marker + `","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"},{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","purpose":"apply-flag","ciphertext":"` + ciphertext + `","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}]`),
	})
}

func btoi(value bool) byte {
	if value {
		return 1
	}
	return 0
}
