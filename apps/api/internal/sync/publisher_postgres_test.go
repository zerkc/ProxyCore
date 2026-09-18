package sync

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

type publisherPostgresFixture struct {
	pool       *pgxpool.Pool
	store      *configuration.PgPhase2Store
	publisher  *SnapshotPublisher
	credential NodeCredential
	latest     publicationCandidate
}

type publicationCandidate struct {
	SnapshotID string
	RevisionID string
	JobID      string
	PrimaryID  string
	Revision   int
	Hash       string
	Body       []byte
}

func newPublisherPostgresFixture(t *testing.T) *publisherPostgresFixture {
	t.Helper()
	url := os.Getenv("PNE4_DATABASE_URL")
	if url == "" {
		url = os.Getenv("PHASE2_DATABASE_URL")
	}
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		t.Skip("PNE4_DATABASE_URL, PHASE2_DATABASE_URL, or DATABASE_URL is not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	schema := "pne4_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	if err := configuration.EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("ensure configuration schema: %v", err)
	}
	primaryID, primaryNodeID := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into installation_identity (id, installation_id, node_id, role, leadership_generation, latest_known_generation)
		values ('default', $1, $2, 'primary', 7, 7)
	`, primaryID, primaryNodeID); err != nil {
		t.Fatalf("insert identity: %v", err)
	}
	masterKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyStore := cluster.NewStore(masterKey)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin cluster key transaction: %v", err)
	}
	material, err := keyStore.LoadOrCreate(ctx, tx, nil)
	if err != nil {
		t.Fatalf("create cluster key: %v", err)
	}
	keyID := material.ID.String()
	material.Destroy()
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit cluster key: %v", err)
	}
	if _, err := pool.Exec(ctx, `update installation_identity set cluster_key_id = $1 where id = 'default'`, keyID); err != nil {
		t.Fatalf("bind cluster key: %v", err)
	}
	credential, err := NewNodeCredential(uuid.NewString(), make([]byte, NodeCredentialSecretBytes))
	if err != nil {
		t.Fatalf("new node credential: %v", err)
	}
	nodeID, nodeInstallationID, attemptID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into node_credentials (id, node_id, credential_hash, hash_version)
		values ($1, $2, $3, $4)
	`, credential.ID(), nodeID, credential.Hash(), NodeCredentialHashVersion); err != nil {
		credential.Destroy()
		t.Fatalf("insert node credential: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into enrolled_nodes (node_id, installation_id, primary_id, credential_id, created_by_attempt_id)
		values ($1, $2, $3, $4, $5)
	`, nodeID, nodeInstallationID, primaryID, credential.ID(), attemptID); err != nil {
		credential.Destroy()
		t.Fatalf("insert enrolled node: %v", err)
	}
	store := configuration.NewPhase2StoreWithMasterKey(pool, masterKey)
	identityService := identity.NewService(identity.NewPgStore(pool))
	if _, err := identityService.Load(ctx); err != nil {
		credential.Destroy()
		t.Fatalf("load identity: %v", err)
	}
	fixture := &publisherPostgresFixture{pool: pool, store: store, credential: credential}
	fixture.publisher = NewSnapshotPublisher(store, identityService, SnapshotPublisherOptions{})
	fixture.latest = insertPublicationCandidate(t, pool, primaryID, 47, 7, "applied", "applied")
	return fixture
}

func insertPublicationCandidate(t *testing.T, pool *pgxpool.Pool, primaryID string, revision int, generation int64, snapshotStatus, jobStatus string) publicationCandidate {
	t.Helper()
	ctx := context.Background()
	revisionID, jobID, snapshotID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	var primaryNodeID string
	var clusterKeyID *uuid.UUID
	if err := pool.QueryRow(ctx, `select node_id::text, cluster_key_id from installation_identity where id = 'default'`).Scan(&primaryNodeID, &clusterKeyID); err != nil {
		t.Fatalf("read primary identity tuple: %v", err)
	}
	body, contentHash := canonicalPublicationEnvelope(t, domain.InstallationID(primaryID), domain.NodeID(primaryNodeID), domain.TopologyRolePrimary, domain.LeadershipGeneration(generation), revision, clusterKeyID)
	rawSum := sha256.Sum256(body)
	rawChecksum := hex.EncodeToString(rawSum[:])
	if _, err := pool.Exec(ctx, `
		insert into config_revisions (id, revision_number, checksum, snapshot, source, source_primary_id,
			snapshot_content_hash, snapshot_version, replication_version, leadership_generation, applied_at)
		values ($1, $2, $3, $4, 'ordinary', $5, $6, 1, 1, $7, case when $8 = 'applied' then now() else null end)
	`, revisionID, revision, rawChecksum, body, primaryID, contentHash, generation, jobStatus); err != nil {
		t.Fatalf("insert revision %d: %v", revision, err)
	}
	var storedBody string
	if err := pool.QueryRow(ctx, `select snapshot::text from config_revisions where id = $1`, revisionID).Scan(&storedBody); err != nil {
		t.Fatalf("read revision %d bytes: %v", revision, err)
	}
	hash := contentHash
	finished := any(nil)
	if jobStatus == "applied" {
		finished = time.Now().UTC()
	}
	if _, err := pool.Exec(ctx, `
		insert into apply_jobs (id, revision_id, target, status, correlation_id, source, source_primary_id,
			snapshot_content_hash, snapshot_version, replication_version, leadership_generation, finished_at)
		values ($1, $2, 'combined', $3, $4, 'ordinary', $5, $6, 1, 1, $7, $8)
	`, jobID, revisionID, jobStatus, uuid.NewString(), primaryID, hash, generation, finished); err != nil {
		t.Fatalf("insert job %d: %v", revision, err)
	}
	if _, err := pool.Exec(ctx, `
		insert into applied_snapshots (id, source_primary_id, leadership_generation, snapshot_version,
			replication_version, content_hash, revision_id, status, apply_job_id, applied_at)
		values ($1, $2, $3, 1, 1, $4, $5, $6, $7, now())
	`, snapshotID, primaryID, generation, hash, revisionID, snapshotStatus, jobID); err != nil {
		t.Fatalf("insert applied snapshot %d: %v", revision, err)
	}
	return publicationCandidate{SnapshotID: snapshotID, RevisionID: revisionID, JobID: jobID, PrimaryID: primaryID, Revision: revision, Hash: hash, Body: []byte(storedBody)}
}

func TestPostgresPublisherDeniesNewestUnpublishableWithoutFallback(t *testing.T) {
	fixture := newPublisherPostgresFixture(t)
	defer fixture.credential.Destroy()
	primaryID := fixture.latest.PrimaryID
	_ = insertPublicationCandidate(t, fixture.pool, primaryID, 41, 7, "applied", "applied")
	_ = insertPublicationCandidate(t, fixture.pool, primaryID, 48, 7, "pending", "queued")
	_ = insertPublicationCandidate(t, fixture.pool, primaryID, 49, 7, "rejected", "failed")
	result, err := fixture.publisher.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy()})
	if err != ErrSnapshotDenied || result.Current || len(result.Bytes) != 0 {
		t.Fatalf("newest-unpublishable result = %#v, err=%v", result, err)
	}
}

func TestPostgresPublisherSupportsAfterForNewestComplete(t *testing.T) {
	fixture := newPublisherPostgresFixture(t)
	defer fixture.credential.Destroy()
	first, err := fixture.publisher.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy()})
	if err != nil || first.Metadata.RevisionNumber != 47 || string(first.Bytes) != string(fixture.latest.Body) {
		t.Fatalf("latest result = %#v, err=%v", first, err)
	}
	current, err := fixture.publisher.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy(), After: fixture.latest.Hash})
	if err != nil || !current.Current || len(current.Bytes) != 0 {
		t.Fatalf("current result = %#v, err=%v", current, err)
	}
	behind, err := fixture.publisher.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy(), After: strings.Repeat("0", SnapshotContentHashHexLength)})
	if err != nil || behind.Current || behind.Metadata.RevisionNumber != 47 {
		t.Fatalf("behind result = %#v, err=%v", behind, err)
	}
}

func TestPostgresPublisherRejectsCorruptOrOversizedLatest(t *testing.T) {
	t.Run("hash mismatch", func(t *testing.T) {
		fixture := newPublisherPostgresFixture(t)
		defer fixture.credential.Destroy()
		if _, err := fixture.pool.Exec(context.Background(), `update config_revisions set snapshot = '{"corrupt":true}'::jsonb where id = $1`, fixture.latest.RevisionID); err != nil {
			t.Fatalf("corrupt snapshot: %v", err)
		}
		result, err := fixture.publisher.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy()})
		if err != ErrSnapshotDenied || result.Current || len(result.Bytes) != 0 {
			t.Fatalf("corrupt result = %#v, err=%v", result, err)
		}
	})
	t.Run("size boundary before body read", func(t *testing.T) {
		fixture := newPublisherPostgresFixture(t)
		defer fixture.credential.Destroy()
		exact := NewSnapshotPublisher(fixture.store, fixture.publisher.identity, SnapshotPublisherOptions{MaxBytes: len(fixture.latest.Body)})
		result, err := exact.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy()})
		if err != nil || len(result.Bytes) != len(fixture.latest.Body) {
			t.Fatalf("exact-size result = %#v, err=%v", result, err)
		}
		tooSmall := NewSnapshotPublisher(fixture.store, fixture.publisher.identity, SnapshotPublisherOptions{MaxBytes: len(fixture.latest.Body) - 1})
		result, err = tooSmall.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy()})
		if err != ErrSnapshotDenied || result.Current || len(result.Bytes) != 0 {
			t.Fatalf("max+1 result = %#v, err=%v", result, err)
		}
	})
}

func TestPostgresPublisherRequiresCompleteApplyProof(t *testing.T) {
	cases := []struct {
		name  string
		table string
		set   string
	}{
		{name: "discarded snapshot", table: "applied_snapshots", set: "discarded_at = now()"},
		{name: "revision unapplied", table: "config_revisions", set: "applied_at = null"},
		{name: "revision source", table: "config_revisions", set: "source = 'sync'"},
		{name: "revision source node", table: "config_revisions", set: "source_node_id = $2"},
		{name: "revision source revision", table: "config_revisions", set: "source_revision_id = $2"},
		{name: "revision source primary", table: "config_revisions", set: "source_primary_id = $2"},
		{name: "revision content hash", table: "config_revisions", set: "snapshot_content_hash = $2"},
		{name: "revision version", table: "config_revisions", set: "snapshot_version = 2"},
		{name: "revision generation", table: "config_revisions", set: "leadership_generation = 8"},
		{name: "job failed", table: "apply_jobs", set: "status = 'failed'"},
		{name: "job unfinished", table: "apply_jobs", set: "finished_at = null"},
		{name: "job source", table: "apply_jobs", set: "source = 'sync'"},
		{name: "job source node", table: "apply_jobs", set: "source_node_id = $2"},
		{name: "job source revision", table: "apply_jobs", set: "source_revision_id = $2"},
		{name: "job source primary", table: "apply_jobs", set: "source_primary_id = $2"},
		{name: "job content hash", table: "apply_jobs", set: "snapshot_content_hash = $2"},
		{name: "job version", table: "apply_jobs", set: "snapshot_version = 2"},
		{name: "job generation", table: "apply_jobs", set: "leadership_generation = 8"},
		{name: "job target", table: "apply_jobs", set: "target = 'coredns'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPublisherPostgresFixture(t)
			defer fixture.credential.Destroy()
			query := "update " + tc.table + " set " + tc.set + " where id = $1"
			wrongPrimary := uuid.NewString()
			id := fixture.latest.SnapshotID
			if tc.table == "config_revisions" {
				id = fixture.latest.RevisionID
			}
			if tc.table == "apply_jobs" {
				id = fixture.latest.JobID
			}
			var err error
			if strings.Contains(tc.set, "$2") {
				value := wrongPrimary
				if strings.Contains(tc.set, "source_revision_id") {
					value = fixture.latest.RevisionID
				}
				_, err = fixture.pool.Exec(context.Background(), query, id, value)
			} else {
				_, err = fixture.pool.Exec(context.Background(), query, id)
			}
			if err != nil {
				t.Fatalf("mutate %s: %v", tc.name, err)
			}
			result, err := fixture.publisher.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy()})
			if err != ErrSnapshotDenied || result.Current || len(result.Bytes) != 0 {
				t.Fatalf("incomplete proof result = %#v, err=%v", result, err)
			}
		})
	}
	t.Run("job revision mismatch", func(t *testing.T) {
		fixture := newPublisherPostgresFixture(t)
		defer fixture.credential.Destroy()
		other := insertPublicationCandidate(t, fixture.pool, fixture.latest.PrimaryID, 46, 7, "applied", "applied")
		if _, err := fixture.pool.Exec(context.Background(), `update apply_jobs set revision_id = $2 where id = $1`, fixture.latest.JobID, other.RevisionID); err != nil {
			t.Fatalf("mismatch job revision: %v", err)
		}
		result, err := fixture.publisher.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy()})
		if err != ErrSnapshotDenied || result.Current || len(result.Bytes) != 0 {
			t.Fatalf("job revision mismatch result = %#v, err=%v", result, err)
		}
	})
}

func TestPostgresPublisherRevisionSequenceRejectsAmbiguousTies(t *testing.T) {
	fixture := newPublisherPostgresFixture(t)
	defer fixture.credential.Destroy()
	_, err := fixture.pool.Exec(context.Background(), `
		insert into config_revisions (id, revision_number, checksum, snapshot, source, source_primary_id,
			snapshot_content_hash, snapshot_version, replication_version, leadership_generation, applied_at)
		values ($1, 47, $2, $3, 'ordinary', $4, $5, 1, 1, 7, now())
	`, uuid.NewString(), uuid.NewString(), fixture.latest.Body, fixture.latest.PrimaryID, fixture.latest.Hash)
	if err == nil {
		t.Fatal("expected the unique revision sequence to reject an ambiguous tie")
	}
}

func TestPostgresPublisherRejectsUnreadyRoleAndGeneration(t *testing.T) {
	cases := []struct {
		name  string
		query string
		args  []any
	}{
		{name: "node role", query: `update installation_identity set role = 'node' where id = 'default'`},
		{name: "stale primary role", query: `update installation_identity set role = 'stale-primary' where id = 'default'`},
		{name: "stale generation", query: `update installation_identity set latest_known_generation = 8 where id = 'default'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPublisherPostgresFixture(t)
			defer fixture.credential.Destroy()
			if _, err := fixture.pool.Exec(context.Background(), tc.query, tc.args...); err != nil {
				t.Fatalf("mutate identity: %v", err)
			}
			current := identity.NewService(identity.NewPgStore(fixture.pool))
			if _, err := current.Load(context.Background()); err != nil {
				t.Fatalf("reload identity: %v", err)
			}
			publisher := NewSnapshotPublisher(fixture.store, current, SnapshotPublisherOptions{})
			result, err := publisher.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy()})
			if err != ErrSnapshotDenied || result.Current || len(result.Bytes) != 0 {
				t.Fatalf("unready result = %#v, err=%v", result, err)
			}
		})
	}
}

func TestPostgresPublisherRestartReadsDurablePublication(t *testing.T) {
	fixture := newPublisherPostgresFixture(t)
	defer fixture.credential.Destroy()
	first, err := fixture.publisher.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy()})
	if err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	restartedIdentity := identity.NewService(identity.NewPgStore(fixture.pool))
	if _, err := restartedIdentity.Load(context.Background()); err != nil {
		t.Fatalf("restart identity load: %v", err)
	}
	restarted := NewSnapshotPublisher(fixture.store, restartedIdentity, SnapshotPublisherOptions{})
	second, err := restarted.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy()})
	if err != nil || second.Metadata.ContentHash != first.Metadata.ContentHash || string(second.Bytes) != string(first.Bytes) {
		t.Fatalf("restart result = %#v, err=%v", second, err)
	}
}
