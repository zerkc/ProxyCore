package configuration_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbimport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/hostread"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/httpexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

type staticIdentity struct{ install, node, role string }

func (s staticIdentity) InstallationIDString() string { return s.install }
func (s staticIdentity) NodeIDString() string         { return s.node }
func (s staticIdentity) RoleString() string           { return s.role }

type noopApply struct{ calls int }

func (a *noopApply) Trigger(context.Context, string) error { a.calls++; return nil }

// backupAndRestorePrimary exports a real backup of the primary and restores it
// into the same primary database with the real HTTP restore importer.
func backupAndRestorePrimary(t *testing.T, e *configuration.ExternalTwoInstalls) {
	t.Helper()
	dir := t.TempDir()
	exporter := &httpexport.SQLiteBackupExporter{
		DB:              e.PrimaryDB,
		Identity:        staticIdentity{e.PrimaryInstallID, e.PrimaryNodeID, e.PrimaryRole},
		Version:         "e2e-test",
		MasterKeyBase64: e.PrimaryMaster,
		HostConfig: hostread.Config{
			EnvPath:       filepath.Join(dir, "missing.env"),
			CandidateRoot: filepath.Join(dir, "candidates"),
		},
	}
	var bundle bytes.Buffer
	if _, err := exporter.Export(context.Background(), &bundle, nil); err != nil {
		t.Fatalf("STEP backup export failed: %v", err)
	}
	t.Logf("STEP 1 backup export OK (%d bytes)", bundle.Len())

	apply := &noopApply{}
	importer := &httpexport.SQLiteBackupImporter{
		DB:              e.PrimaryDB,
		MasterKeyBase64: e.PrimaryMaster,
		EnvRestorePath:  filepath.Join(dir, "restored.env"),
		CandidateRoot:   filepath.Join(dir, "restored-candidates"),
		EnvMode:         "0600",
		Apply:           apply,
		Audit:           &dbimport.SQLiteAuditEmitter{DB: e.PrimaryDB},
		NewRevisionID:   uuid.NewString(),
	}
	if _, err := importer.Import(context.Background(), bytes.NewReader(bundle.Bytes()), int64(bundle.Len()), nil, false); err != nil {
		t.Fatalf("STEP restore failed: %v", err)
	}
	t.Logf("STEP 2 restore into primary OK (apply triggered %d time)", apply.calls)
}

func TestPrimaryWithoutBackupRestoreEnrollsNode_Control(t *testing.T) {
	e := configuration.NewExternalTwoInstalls(t)
	env := e.Export(t)
	kek := e.DeliverKEK(t)
	defer kek.Destroy()
	if result := replicationsnapshot.NewValidator(kek).Validate(context.Background(), env); !result.OK {
		t.Fatalf("validation failed: %+v", result.Issues)
	}
	if err := e.ImportOnNode(env); err != nil {
		t.Fatalf("control import failed: %v", err)
	}
	var markers int
	if err := e.PrimaryDB.QueryRow(`select count(*) from secrets where purpose = ?`, dbexport.BackupMasterKeyMarkerPurpose).Scan(&markers); err != nil || markers != 0 {
		t.Fatalf("control primary must hold no marker row, got %d, %v", markers, err)
	}
	t.Log("CONTROL: no restore -> no marker row -> node import succeeds")
}

// TestBackupRestoredPrimaryEnrollsNode_SkipsBackupMarkerSecret is the regression
// test for the production failure "import secrets: secret is incomplete". The
// real restore intentionally keeps the backup master-key marker (reserved nil
// id) in the primary's secrets table; the snapshot export must not replicate it,
// so a node can still enroll after the primary was restored from a backup.
func TestBackupRestoredPrimaryEnrollsNode_SkipsBackupMarkerSecret(t *testing.T) {
	e := configuration.NewExternalTwoInstalls(t)
	backupAndRestorePrimary(t, e)

	// (a) the restore wrote exactly one nil-id marker row into secrets.
	var markers int
	if err := e.PrimaryDB.QueryRow(`select count(*) from secrets where id = ? and purpose = ?`,
		uuid.Nil.String(), dbexport.BackupMasterKeyMarkerPurpose).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if markers != 1 {
		t.Fatalf("(a) restored primary must hold exactly one nil-id marker row, got %d", markers)
	}
	var total int
	_ = e.PrimaryDB.QueryRow(`select count(*) from secrets`).Scan(&total)
	t.Logf("STEP 3 (a) primary secrets after restore: %d rows, including the nil-id marker row", total)

	// (b) the snapshot export must NOT replicate the bundle-only marker.
	env := e.Export(t)
	for i, secret := range env.Replicated.Secrets {
		if secret.ID == uuid.Nil || secret.Purpose == dbexport.BackupMasterKeyMarkerPurpose {
			t.Fatalf("(b) snapshot must not carry the backup marker, found it at index %d", i)
		}
	}
	t.Logf("STEP 4 (b) snapshot exported %d secrets; no backup marker among them", len(env.Replicated.Secrets))

	kek := e.DeliverKEK(t)
	defer kek.Destroy()
	result := replicationsnapshot.NewValidator(kek).Validate(context.Background(), env)
	var codes []string
	for _, issue := range result.Issues {
		codes = append(codes, issue.Code)
	}
	t.Logf("STEP 5 validator OK=%v issues=%v", result.OK, codes)
	if !result.OK {
		t.Fatalf("validator rejects the snapshot: %v", codes)
	}

	// (c) the node imports it.
	if err := e.ImportOnNode(env); err != nil {
		t.Fatalf("(c) node import after backup restore failed: %v", err)
	}
	t.Logf("STEP 6 node import OK")
}
