package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
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
	securesecrets "github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

type producerPostgresFixture struct {
	pool       *pgxpool.Pool
	store      *configuration.PgPhase2Store
	publisher  *SnapshotPublisher
	credential NodeCredential
	masterKey  string
	primaryID  string
	clusterKey uuid.UUID
	desired    domain.ConfigurationSnapshot
}

func newProducerPostgresFixture(t *testing.T) *producerPostgresFixture {
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
	schema := "pne4b_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
		t.Fatalf("insert primary identity: %v", err)
	}
	masterKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x17}, 32))
	keyStore := cluster.NewStore(masterKey)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin cluster key transaction: %v", err)
	}
	material, err := keyStore.LoadOrCreate(ctx, tx, nil)
	if err != nil {
		t.Fatalf("create cluster key: %v", err)
	}
	clusterKey := material.ID
	material.Destroy()
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit cluster key: %v", err)
	}
	if _, err := pool.Exec(ctx, `update installation_identity set cluster_key_id = $1 where id = 'default'`, clusterKey); err != nil {
		t.Fatalf("bind cluster key: %v", err)
	}

	credential, err := NewNodeCredential(uuid.NewString(), bytes.Repeat([]byte{0x21}, NodeCredentialSecretBytes))
	if err != nil {
		t.Fatalf("new node credential: %v", err)
	}
	nodeID, nodeInstallationID := uuid.NewString(), uuid.NewString()
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
	`, nodeID, nodeInstallationID, primaryID, credential.ID(), uuid.NewString()); err != nil {
		credential.Destroy()
		t.Fatalf("insert enrolled node: %v", err)
	}

	ownerID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into users (id, username, password_hash, role, active)
		values ($1, 'owner', 'scrypt$16384$8$1$hash$hash', 'owner', true)
	`, ownerID); err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	ciphertext, err := securesecrets.EncryptSecret("producer-plaintext-marker", masterKey)
	if err != nil {
		t.Fatalf("encrypt test secret: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into secrets (id, purpose, ciphertext) values ($1, 'producer-test', $2)`, uuid.NewString(), ciphertext); err != nil {
		t.Fatalf("insert secret: %v", err)
	}

	desired := domain.ConfigurationSnapshot{
		Settings: domain.Settings{
			Ingress:     domain.Ingress{IPv4: "192.0.2.10"},
			DefaultPool: &domain.ResolverPool{ID: "default", Endpoints: []domain.ResolverEndpoint{{Host: "192.0.2.53", Port: 53}}},
		},
		Zones: []domain.ZoneState{}, Streams: []domain.StreamRoute{}, Certificates: []domain.CertificateStatus{},
	}
	store := configuration.NewPhase2StoreWithMasterKey(pool, masterKey)
	identityService := identity.NewService(identity.NewPgStore(pool))
	if _, err := identityService.Load(ctx); err != nil {
		credential.Destroy()
		t.Fatalf("load identity: %v", err)
	}
	return &producerPostgresFixture{
		pool: pool, store: store, publisher: NewSnapshotPublisher(store, identityService, SnapshotPublisherOptions{}),
		credential: credential, masterKey: masterKey, primaryID: primaryID, clusterKey: clusterKey, desired: desired,
	}
}

func (f *producerPostgresFixture) insertTerminalApply(t *testing.T, revisionNumber int, desired domain.ConfigurationSnapshot, finishedAt time.Time) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	revisionID, jobID := uuid.NewString(), uuid.NewString()
	body := []byte(domain.StableStringify(desired))
	sum := sha256.Sum256(body)
	if _, err := f.pool.Exec(ctx, `
		insert into config_revisions (id, revision_number, checksum, snapshot, source, applied_at)
		values ($1, $2, $3, $4, 'ordinary', $5)
	`, revisionID, revisionNumber, hex.EncodeToString(sum[:]), body, finishedAt.Add(-time.Second)); err != nil {
		t.Fatalf("insert desired revision: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		insert into apply_jobs (id, revision_id, target, status, correlation_id, source, finished_at)
		values ($1, $2, 'combined', 'applied', $3, 'ordinary', $4)
	`, jobID, revisionID, uuid.NewString(), finishedAt); err != nil {
		t.Fatalf("insert terminal apply job: %v", err)
	}
	var before string
	if err := f.pool.QueryRow(ctx, `select snapshot::text from config_revisions where id = $1`, revisionID).Scan(&before); err != nil {
		t.Fatalf("read desired revision: %v", err)
	}
	return revisionID, jobID, before
}

func TestPostgresProducerPublishesAndReaderReturnsExactBody(t *testing.T) {
	fixture := newProducerPostgresFixture(t)
	defer fixture.credential.Destroy()
	finishedAt := time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC)
	revisionID, jobID, desiredBefore := fixture.insertTerminalApply(t, 47, fixture.desired, finishedAt)
	producer := NewCanonicalSnapshotProducer(fixture.store, CanonicalSnapshotProducerOptions{})
	if published, err := producer.Reconcile(context.Background()); err != nil || published != 1 {
		t.Fatalf("first reconcile published=%d err=%v", published, err)
	}
	var body []byte
	var source, hash string
	var revision, appliedJob string
	if err := fixture.pool.QueryRow(context.Background(), `
		select snapshot_body, source_primary_id::text, content_hash, revision_id::text, apply_job_id::text
		from applied_snapshots where apply_job_id = $1
	`, jobID).Scan(&body, &source, &hash, &revision, &appliedJob); err != nil {
		t.Fatalf("read published row: %v", err)
	}
	if len(body) == 0 || source != fixture.primaryID || revision != revisionID || appliedJob != jobID {
		t.Fatalf("published tuple source=%s revision=%s job=%s body=%d", source, revision, appliedJob, len(body))
	}
	var desiredAfter string
	if err := fixture.pool.QueryRow(context.Background(), `select snapshot::text from config_revisions where id = $1`, revisionID).Scan(&desiredAfter); err != nil {
		t.Fatalf("read desired revision after publication: %v", err)
	}
	if desiredAfter != desiredBefore {
		t.Fatalf("desired revision changed from %q to %q", desiredBefore, desiredAfter)
	}
	result, err := fixture.publisher.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy()})
	if err != nil || !bytes.Equal(result.Bytes, body) || result.Metadata.ContentHash != hash {
		t.Fatalf("publisher result bytes=%d hash=%s err=%v", len(result.Bytes), result.Metadata.ContentHash, err)
	}
	second, err := NewSnapshotPublisher(fixture.store, identityServiceForProducerTest(t, fixture.pool), SnapshotPublisherOptions{}).Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy()})
	if err != nil || !bytes.Equal(second.Bytes, body) {
		t.Fatalf("restart publisher bytes=%d err=%v", len(second.Bytes), err)
	}
	if published, err := producer.Reconcile(context.Background()); err != nil || published != 0 {
		t.Fatalf("idempotent reconcile published=%d err=%v", published, err)
	}
	assertProducerPlaintextAbsent(t, fixture, "producer-plaintext-marker")

	newKey := rotateProducerClusterKey(t, fixture)
	fixture.clusterKey = newKey
	desiredNext := fixture.desired
	desiredNext.Settings.RetentionMaxAgeDays = 9
	finishedNext := finishedAt.Add(time.Minute)
	_, nextJobID, _ := fixture.insertTerminalApply(t, 48, desiredNext, finishedNext)
	failedProducer := NewCanonicalSnapshotProducer(fixture.store, CanonicalSnapshotProducerOptions{
		ExporterFactory: func(domain.ConfigurationSnapshot, configuration.CanonicalSnapshotTransaction) CanonicalSnapshotExporter {
			return producerExporterFunc(func(context.Context, replicationsnapshot.ExporterInput) (replicationsnapshot.Envelope, error) {
				return replicationsnapshot.Envelope{}, errors.New("export temporarily unavailable")
			})
		},
	})
	if err := failedProducer.Produce(context.Background(), nextJobID); err == nil {
		t.Fatal("failed exporter unexpectedly published a snapshot")
	}
	var failedRows int
	if err := fixture.pool.QueryRow(context.Background(), `select count(*) from applied_snapshots where apply_job_id = $1`, nextJobID).Scan(&failedRows); err != nil {
		t.Fatalf("count failed publication rows: %v", err)
	}
	if failedRows != 0 {
		t.Fatalf("failed publication rows=%d, want 0", failedRows)
	}
	if err := producer.Produce(context.Background(), nextJobID); err != nil {
		t.Fatalf("recovered Produce: %v", err)
	}
	restarted := NewSnapshotPublisher(fixture.store, identityServiceForProducerTest(t, fixture.pool), SnapshotPublisherOptions{})
	next, err := restarted.Publish(context.Background(), SnapshotRequest{Credential: fixture.credential.BearerCopy()})
	if err != nil || next.Metadata.LeadershipGeneration != 8 || len(next.Bytes) == 0 || bytes.Equal(next.Bytes, body) {
		t.Fatalf("rotated publication generation=%d bytes=%d err=%v", next.Metadata.LeadershipGeneration, len(next.Bytes), err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `update installation_identity set role = 'node' where id = 'default'`); err != nil {
		t.Fatalf("demote producer identity: %v", err)
	}
	if err := producer.Produce(context.Background(), nextJobID); err != ErrCanonicalSnapshotProducerNotReady {
		t.Fatalf("node producer error=%v, want %v", err, ErrCanonicalSnapshotProducerNotReady)
	}
	assertProducerPlaintextAbsent(t, fixture, "producer-plaintext-marker")
}

func rotateProducerClusterKey(t *testing.T, fixture *producerPostgresFixture) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if _, err := fixture.pool.Exec(ctx, `update cluster_keys set retired_at = now() where id = $1`, fixture.clusterKey); err != nil {
		t.Fatalf("retire cluster key: %v", err)
	}
	keyStore := cluster.NewStore(fixture.masterKey)
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin key rotation: %v", err)
	}
	material, err := keyStore.LoadOrCreate(ctx, tx, nil)
	if err != nil {
		t.Fatalf("create rotated key: %v", err)
	}
	id := material.ID
	material.Destroy()
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit rotated key: %v", err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		update installation_identity
		set role = 'primary', leadership_generation = 8, latest_known_generation = 8, cluster_key_id = $1
		where id = 'default'
	`, id); err != nil {
		t.Fatalf("bind rotated key: %v", err)
	}
	return id
}

func identityServiceForProducerTest(t *testing.T, pool *pgxpool.Pool) *identity.Service {
	t.Helper()
	service := identity.NewService(identity.NewPgStore(pool))
	if _, err := service.Load(context.Background()); err != nil {
		t.Fatalf("reload identity: %v", err)
	}
	return service
}

func assertProducerPlaintextAbsent(t *testing.T, fixture *producerPostgresFixture, marker string) {
	t.Helper()
	rows, err := fixture.pool.Query(context.Background(), `select ciphertext from secrets`)
	if err != nil {
		t.Fatalf("read ciphertexts: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan ciphertext: %v", err)
		}
		if strings.Contains(value, marker) {
			t.Fatalf("plaintext marker persisted in secret ciphertext")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("secret rows: %v", err)
	}
	var bodies string
	if err := fixture.pool.QueryRow(context.Background(), `select coalesce(string_agg(snapshot::text, '|'), '') from config_revisions`).Scan(&bodies); err != nil {
		t.Fatalf("read revision bodies: %v", err)
	}
	if strings.Contains(bodies, marker) {
		t.Fatalf("plaintext marker persisted in desired revisions")
	}
}
