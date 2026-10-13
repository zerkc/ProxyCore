package configuration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func newBackendFixture(t *testing.T) (*recordFixture, *SQLiteConfigurationBackend) {
	t.Helper()
	f := newRecordFixture(t)
	return f, NewSQLiteConfigurationBackend(f.db, settingsWriter(f.db), domain.Ingress{IPv4: "192.0.2.1"}, f.secrets)
}

func TestSQLiteConfigurationBackendStatusEmptyInstallation(t *testing.T) {
	db, _ := enqueueFixture(t)
	b := NewSQLiteConfigurationBackend(db, settingsWriter(db), domain.Ingress{}, nil)
	got, err := b.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredRevision != nil || got.AppliedRevision != nil {
		t.Fatalf("revisions: %+v %+v", got.DesiredRevision, got.AppliedRevision)
	}
	if got.Jobs == nil || len(got.Jobs) != 0 || got.Zones == nil || got.Streams == nil || got.Certificates == nil {
		t.Fatalf("collections must be non-nil and empty: %+v", got)
	}
	if got.Settings.DefaultPool == nil {
		t.Fatal("default pool should be reported")
	}
}

func TestSQLiteConfigurationBackendStatusReportsRevisionsJobsAndCollections(t *testing.T) {
	f, b := newBackendFixture(t)
	ctx := context.Background()
	applied, err := b.CreateApplyJob(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	settingsExec(t, f.db, `update config_revisions set applied_at=? where id=?`, certKey(t, 5, 0), applied.RevisionID)
	settingsExec(t, f.db, `update installation_settings set current_applied_revision_id=?`, applied.RevisionID)
	f.seedRecord(t, "00000000-0000-4000-8000-0000000000a1", recordZoneID, "www", "192.0.2.9")
	if _, err := b.AddStream(ctx, streamRoute(2222, "tcp")); err != nil {
		t.Fatal(err)
	}
	next, err := b.CreateApplyJob(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	// The fixed writer clock gives both jobs the same created_at; order them.
	settingsExec(t, f.db, `update apply_jobs set created_at=? where id=?`, certKey(t, 10, 0), applied.Job.ID)
	settingsExec(t, f.db, `update apply_jobs set created_at=? where id=?`, certKey(t, 20, 0), next.Job.ID)
	got, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredRevision == nil || got.DesiredRevision.ID != next.RevisionID || got.DesiredRevision.RevisionNumber != 2 || got.DesiredRevision.AppliedAt != nil {
		t.Fatalf("desired: %+v", got.DesiredRevision)
	}
	if got.AppliedRevision == nil || got.AppliedRevision.ID != applied.RevisionID || got.AppliedRevision.AppliedAt == nil || got.AppliedRevision.CreatedAt.IsZero() {
		t.Fatalf("applied: %+v", got.AppliedRevision)
	}
	if _, err := json.Marshal(got.DesiredRevision.Snapshot); err != nil || got.DesiredRevision.Snapshot == nil {
		t.Fatalf("snapshot not decoded: %v %v", got.DesiredRevision.Snapshot, err)
	}
	if len(got.Jobs) != 2 || got.Jobs[0].ID != next.Job.ID || got.Jobs[1].ID != applied.Job.ID {
		t.Fatalf("jobs newest first: %+v", got.Jobs)
	}
	if got.Jobs[0].Status != "queued" || got.Jobs[0].CreatedAt.IsZero() {
		t.Fatalf("job: %+v", got.Jobs[0])
	}
	if len(got.Zones) != 2 || len(got.Streams) != 1 || len(got.Certificates) != 1 || got.Settings.Ingress.IPv4 != "192.0.2.1" {
		t.Fatalf("collections: zones=%d streams=%d certs=%d settings=%+v", len(got.Zones), len(got.Streams), len(got.Certificates), got.Settings)
	}
}

func TestSQLiteConfigurationBackendStatusDanglingRevisionIsNil(t *testing.T) {
	db, _ := enqueueFixture(t)
	b := NewSQLiteConfigurationBackend(db, settingsWriter(db), domain.Ingress{}, nil)
	settingsExec(t, db, `update installation_settings set current_desired_revision_id='00000000-0000-4000-8000-0000000000f1'`)
	got, err := b.Status(context.Background())
	if err != nil || got.DesiredRevision != nil {
		t.Fatalf("PG returns a nil revision for a missing row: %+v %v", got.DesiredRevision, err)
	}
}

const errRevisionID = "00000000-0000-4000-8000-0000000000f0"

func TestSQLiteConfigurationBackendStatusErrors(t *testing.T) {
	db, _ := enqueueFixture(t)
	b := NewSQLiteConfigurationBackend(db, settingsWriter(db), domain.Ingress{}, nil)
	settingsExec(t, db, `insert into config_revisions (id,revision_number,checksum,snapshot,created_at) values (?,1,'c','{}','2')`, errRevisionID)
	settingsExec(t, db, `update installation_settings set current_desired_revision_id=?`, errRevisionID)
	if _, err := b.Status(context.Background()); err == nil {
		t.Fatal("nonfinite revision clock must fail")
	}
	settingsExec(t, db, `update installation_settings set current_desired_revision_id=NULL`)
	settingsExec(t, db, `insert into apply_jobs (id,revision_id,target,status,correlation_id,created_at) values (?,?,'combined','queued','c1','2')`, "00000000-0000-4000-8000-0000000000f2", errRevisionID)
	if _, err := b.Status(context.Background()); err == nil {
		t.Fatal("nonfinite job clock must fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Status(ctx); err == nil {
		t.Fatal("canceled context must fail")
	}
}

func TestSQLiteConfigurationBackendCreateApplyJobAndForceApply(t *testing.T) {
	f, b := newBackendFixture(t)
	ctx := context.Background()
	const actor = "aaaaaaaa-0000-4000-8000-0000000000a1"
	settingsExec(t, f.db, `insert into users (id,username,password_hash,role,active,created_at,updated_at) values (?, 'u', 'x', 'owner', 1, '0', '0')`, actor)
	first, err := b.CreateApplyJob(ctx, actor)
	if err != nil {
		t.Fatal(err)
	}
	if first.Job.Status != "queued" || first.Job.Target != "combined" || first.Job.RevisionID != first.RevisionID || first.Job.ActorUserID == nil || *first.Job.ActorUserID != actor {
		t.Fatalf("job: %+v", first.Job)
	}
	forced, err := b.ForceApply(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if forced.RevisionID != first.RevisionID || forced.Job.ID == first.Job.ID || forced.Job.ActorUserID != nil {
		t.Fatalf("force must behave like CreateApplyJob (same revision, new job): %+v %+v", first, forced)
	}
	if n := enqueueCount(t, f.db, "apply_jobs"); n != 2 {
		t.Fatal("jobs", n)
	}
	var current string
	if err := f.db.QueryRow(`select current_desired_revision_id from installation_settings`).Scan(&current); err != nil || current != first.RevisionID {
		t.Fatal(current, err)
	}
}

func TestSQLiteConfigurationBackendCreateApplyJobErrors(t *testing.T) {
	db, _ := enqueueFixture(t)
	b := NewSQLiteConfigurationBackend(db, settingsWriter(db), domain.Ingress{}, nil)
	settingsExec(t, db, `update installation_settings set default_resolver_pool=NULL`)
	for name, call := range map[string]func() error{
		"create": func() error { _, err := b.CreateApplyJob(context.Background(), ""); return err },
		"force":  func() error { _, err := b.ForceApply(context.Background(), ""); return err },
	} {
		if err := call(); err == nil || err.Error() != "Configure a default resolver pool before apply" {
			t.Fatal(name, err)
		}
	}
	if enqueueCount(t, db, "config_revisions") != 0 || enqueueCount(t, db, "apply_jobs") != 0 {
		t.Fatal("failed enqueue must leave no rows")
	}
	settingsExec(t, db, `update installation_settings set default_resolver_pool=?`, enqueuePool)
	if _, err := b.CreateApplyJob(context.Background(), "not-a-uuid"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("malformed actor: %v", err)
	}
	if enqueueCount(t, db, "apply_jobs") != 0 {
		t.Fatal("malformed actor left a job")
	}
}

func TestSQLiteConfigurationBackendComposesSharedDBAndWriter(t *testing.T) {
	f, b := newBackendFixture(t)
	ctx := context.Background()
	zone, err := b.CreateZone(ctx, "Composed.Test", "")
	if err != nil {
		t.Fatal(err)
	}
	if zone.Value.Name != "composed.test" || zone.Apply.Job.Status != "queued" {
		t.Fatalf("zone: %+v", zone)
	}
	read, err := b.GetZone(ctx, zone.Value.ID)
	if err != nil || read.ID != zone.Value.ID {
		t.Fatal(read, err)
	}
	if _, err := b.AddRecord(ctx, zone.Value.ID, recordA("www", "192.0.2.7"), ""); err != nil {
		t.Fatal(err)
	}
	if zones, err := b.ListZones(ctx); err != nil || len(zones) != 3 {
		t.Fatal(len(zones), err)
	}
	if _, err := b.UpdateEnrollmentHostnames(ctx, []string{"enroll.example.test"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := b.GetEnrollmentHostnames(ctx)
	if err != nil || len(cfg.Hostnames) != 1 {
		t.Fatalf("%+v %v", cfg, err)
	}
	cert, err := b.IssueCertificate(ctx, CertificateIssueInput{Hostnames: []string{"composed.test"}, Issuer: "self-signed", Challenge: "none"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if certs, err := b.ListCertificates(ctx); err != nil || len(certs) != 2 {
		t.Fatal(len(certs), err)
	}
	if pem, err := b.EnsureInternalCACertificatePEM(ctx); err != nil || !strings.Contains(pem, "BEGIN CERTIFICATE") {
		t.Fatal(err)
	}
	if err := b.DeleteCertificate(ctx, cert.ID); err != nil {
		t.Fatal(err)
	}
	status, err := b.Status(ctx)
	if err != nil || len(status.Zones) != 3 || len(status.Jobs) < 2 {
		t.Fatalf("status must see writes made through the shared db: %+v %v", status, err)
	}
	if enqueueCount(t, f.db, "apply_jobs") != len(status.Jobs) {
		t.Fatal("job count mismatch")
	}
}

func TestSQLiteConfigurationBackendNilSecretsAndTypedNil(t *testing.T) {
	db, _ := enqueueFixture(t)
	var typedNil *secrets.SQLiteStore
	for _, store := range []sqliteInternalCASecrets{nil, typedNil} {
		b := NewSQLiteConfigurationBackend(db, settingsWriter(db), domain.Ingress{}, store)
		if _, err := b.Status(context.Background()); err != nil {
			t.Fatal(err)
		}
		// Basic Auth needs a secret store; without one it fails instead of panicking.
		if _, err := b.AddRecord(context.Background(), recordZoneID, recordProxied(map[string]any{"username": "u", "password": "p"}), ""); err == nil {
			t.Fatal("expected an error without a secret store")
		}
	}
}

func TestSQLiteConfigurationBackendClosedWriter(t *testing.T) {
	db, _ := enqueueFixture(t)
	b := NewSQLiteConfigurationBackend(db, sqlitedb.NewWriter(db, nil), domain.Ingress{}, nil)
	db.Close()
	if _, err := b.CreateApplyJob(context.Background(), ""); err == nil {
		t.Fatal("closed database must fail")
	}
	if _, err := b.Status(context.Background()); err == nil {
		t.Fatal("closed database must fail")
	}
}
