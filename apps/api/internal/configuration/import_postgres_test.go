package configuration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

type importPostgresFixture struct {
	pool  *pgxpool.Pool
	admin *pgxpool.Pool
	store *Store
}

func newImportPostgresFixture(t *testing.T) *importPostgresFixture {
	t.Helper()
	url := os.Getenv("PNE5_DATABASE_URL")
	if url == "" {
		url = os.Getenv("PHASE2_DATABASE_URL")
	}
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		t.Skip("PNE5_DATABASE_URL, PHASE2_DATABASE_URL, or DATABASE_URL is not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	schema := "pne5_import_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+ident); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		admin.Close()
		t.Fatalf("parse postgres config: %v", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		admin.Close()
		t.Fatalf("connect isolated schema: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "drop schema if exists "+ident+" cascade")
		admin.Close()
	})
	if err := auth.NewPostgresStore(pool).EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure auth schema: %v", err)
	}
	if err := EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("ensure configuration schema: %v", err)
	}
	return &importPostgresFixture{pool: pool, admin: admin, store: New(pool, os.Getenv("PROXYCORE_MASTER_KEY_BASE64"), domain.Ingress{})}
}

func TestPostgresCreateApplyJobFromSnapshotPersistsAtomicImportTuple(t *testing.T) {
	fixture := newImportPostgresFixture(t)
	ctx := context.Background()
	env := importTestEnvelope(t)
	body, err := snapshot.Marshal(env)
	if err != nil {
		t.Fatalf("snapshot.Marshal: %v", err)
	}
	jobID, err := fixture.store.CreateApplyJobFromSnapshot(ctx, "import", body)
	if err != nil {
		t.Fatalf("CreateApplyJobFromSnapshot: %v", err)
	}
	var (
		revisionID, revisionSource, revisionPrimary, revisionNode, revisionHash string
		revisionSnapshot                                                        []byte
		revisionVersion, revisionReplication, revisionGeneration                int
		revisionApplied                                                         *time.Time
		jobRevision, jobSource, jobPrimary, jobNode, jobHash, correlation       string
		jobVersion, jobReplication, jobGeneration                               int
		jobStatus, jobTarget                                                    string
	)
	if err := fixture.pool.QueryRow(ctx, `
		select r.id::text, r.source::text, r.source_primary_id::text, r.source_node_id::text,
			r.snapshot::text, r.checksum, r.snapshot_content_hash, r.snapshot_version,
			r.replication_version, r.leadership_generation, r.applied_at,
			j.revision_id::text, j.source::text, j.source_primary_id::text, j.source_node_id::text,
			j.snapshot_content_hash, j.snapshot_version, j.replication_version, j.leadership_generation,
			j.correlation_id, j.status::text, j.target::text
		from apply_jobs j join config_revisions r on r.id = j.revision_id where j.id = $1
	`, jobID).Scan(
		&revisionID, &revisionSource, &revisionPrimary, &revisionNode, &revisionSnapshot, &revisionHash,
		new(string), &revisionVersion, &revisionReplication, &revisionGeneration, &revisionApplied,
		&jobRevision, &jobSource, &jobPrimary, &jobNode, &jobHash, &jobVersion, &jobReplication,
		&jobGeneration, &correlation, &jobStatus, &jobTarget,
	); err != nil {
		t.Fatalf("read imported tuple: %v", err)
	}
	checksum := sha256.Sum256(body)
	wantChecksum := hex.EncodeToString(checksum[:])
	if revisionID == "" || jobRevision != revisionID || jobSource != "import" || revisionSource != "import" ||
		revisionPrimary != env.Transient.SourcePrimaryID.String() || revisionNode != env.NodeLocal.NodeID.String() ||
		jobPrimary != revisionPrimary || jobNode != revisionNode || revisionHash != wantChecksum ||
		jobHash != env.ContentHash || jobStatus != "queued" || jobTarget != "combined" || correlation == "" ||
		revisionApplied != nil {
		t.Fatalf("revision=%s/%s source=%s/%s ids=%s/%s hashes=%s/%s version=%d/%d generation=%d status=%s target=%s correlation=%s applied=%v", revisionID, jobRevision, revisionSource, jobSource, revisionPrimary, revisionNode, revisionHash, jobHash, revisionVersion, jobVersion, jobGeneration, jobStatus, jobTarget, correlation, revisionApplied)
	}
	storedEnvelope, err := snapshot.Unmarshal(revisionSnapshot)
	if err != nil {
		t.Fatalf("unmarshal stored snapshot: %v", err)
	}
	canonicalStored, err := snapshot.Marshal(storedEnvelope)
	if err != nil {
		t.Fatalf("marshal stored snapshot: %v", err)
	}
	if !bytes.Equal(canonicalStored, body) || revisionVersion != int(env.Transient.SnapshotVersion) || revisionReplication != int(env.Transient.ReplicationVersion) ||
		jobVersion != revisionVersion || jobReplication != revisionReplication || jobGeneration != int(env.Transient.LeadershipGeneration) {
		t.Fatalf("stored snapshot/metadata snapshot=%s version=%d/%d replication=%d/%d generation=%d", revisionSnapshot, revisionVersion, jobVersion, revisionReplication, jobReplication, jobGeneration)
	}
	var revisionCorrelation string
	if err := fixture.pool.QueryRow(ctx, `select correlation_id from apply_jobs where id = $1`, jobID).Scan(&revisionCorrelation); err != nil {
		t.Fatalf("read correlation: %v", err)
	}
	if revisionCorrelation != correlation {
		t.Fatalf("correlation mismatch job=%q revision=%q", correlation, revisionCorrelation)
	}
}

func TestPostgresCreateApplyJobFromSnapshotRollsBackRevisionWhenJobInsertFails(t *testing.T) {
	fixture := newImportPostgresFixture(t)
	ctx := context.Background()
	if _, err := fixture.pool.Exec(ctx, `
		create or replace function reject_import_apply_job() returns trigger language plpgsql as $$
		begin
			if new.source::text = 'import' then raise exception 'test import apply rejection'; end if;
			return new;
		end $$;
	`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	if _, err := fixture.pool.Exec(ctx, `create trigger reject_import_apply_job before insert on apply_jobs for each row execute function reject_import_apply_job()`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fixture.pool.Exec(context.Background(), `drop trigger if exists reject_import_apply_job on apply_jobs`)
		_, _ = fixture.pool.Exec(context.Background(), `drop function if exists reject_import_apply_job()`)
	})
	body, err := snapshot.Marshal(importTestEnvelope(t))
	if err != nil {
		t.Fatalf("snapshot.Marshal: %v", err)
	}
	if _, err := fixture.store.CreateApplyJobFromSnapshot(ctx, "import", body); err == nil {
		t.Fatal("CreateApplyJobFromSnapshot unexpectedly succeeded")
	}
	var revisions, jobs int
	if err := fixture.pool.QueryRow(ctx, `select count(*) from config_revisions`).Scan(&revisions); err != nil {
		t.Fatalf("count revisions: %v", err)
	}
	if err := fixture.pool.QueryRow(ctx, `select count(*) from apply_jobs`).Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if revisions != 0 || jobs != 0 {
		t.Fatalf("transaction did not roll back revisions=%d jobs=%d", revisions, jobs)
	}
}

func TestPostgresArchiveAndTerminalReaderPreserveAuditEvidence(t *testing.T) {
	fixture := newImportPostgresFixture(t)
	ctx := context.Background()
	env := importTestEnvelope(t)
	body, err := snapshot.Marshal(env)
	if err != nil {
		t.Fatalf("snapshot.Marshal: %v", err)
	}
	archive := snapshot.StandaloneArchive{
		ID:           uuid.New(),
		CapturedAt:   time.Date(2026, 5, 10, 11, 12, 13, 0, time.UTC),
		ExpiresAt:    time.Date(2026, 6, 10, 11, 12, 13, 0, time.UTC),
		Reason:       "transition-to-node",
		SnapshotData: body,
	}
	if err := fixture.store.SaveStandaloneArchive(ctx, archive); err != nil {
		t.Fatalf("SaveStandaloneArchive: %v", err)
	}
	jobID, err := fixture.store.CreateApplyJobFromSnapshot(ctx, "import", body)
	if err != nil {
		t.Fatalf("CreateApplyJobFromSnapshot: %v", err)
	}
	queued, err := fixture.store.GetApplyJobTerminal(ctx, jobID)
	if err != nil || queued.Status != "queued" || queued.FinishedAt != nil {
		t.Fatalf("queued terminal=%+v err=%v", queued, err)
	}
	finished := time.Date(2026, 5, 10, 11, 12, 14, 0, time.UTC)
	failure := "apply failed"
	if _, err := fixture.pool.Exec(ctx, `update apply_jobs set status = 'failed', error_message = $2, finished_at = $3 where id = $1`, jobID, failure, finished); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	failed, err := fixture.store.GetApplyJobTerminal(ctx, jobID)
	if err != nil || failed.Status != ApplyTerminalStatusFailed || failed.FinishedAt == nil || !failed.FinishedAt.Equal(finished) || failed.FailureCode == nil || *failed.FailureCode != failure {
		t.Fatalf("failed terminal=%+v err=%v", failed, err)
	}
	var blob, reason string
	if err := fixture.pool.QueryRow(ctx, `select archive_blob, capture_reason from standalone_archives where id = $1`, archive.ID).Scan(&blob, &reason); err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if blob != string(body) || reason != archive.Reason {
		t.Fatalf("archive blob/reason=%q/%q", blob, reason)
	}
}
