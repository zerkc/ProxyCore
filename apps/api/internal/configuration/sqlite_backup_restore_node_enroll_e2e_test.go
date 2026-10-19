package configuration_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
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

// TestBackupRestoredPrimaryCannotEnrollNode_ReproducesNilIDMarkerSecret
// documents a KNOWN BUG as a characterization test: the real restore persists
// the backup master-key marker (nil id) into the primary's secrets table, the
// snapshot exporter replicates it, and the node import fails closed. When the
// bug is fixed, invert the final assertions (restore must not leak the marker,
// or the exporter must skip it, and the node import must succeed).
func TestBackupRestoredPrimaryCannotEnrollNode_ReproducesNilIDMarkerSecret(t *testing.T) {
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

	// (b) the snapshot export replicates it with uuid.Nil.
	env := e.Export(t)
	nilIndex := -1
	for i, s := range env.Replicated.Secrets {
		if s.ID == uuid.Nil {
			nilIndex = i
		}
	}
	if nilIndex < 0 {
		t.Fatal("(b) snapshot must contain a secret with uuid.Nil")
	}
	t.Logf("STEP 4 (b) snapshot exported %d secrets; nil-id secret at index %d", len(env.Replicated.Secrets), nilIndex)

	kek := e.DeliverKEK(t)
	defer kek.Destroy()
	result := replicationsnapshot.NewValidator(kek).Validate(context.Background(), env)
	var codes []string
	for _, issue := range result.Issues {
		codes = append(codes, issue.Code)
	}
	t.Logf("STEP 5 validator OK=%v issues=%v (on this branch the nil-id check is missing)", result.OK, codes)
	if !result.OK {
		t.Fatalf("validator unexpectedly rejects the snapshot (nil-id check may already exist): %v", codes)
	}

	// (c) the node import fails with the secret-id-missing sentinel.
	err := e.ImportOnNode(env)
	t.Logf("STEP 6 node import error: %v", err)
	if !errors.Is(err, configuration.ErrImportSecretIDMissing) {
		t.Fatalf("(c) err = %v, want ErrImportSecretIDMissing", err)
	}
	if want := fmt.Sprintf("(index %d)", nilIndex); !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(err.Error()) {
		t.Fatalf("(c) error must report %s: %q", want, err)
	}
}
