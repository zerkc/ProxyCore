package configuration

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
)

type phase2AcknowledgementFixture struct {
	pool           *pgxpool.Pool
	admin          *pgxpool.Pool
	store          *PgPhase2Store
	credentialID   string
	credentialHash string
	masterKey      string
	primaryID      uuid.UUID
	nodeID         uuid.UUID
	keyID          uuid.UUID
	tokenID        uuid.UUID
	attemptID      uuid.UUID
	revisionID     uuid.UUID
	jobID          uuid.UUID
	contentHash    string
	appliedAt      time.Time
}

func newPhase2AcknowledgementFixture(t *testing.T) *phase2AcknowledgementFixture {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("PNE8_DATABASE_URL"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("PHASE2_DATABASE_URL"))
	}
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	if dsn == "" {
		t.Skip("PNE8_DATABASE_URL, PHASE2_DATABASE_URL, or DATABASE_URL is not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skip("cannot connect to database: " + err.Error())
	}
	schema := "pne8_ack_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+ident); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "drop schema if exists "+ident+" cascade")
		admin.Close()
	})
	if err := auth.NewPostgresStore(pool).EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}

	fixture := &phase2AcknowledgementFixture{
		pool: pool, admin: admin, masterKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x37}, 32)),
		primaryID: uuid.New(), nodeID: uuid.New(), tokenID: uuid.New(), attemptID: uuid.New(),
		revisionID: uuid.New(), jobID: uuid.New(), contentHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		appliedAt: time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC),
	}
	ownerID := uuid.New()
	if _, err := pool.Exec(ctx, `insert into users (id, username, password_hash, role, active) values ($1, 'pne8-owner', 'scrypt$16384$8$1$hash$hash', 'owner', true)`, ownerID); err != nil {
		t.Fatal(err)
	}
	primaryNodeID := uuid.New()
	if _, err := pool.Exec(ctx, `insert into installation_identity (id, installation_id, node_id, role, leadership_generation, latest_known_generation) values ('default', $1, $2, 'primary', 7, 7)`, fixture.primaryID, primaryNodeID); err != nil {
		t.Fatal(err)
	}
	keyStore := cluster.NewStore(fixture.masterKey)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	material, err := keyStore.LoadOrCreate(ctx, tx, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	fixture.keyID = material.ID
	material.Destroy()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update installation_identity set cluster_key_id = $1 where id = 'default'`, fixture.keyID); err != nil {
		t.Fatal(err)
	}
	fixture.credentialID = uuid.NewString()
	secret := bytes.Repeat([]byte{0x42}, 32)
	digest := sha256.Sum256(append([]byte("proxycore/node-credential/"), secret...))
	fixture.credentialHash = hex.EncodeToString(digest[:])
	if _, err := pool.Exec(ctx, `insert into node_credentials (id, node_id, credential_hash, hash_version) values ($1, $2, $3, $4)`, fixture.credentialID, fixture.nodeID, fixture.credentialHash, "sha256-node-v1"); err != nil {
		t.Fatal(err)
	}
	createdAt := fixture.appliedAt.Add(-time.Hour)
	nodeInstallationID := uuid.New()
	if _, err := pool.Exec(ctx, `insert into enrollment_tokens (id, token_selector, token_hash, hash_version, created_by_user_id, created_at, expires_at, consumed_at, consumed_by_attempt_id) values ($1, $2, 'token-hash', $3, $4, $5, $6, $7, $8)`, fixture.tokenID, strings.Repeat("ab", 16), EnrollmentTokenHashVersion, ownerID, createdAt, createdAt.Add(time.Hour), createdAt.Add(time.Minute), fixture.attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into enrollment_attempts (id, state, primary_url, expected_primary_id, local_node_ip, cluster_key_id, ephemeral_private_key_wrapped, created_at, updated_at) values ($1, 'committed', 'https://primary.example', $2, '127.0.0.1', $3, 'wrapped', $4, $4)`, fixture.attemptID, fixture.primaryID, fixture.keyID, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into enrollment_grants (attempt_id, token_id, installation_id, node_id, primary_id, primary_generation, node_ephemeral_public_key, sealed_bootstrap_payload, payload_hash, created_at, expires_at) values ($1, $2, $3, $4, $5, 7, 'ephemeral', 'sealed', 'payload-hash', $6, $7)`, fixture.attemptID, fixture.tokenID, nodeInstallationID, fixture.nodeID, fixture.primaryID, createdAt, createdAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into enrolled_nodes (node_id, installation_id, primary_id, credential_id, created_by_attempt_id, enrolled_at) values ($1, $2, $3, $4, $5, $6)`, fixture.nodeID, nodeInstallationID, fixture.primaryID, fixture.credentialID, fixture.attemptID, createdAt); err != nil {
		t.Fatal(err)
	}
	desired := []byte(`{"settings":{}}`)
	desiredSum := sha256.Sum256(desired)
	if _, err := pool.Exec(ctx, `insert into config_revisions (id, revision_number, checksum, snapshot, source, source_primary_id, snapshot_content_hash, snapshot_version, replication_version, leadership_generation, applied_at) values ($1, 1, $2, $3, 'ordinary', $4, $5, 1, 1, 7, $6)`, fixture.revisionID, hex.EncodeToString(desiredSum[:]), desired, fixture.primaryID, fixture.contentHash, fixture.appliedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into apply_jobs (id, revision_id, target, status, correlation_id, source, source_primary_id, snapshot_content_hash, snapshot_version, replication_version, leadership_generation, finished_at) values ($1, $2, 'combined', 'applied', $3, 'ordinary', $4, $5, 1, 1, 7, $6)`, fixture.jobID, fixture.revisionID, uuid.NewString(), fixture.primaryID, fixture.contentHash, fixture.appliedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into applied_snapshots (id, source_primary_id, leadership_generation, snapshot_version, replication_version, content_hash, snapshot_body, revision_id, status, apply_job_id, applied_at) values ($1, $2, 7, 1, 1, $3, $4, $5, 'applied', $6, $7)`, uuid.New(), fixture.primaryID, fixture.contentHash, []byte(`{}`), fixture.revisionID, fixture.jobID, fixture.appliedAt); err != nil {
		t.Fatal(err)
	}
	fixture.store = NewPhase2StoreWithMasterKey(pool, fixture.masterKey)
	return fixture
}

func (f *phase2AcknowledgementFixture) ack() SnapshotAcknowledgement {
	return SnapshotAcknowledgement{
		NodeID: f.nodeID.String(), ContentHash: f.contentHash, SnapshotVersion: 1,
		ReplicationVersion: 1, RevisionID: f.revisionID.String(), LeadershipGeneration: 7,
		AppliedAt: f.appliedAt, ReceivedAt: f.appliedAt.Add(time.Minute),
	}
}

func TestPhase2AcknowledgmentPostgresHappyPathAndDefensiveCopy(t *testing.T) {
	fixture := newPhase2AcknowledgementFixture(t)
	if err := fixture.store.RecordSnapshotAcknowledgement(context.Background(), fixture.ack()); err != nil {
		t.Fatalf("record acknowledgement: %v", err)
	}
	got, err := fixture.store.GetSnapshotAcknowledgement(context.Background(), fixture.nodeID.String(), fixture.contentHash)
	if err != nil {
		t.Fatalf("get acknowledgement: %v", err)
	}
	if got.RevisionID != fixture.revisionID.String() || got.ContentHash != fixture.contentHash || !got.AppliedAt.Equal(fixture.appliedAt) {
		t.Fatalf("ack=%+v", got)
	}
	got.ContentHash = strings.Repeat("f", 64)
	got.RevisionID = uuid.NewString()
	again, err := fixture.store.GetSnapshotAcknowledgement(context.Background(), fixture.nodeID.String(), fixture.contentHash)
	if err != nil || again.ContentHash != fixture.contentHash || again.RevisionID != fixture.revisionID.String() {
		t.Fatalf("defensive copy row=%+v err=%v", again, err)
	}
}

func TestPhase2AcknowledgmentPostgresRejectsRevocationAndMismatches(t *testing.T) {
	cases := []struct {
		name string
		edit func(*phase2AcknowledgementFixture)
		want error
	}{
		{name: "credential revoked", edit: func(f *phase2AcknowledgementFixture) {
			_, _ = f.pool.Exec(context.Background(), `update node_credentials set revoked_at = now() where id = $1`, f.credentialID)
		}, want: ErrSnapshotAcknowledgementRevoked},
		{name: "token revoked", edit: func(f *phase2AcknowledgementFixture) {
			_, _ = f.pool.Exec(context.Background(), `update enrollment_tokens set revoked_at = now() where id = $1`, f.tokenID)
		}, want: ErrSnapshotAcknowledgementRevoked},
		{name: "mismatched node", edit: func(f *phase2AcknowledgementFixture) { f.nodeID = uuid.New() }, want: ErrSnapshotAcknowledgementDenied},
		{name: "mismatched generation", edit: func(f *phase2AcknowledgementFixture) {}, want: ErrSnapshotAcknowledgementDenied},
		{name: "mismatched cluster key", edit: func(f *phase2AcknowledgementFixture) {
			_, _ = f.pool.Exec(context.Background(), `update installation_identity set cluster_key_id = $1 where id = 'default'`, uuid.New())
		}, want: ErrSnapshotAcknowledgementDenied},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPhase2AcknowledgementFixture(t)
			test.edit(fixture)
			ack := fixture.ack()
			if test.name == "mismatched generation" {
				ack.LeadershipGeneration++
			}
			err := fixture.store.RecordSnapshotAcknowledgement(context.Background(), ack)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want %v", err, test.want)
			}
		})
	}
}

func TestPhase2AcknowledgmentPostgresDatabaseFailure(t *testing.T) {
	fixture := newPhase2AcknowledgementFixture(t)
	fixture.pool.Close()
	if err := fixture.store.RecordSnapshotAcknowledgement(context.Background(), fixture.ack()); !errors.Is(err, ErrSnapshotAcknowledgementUnavailable) {
		t.Fatalf("error=%v want ErrSnapshotAcknowledgementUnavailable", err)
	}
}
